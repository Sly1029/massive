package target_test

import (
	"errors"
	"os"
	"path/filepath"
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
	return target.CompileInput{
		Plan:     compileResult.Plan,
		PlanJSON: compileResult.CanonicalJSON,
		PlanHash: compileResult.PlanHash,
		Target:   spec.Target{Kind: "argo", Namespace: "argo", ServiceAccountName: "argo"},
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
