package argo

import (
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target"
)

// WS-7.1: a node-env workflow targeting Argo fails with the documented
// diagnostic; a container-env workflow proceeds.
func TestEnvGateRejectsNodeEnvironment(t *testing.T) {
	compileResult := compileFixturePlan(t, "linear-chain")
	// Rewrite the materialized environment to a Node dependency environment,
	// which the wedge cannot run on Kubernetes yet.
	for _, environment := range compileResult.Plan.GetEnvironments() {
		kind := spec.EnvironmentKindNode
		environment.Kind = &kind
		environment.Container = nil
	}

	_, err := New().Compile(target.CompileInput{
		Plan:     compileResult.Plan,
		PlanJSON: compileResult.CanonicalJSON,
		PlanHash: compileResult.PlanHash,
		TargetKind:   Kind,
		TargetConfig: argoConfigJSON(t, argoConfigValue),
	})
	if err == nil {
		t.Fatal("expected node-env compile to fail")
	}
	if !IsTargetCompatibilityError(err) {
		t.Fatalf("expected a TargetCompatibilityError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "double") || !strings.Contains(err.Error(), "node") {
		t.Fatalf("diagnostic should name the step and env kind, got: %v", err)
	}
}

func TestEnvGateRejectsContainerWithoutImage(t *testing.T) {
	compileResult := compileFixturePlan(t, "linear-chain")
	for _, environment := range compileResult.Plan.GetEnvironments() {
		environment.Container = nil // container kind, but no image
	}

	_, err := New().Compile(target.CompileInput{
		Plan:     compileResult.Plan,
		PlanJSON: compileResult.CanonicalJSON,
		PlanHash: compileResult.PlanHash,
		TargetKind:   Kind,
		TargetConfig: argoConfigJSON(t, argoConfigValue),
	})
	if err == nil || !IsTargetCompatibilityError(err) {
		t.Fatalf("expected a target-compatibility error for a container env with no image, got: %v", err)
	}
}

// WS-7.1 positive: a container-env workflow compiles.
func TestEnvGateAcceptsContainerEnvironment(t *testing.T) {
	if _, err := New().Compile(mustCompileInput(t, "linear-chain")); err != nil {
		t.Fatalf("container-env compile should succeed: %v", err)
	}
}

// The vendored CRD schema has no name patterns, so invalid names must be
// gated at compile time rather than surfacing at apply time.
func TestNameGateRejectsInvalidWorkflowTemplateName(t *testing.T) {
	input := mustCompileInput(t, "linear-chain")
	input.TargetConfig = argoConfigJSON(t, argoConfig{Namespace: "argo", ServiceAccountName: "argo", WorkflowTemplateName: "Linear_Chain"})

	_, err := New().Compile(input)
	if err == nil || !strings.Contains(err.Error(), "Linear_Chain") {
		t.Fatalf("expected a diagnostic naming the invalid template name, got: %v", err)
	}
}

func TestNameGateRejectsStepIDArgoCannotName(t *testing.T) {
	input := mustCompileInput(t, "linear-chain")
	for _, node := range input.Plan.GetGraph().GetNodes() {
		if node.GetId() == "double" {
			invalid := "double_step"
			node.Id = &invalid
		}
	}

	_, err := New().Compile(input)
	if err == nil || !strings.Contains(err.Error(), "double_step") {
		t.Fatalf("expected a diagnostic naming the invalid step id, got: %v", err)
	}
}

// A node id at the length bound produces a step-<id> template name at exactly
// Argo's workflow-field limit; one character longer overflows it.
func TestNameGateStepIDLengthBoundary(t *testing.T) {
	atLimit := planIndex{stepOrder: []string{strings.Repeat("a", maxStepNodeIDLength)}}
	if err := validateNames("wf", atLimit); err != nil {
		t.Fatalf("a %d-char step id should pass: %v", maxStepNodeIDLength, err)
	}

	overLimit := planIndex{stepOrder: []string{strings.Repeat("a", maxStepNodeIDLength+1)}}
	err := validateNames("wf", overLimit)
	if err == nil || !strings.Contains(err.Error(), "characters") {
		t.Fatalf("a %d-char step id should fail the length gate, got: %v", maxStepNodeIDLength+1, err)
	}
}

// The wf-system- prefix is reserved for compiler-generated system tasks, so a
// user step id may not use it.
func TestNameGateRejectsReservedSystemPrefix(t *testing.T) {
	input := mustCompileInput(t, "linear-chain")
	for _, node := range input.Plan.GetGraph().GetNodes() {
		if node.GetId() == "double" {
			reserved := "wf-system-double"
			node.Id = &reserved
		}
	}

	_, err := New().Compile(input)
	if err == nil || !strings.Contains(err.Error(), "wf-system-") {
		t.Fatalf("expected a diagnostic rejecting the reserved prefix, got: %v", err)
	}
}

func mustCompileInput(t *testing.T, caseName string) target.CompileInput {
	t.Helper()
	compileResult := compileFixturePlan(t, caseName)
	return target.CompileInput{
		Plan:         compileResult.Plan,
		PlanJSON:     compileResult.CanonicalJSON,
		PlanHash:     compileResult.PlanHash,
		TargetKind:   Kind,
		TargetConfig: argoConfigJSON(t, argoConfigValue),
	}
}
