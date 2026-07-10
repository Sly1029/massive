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
		Target:   argoTarget,
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
		Target:   argoTarget,
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

func mustCompileInput(t *testing.T, caseName string) target.CompileInput {
	t.Helper()
	compileResult := compileFixturePlan(t, caseName)
	return target.CompileInput{
		Plan:     compileResult.Plan,
		PlanJSON: compileResult.CanonicalJSON,
		PlanHash: compileResult.PlanHash,
		Target:   argoTarget,
	}
}
