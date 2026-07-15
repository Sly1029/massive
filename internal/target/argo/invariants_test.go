package argo

import (
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/target"
)

func diamondTemplate(t *testing.T) (*workflowTemplate, planIndex, string) {
	t.Helper()
	compileResult := compileFixturePlan(t, "diamond")
	index := buildPlanIndex(compileResult.Plan)
	tmpl, err := buildWorkflowTemplate(index, compileContext{
		plan:     compileResult.Plan,
		planHash: compileResult.PlanHash,
		config:   argoConfigValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tmpl, index, compileResult.PlanHash
}

func TestDAGIntegrityInvariant(t *testing.T) {
	tmpl, index, _ := diamondTemplate(t)

	if got := checkDAGIntegrity(tmpl, index); !got.Passed {
		t.Fatalf("well-formed DAG should pass: %s", got.Diagnostic)
	}

	// Break a dependency: drop split from merge's upstreams.
	dag := findDAGTemplate(tmpl)
	for i := range dag.Tasks {
		if dag.Tasks[i].Name == "merge" {
			dag.Tasks[i].Dependencies = []string{"left"} // missing "right"
		}
	}
	if got := checkDAGIntegrity(tmpl, index); got.Passed {
		t.Fatal("DAG missing an edge should fail dag-integrity")
	}
}

func TestDAGIntegrityInvariantCatchesMissingTemplate(t *testing.T) {
	tmpl, index, _ := diamondTemplate(t)
	// Remove the step-left template so its task references a non-runnable template.
	kept := tmpl.Spec.Templates[:0]
	for _, template := range tmpl.Spec.Templates {
		if template.Name == stepTemplateName("left") {
			continue
		}
		kept = append(kept, template)
	}
	tmpl.Spec.Templates = kept
	if got := checkDAGIntegrity(tmpl, index); got.Passed {
		t.Fatal("a task pointing at a removed step template should fail dag-integrity")
	}
}

func TestPlanProvenanceInvariant(t *testing.T) {
	tmpl, _, planHash := diamondTemplate(t)
	compileResult := compileFixturePlan(t, "diamond")

	if got := checkPlanProvenance(tmpl, planHash, compileResult.Plan); !got.Passed {
		t.Fatalf("matching plan hash should pass: %s", got.Diagnostic)
	}

	tmpl.Metadata.Annotations = map[string]string{}
	if got := checkPlanProvenance(tmpl, planHash, compileResult.Plan); got.Passed {
		t.Fatal("missing plan-hash annotation should fail plan-provenance")
	}
}

func TestPlanProvenanceInvariantCatchesMismatch(t *testing.T) {
	tmpl, _, planHash := diamondTemplate(t)
	compileResult := compileFixturePlan(t, "diamond")
	tmpl.Metadata.Annotations[PlanHashAnnotation] = "sha256:" + strings.Repeat("0", 64)
	if got := checkPlanProvenance(tmpl, planHash, compileResult.Plan); got.Passed {
		t.Fatal("a plan-hash annotation that disagrees with the plan should fail")
	}
}

func TestIdentitySetInvariant(t *testing.T) {
	tmpl, _, _ := diamondTemplate(t)

	if got := checkIdentitySet(tmpl); !got.Passed {
		t.Fatalf("template with a service account should pass: %s", got.Diagnostic)
	}

	tmpl.Spec.ServiceAccountName = ""
	if got := checkIdentitySet(tmpl); got.Passed {
		t.Fatal("template without a service account should fail identity-set")
	}
}

// A target config without a service account is rejected by the backend's own
// config validation before any bundle is built. (The identity-set invariant is
// the defense-in-depth check on the materialized template; see
// TestIdentitySetInvariant.)
func TestCompileFailsWhenServiceAccountMissing(t *testing.T) {
	compileResult := compileFixturePlan(t, "diamond")
	_, err := New().Compile(target.CompileInput{
		Plan:         compileResult.Plan,
		PlanJSON:     compileResult.CanonicalJSON,
		PlanHash:     compileResult.PlanHash,
		TargetKind:   Kind,
		TargetConfig: argoConfigJSON(t, argoConfig{Namespace: "argo"}),
	})
	if err == nil || !strings.Contains(err.Error(), "serviceAccountName") {
		t.Fatalf("expected a serviceAccountName config failure, got: %v", err)
	}
}
