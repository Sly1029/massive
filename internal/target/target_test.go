package target_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target"
	"github.com/Sly1029/massive/internal/target/argo"
)

func diamondInput(t *testing.T) target.CompileInput {
	t.Helper()
	specPath := filepath.Join("..", "..", "conformance", "fixtures", "specs", "diamond", "workflow-spec.json")
	specData, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compileResult, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}
	var argoTarget spec.Target
	for _, request := range workflowSpec.Targets {
		if request.Kind == "argo" {
			argoTarget = request
		}
	}
	if argoTarget.Kind != "argo" {
		t.Fatal("diamond fixture does not declare an argo target")
	}
	return target.CompileInput{
		Plan:         compileResult.Plan,
		PlanJSON:     compileResult.CanonicalJSON,
		PlanHash:     compileResult.PlanHash,
		TargetKind:   argoTarget.Kind,
		TargetConfig: argoTarget.Config,
	}
}

func TestRegistryCompilesRegisteredBackend(t *testing.T) {
	registry := target.NewRegistry()
	registry.Register(argo.New())

	bundle, err := registry.Compile("argo", diamondInput(t))
	if err != nil {
		t.Fatalf("compile via registry: %v", err)
	}
	if bundle.Manifest.GetTarget() != "argo" {
		t.Fatalf("manifest target = %q, want argo", bundle.Manifest.GetTarget())
	}
}

// The registry rejects a compile input whose PlanJSON no longer matches its
// PlanHash/Plan, so a bundle can never describe a different plan than its own
// massive-plan.json.
func TestRegistryRejectsTamperedPlanJSON(t *testing.T) {
	registry := target.NewRegistry()
	registry.Register(argo.New())

	input := diamondInput(t)
	tampered := bytes.Replace(input.PlanJSON, []byte(`"diamond"`), []byte(`"diamon0"`), 1)
	if bytes.Equal(tampered, input.PlanJSON) {
		t.Fatal("tamper did not change the plan JSON")
	}
	input.PlanJSON = tampered

	_, err := registry.Compile("argo", input)
	if err == nil {
		t.Fatal("expected a plan-consistency rejection for tampered PlanJSON")
	}
	if !strings.Contains(err.Error(), "plan") {
		t.Fatalf("expected a plan-consistency diagnostic, got: %v", err)
	}
}

// The registry also rejects a mutated typed plan even when PlanJSON/PlanHash are
// left consistent — the typed plan (which a backend materializes YAML from) must
// match the PlanJSON emitted as massive-plan.json.
func TestRegistryRejectsMutatedTypedPlan(t *testing.T) {
	registry := target.NewRegistry()
	registry.Register(argo.New())

	input := diamondInput(t)
	mutated := false
	for _, node := range input.Plan.GetGraph().GetNodes() {
		if node.GetKind() == "step" {
			id := node.GetId() + "-tampered"
			node.Id = &id
			mutated = true
			break
		}
	}
	if !mutated {
		t.Fatal("no step node to mutate")
	}

	_, err := registry.Compile("argo", input)
	if err == nil {
		t.Fatal("expected a plan-consistency rejection for a mutated typed plan")
	}
	if !strings.Contains(err.Error(), "plan") {
		t.Fatalf("expected a plan-consistency diagnostic, got: %v", err)
	}
}

func TestRegistryUnknownKindListsSupported(t *testing.T) {
	registry := target.NewRegistry()
	registry.Register(argo.New())

	_, err := registry.Compile("temporal", diamondInput(t))
	var unknown *target.UnknownTargetError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected UnknownTargetError, got %T: %v", err, err)
	}
	if unknown.Kind != "temporal" {
		t.Fatalf("error kind = %q, want temporal", unknown.Kind)
	}
	if len(unknown.Supported) != 1 || unknown.Supported[0] != "argo" {
		t.Fatalf("supported kinds = %v, want [argo]", unknown.Supported)
	}
}

// WriteBundle emits every content artifact plus bundle-manifest.json, and the
// manifest records each content file with its real sha256.
func TestWriteBundleRoundTrip(t *testing.T) {
	registry := target.NewRegistry()
	registry.Register(argo.New())
	bundle, err := registry.Compile("argo", diamondInput(t))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := target.WriteBundle(dir, bundle); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	for _, artifact := range bundle.Artifacts {
		onDisk, err := os.ReadFile(filepath.Join(dir, artifact.Path))
		if err != nil {
			t.Fatalf("read %s: %v", artifact.Path, err)
		}
		if string(onDisk) != string(artifact.Bytes) {
			t.Fatalf("%s on disk differs from bundle", artifact.Path)
		}
	}

	manifestOnDisk, err := os.ReadFile(filepath.Join(dir, target.BundleManifestPath))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(manifestOnDisk) != string(bundle.ManifestJSON) {
		t.Fatal("manifest on disk differs from bundle")
	}

	recorded := map[string]string{}
	for _, file := range bundle.Manifest.GetFiles() {
		recorded[file.GetPath()] = file.GetArtifact().GetHash()
	}
	if len(recorded) != len(bundle.Artifacts) {
		t.Fatalf("manifest records %d files, bundle has %d artifacts", len(recorded), len(bundle.Artifacts))
	}
	for _, artifact := range bundle.Artifacts {
		want := canonical.DigestBytes(artifact.Bytes)
		if recorded[artifact.Path] != want {
			t.Fatalf("manifest hash for %s = %q, want %q", artifact.Path, recorded[artifact.Path], want)
		}
	}
}
