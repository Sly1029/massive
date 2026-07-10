package argo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target"
)

// argoTarget is the target request the wedge tests compile against. linear-chain
// declares no argo target in its fixture, so tests supply one; diamond declares
// an equivalent one.
var argoTarget = spec.Target{Kind: "argo", Namespace: "argo", ServiceAccountName: "argo"}

func compileFixtureBundle(t *testing.T, caseName string, targetRequest spec.Target) *target.Bundle {
	t.Helper()
	compileResult := compileFixturePlan(t, caseName)
	bundle, err := New().Compile(target.CompileInput{
		Plan:     compileResult.Plan,
		PlanJSON: compileResult.CanonicalJSON,
		PlanHash: compileResult.PlanHash,
		Target:   targetRequest,
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
	bundle := compileFixtureBundle(t, "diamond", argoTarget)

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

	// The step container must be the runtime image, not a placeholder.
	for _, tmplate := range tmpl.Spec.Templates {
		if tmplate.Container == nil {
			continue
		}
		if tmplate.Container.Image != "ghcr.io/massive-dev/typescript-runner:v0" {
			t.Fatalf("step %q image = %q, want the container-env image", tmplate.Name, tmplate.Container.Image)
		}
		if len(tmplate.Container.Command) == 0 || tmplate.Container.Command[0] != stepRunnerCommand {
			t.Fatalf("step %q command = %v, want the step runner", tmplate.Name, tmplate.Container.Command)
		}
		if tmplate.Inputs == nil || len(tmplate.Inputs.Artifacts) != 1 {
			t.Fatalf("step %q must carry exactly one descriptor input artifact", tmplate.Name)
		}
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
