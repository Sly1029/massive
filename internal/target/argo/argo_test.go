package argo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target"
)

// argoConfigValue is the argo target config the wedge tests compile against.
var argoConfigValue = argoConfig{Namespace: "argo", ServiceAccountName: "argo"}

func compileFixtureBundle(t *testing.T, caseName string) *target.Bundle {
	t.Helper()
	compileResult := compileFixturePlan(t, caseName)
	bundle, err := New().Compile(target.CompileInput{
		Plan:         compileResult.Plan,
		PlanJSON:     compileResult.CanonicalJSON,
		PlanHash:     compileResult.PlanHash,
		TargetKind:   Kind,
		TargetConfig: argoConfigJSON(t, argoConfigValue),
	})
	if err != nil {
		t.Fatalf("compile %s argo bundle: %v", caseName, err)
	}
	return bundle
}

func compileFixturePlan(t *testing.T, caseName string) *plan.CompileResult {
	t.Helper()
	specPath := filepath.Join("..", "..", "..", "conformance", "fixtures", "specs", caseName, "workflow-spec.json")
	specData, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatalf("parse %s spec: %v", caseName, err)
	}
	compileResult, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		t.Fatalf("compile %s plan: %v", caseName, err)
	}
	return compileResult
}

func TestCompileDiamondProducesValidTemplate(t *testing.T) {
	bundle := compileFixtureBundle(t, "diamond")

	tmpl := parseBundleTemplate(t, bundle)
	if tmpl.Kind != "WorkflowTemplate" {
		t.Fatalf("kind = %q, want WorkflowTemplate", tmpl.Kind)
	}
	if tmpl.Metadata.Name != "diamond" {
		t.Fatalf("metadata.name = %q, want diamond", tmpl.Metadata.Name)
	}
	if got := tmpl.Metadata.Annotations[PlanHashAnnotation]; got != bundle.Manifest.GetPlanHash() {
		t.Fatalf("plan-hash annotation = %q, want %q", got, bundle.Manifest.GetPlanHash())
	}
	if tmpl.Spec.ServiceAccountName != "argo" {
		t.Fatalf("serviceAccountName = %q, want argo", tmpl.Spec.ServiceAccountName)
	}

	dag := findDAGTemplate(tmpl)
	if dag == nil {
		t.Fatal("no entrypoint DAG template")
	}
	deps := map[string][]string{}
	for _, task := range dag.Tasks {
		deps[task.Name] = sortedCopy(task.Dependencies)
	}
	wantDeps := map[string][]string{
		"split": {},
		"left":  {"split"},
		"right": {"split"},
		"merge": {"left", "right"},
	}
	for name, want := range wantDeps {
		if !equalStringSets(deps[name], want) {
			t.Fatalf("task %q dependencies = %v, want %v", name, deps[name], want)
		}
	}

	// Each step container runs the step driver in the runtime image: it carries
	// no embedded descriptor (descriptors are built at run time), its args name
	// the node/run/plan, and its env references the datastore/project workflow
	// parameters. The wf-system-finalize task runs the finalize driver instead
	// and is checked separately.
	sawFinalize := false
	for _, tmplate := range tmpl.Spec.Templates {
		if tmplate.Container == nil {
			continue
		}
		if tmplate.Name == finalizeTaskName {
			sawFinalize = true
			wantCommand := []string{stepDriverCommand, stepDriverFinalizeSubcommand}
			if len(tmplate.Container.Command) != 2 || !equalStringSets(tmplate.Container.Command, wantCommand) {
				t.Fatalf("finalize command = %v, want %v", tmplate.Container.Command, wantCommand)
			}
			args := strings.Join(tmplate.Container.Args, " ")
			if strings.Contains(args, "--node") || !strings.Contains(args, "--run-id "+argoRunIDVariable) || !strings.Contains(args, "--plan-hash") {
				t.Fatalf("finalize args = %v, want run-id/plan-hash and no --node", tmplate.Container.Args)
			}
			continue
		}
		if tmplate.Container.Image != "ghcr.io/massive-dev/typescript-runner:v0" {
			t.Fatalf("step %q image = %q, want the container-env image", tmplate.Name, tmplate.Container.Image)
		}
		wantCommand := []string{stepDriverCommand, stepDriverSubcommand}
		if !equalStringSets(tmplate.Container.Command, wantCommand) || len(tmplate.Container.Command) != 2 {
			t.Fatalf("step %q command = %v, want %v", tmplate.Name, tmplate.Container.Command, wantCommand)
		}
		args := strings.Join(tmplate.Container.Args, " ")
		if !strings.Contains(args, "--node") || !strings.Contains(args, "--run-id "+argoRunIDVariable) || !strings.Contains(args, "--plan-hash") {
			t.Fatalf("step %q args = %v, want node/run-id/plan-hash", tmplate.Name, tmplate.Container.Args)
		}
		envNames := map[string]string{}
		for _, env := range tmplate.Container.Env {
			envNames[env.Name] = env.Value
		}
		for _, name := range []string{envDatastoreKind, envDatastorePath, envProjectID} {
			if _, ok := envNames[name]; !ok {
				t.Fatalf("step %q env is missing %q", tmplate.Name, name)
			}
		}
	}
	if !sawFinalize {
		t.Fatal("template has no wf-system-finalize task")
	}

	// The finalize task is a barrier over every step, not just the end feeders.
	var finalizeDeps []string
	found := false
	for _, task := range dag.Tasks {
		if task.Name == finalizeTaskName {
			finalizeDeps = task.Dependencies
			found = true
		}
	}
	if !found {
		t.Fatal("DAG has no wf-system-finalize task")
	}
	if !equalStringSets(finalizeDeps, []string{"split", "left", "right", "merge"}) {
		t.Fatalf("finalize dependencies = %v, want all steps", finalizeDeps)
	}
}

// A step need not lie on a path to the end node — the spec validator only
// requires reachability from start. The finalize barrier must depend on every
// step (not just end feeders) so it never fires while a side branch still runs.
func TestFinalizeBarriersOnAllStepsIncludingSideBranch(t *testing.T) {
	specData, err := os.ReadFile(filepath.Join("testdata", "side-branch", "workflow-spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The spec (main reaches end; side is reachable from start but does not) must
	// be accepted — this documents that such graphs are valid.
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatalf("side-branch spec should be valid (a step need not reach end): %v", err)
	}
	compileResult, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		t.Fatalf("compile side-branch plan: %v", err)
	}

	index := buildPlanIndex(compileResult.Plan)
	tmpl, err := buildWorkflowTemplate(index, compileContext{
		plan:     compileResult.Plan,
		planHash: compileResult.PlanHash,
		config:   argoConfigValue,
	})
	if err != nil {
		t.Fatalf("build side-branch template: %v", err)
	}

	// "side" feeds no end path, so end-feeder-only wiring would omit it. The
	// barrier must include both steps.
	dag := findDAGTemplate(tmpl)
	var finalizeDeps []string
	for _, task := range dag.Tasks {
		if task.Name == finalizeTaskName {
			finalizeDeps = task.Dependencies
		}
	}
	if !equalStringSets(finalizeDeps, []string{"main", "side"}) {
		t.Fatalf("finalize dependencies = %v, want all steps [main side]", finalizeDeps)
	}
	// The invariant agrees.
	if got := checkDAGIntegrity(tmpl, index); !got.Passed {
		t.Fatalf("dag-integrity should pass for a side-branch graph: %s", got.Diagnostic)
	}
}

func parseBundleTemplate(t *testing.T, bundle *target.Bundle) *workflowTemplate {
	t.Helper()
	for _, artifact := range bundle.Artifacts {
		if artifact.Path != "workflow-template.yaml" {
			continue
		}
		asJSON, err := yamlToJSON(artifact.Bytes)
		if err != nil {
			t.Fatalf("convert template YAML to JSON: %v", err)
		}
		tmpl, err := decodeTemplate(asJSON)
		if err != nil {
			t.Fatalf("decode template: %v", err)
		}
		return tmpl
	}
	t.Fatal("bundle has no workflow-template.yaml")
	return nil
}
