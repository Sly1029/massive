package argo

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/target"
)

// Minimal invariant names (WS-7.4). See docs/spec/argo-backend.md#invariants.
const (
	invariantDAGIntegrity   = "dag-integrity"
	invariantPlanProvenance = "plan-provenance"
	invariantIdentitySet    = "identity-set"
)

// runInvariants enforces the wedge's minimal invariant set in a fixed order.
func runInvariants(tmpl *workflowTemplate, index planIndex, planHash string, plan *planpb.WorkflowPlan) []target.Validation {
	return []target.Validation{
		checkDAGIntegrity(tmpl, index),
		checkPlanProvenance(tmpl, planHash, plan),
		checkIdentitySet(tmpl),
	}
}

// checkDAGIntegrity: every IR step node maps to a reachable Argo step template
// (one DAG task referencing a runnable template), and every IR step-to-step edge
// survives as a DAG dependency — no missing and no invented dependencies. The
// system finalize task is the one non-step task; it must exist, reference the
// finalize template, and depend on exactly the steps feeding the end node.
func checkDAGIntegrity(tmpl *workflowTemplate, index planIndex) target.Validation {
	dag := findDAGTemplate(tmpl)
	if dag == nil {
		return fail(invariantDAGIntegrity, fmt.Sprintf("entrypoint DAG template %q not found", entrypointTemplateName))
	}

	stepTemplates := make(map[string]bool)
	for _, t := range tmpl.Spec.Templates {
		if t.Container != nil {
			stepTemplates[t.Name] = true
		}
	}

	tasksByName := make(map[string]dagTask, len(dag.Tasks))
	for _, task := range dag.Tasks {
		tasksByName[task.Name] = task
	}

	// One task per step node plus the single system finalize task.
	if len(dag.Tasks) != len(index.stepOrder)+1 {
		return fail(invariantDAGIntegrity, fmt.Sprintf("DAG has %d tasks but the plan has %d step nodes plus one finalize task", len(dag.Tasks), len(index.stepOrder)))
	}

	for _, nodeID := range index.stepOrder {
		task, ok := tasksByName[nodeID]
		if !ok {
			return fail(invariantDAGIntegrity, fmt.Sprintf("step node %q has no DAG task", nodeID))
		}
		if !stepTemplates[task.Template] {
			return fail(invariantDAGIntegrity, fmt.Sprintf("DAG task %q references template %q, which is not a runnable step template", nodeID, task.Template))
		}
		if task.Template != stepTemplateName(nodeID) {
			return fail(invariantDAGIntegrity, fmt.Sprintf("DAG task %q references template %q, expected %q", nodeID, task.Template, stepTemplateName(nodeID)))
		}
		want := index.stepDependencies[nodeID]
		got := task.Dependencies
		if !equalStringSets(want, got) {
			return fail(invariantDAGIntegrity, fmt.Sprintf("step %q dependencies are %v, expected the plan edges %v", nodeID, sortedCopy(got), sortedCopy(want)))
		}
	}

	finalize, ok := tasksByName[finalizeTaskName]
	if !ok {
		return fail(invariantDAGIntegrity, fmt.Sprintf("finalize task %q is missing", finalizeTaskName))
	}
	if !stepTemplates[finalize.Template] || finalize.Template != finalizeTaskName {
		return fail(invariantDAGIntegrity, fmt.Sprintf("finalize task references template %q, expected the runnable %q template", finalize.Template, finalizeTaskName))
	}
	// finalize is a barrier over every step, so it can compose the manifest from
	// all node entries regardless of which steps feed the end node.
	if !equalStringSets(finalize.Dependencies, index.stepOrder) {
		return fail(invariantDAGIntegrity, fmt.Sprintf("finalize dependencies are %v, expected all step tasks %v", sortedCopy(finalize.Dependencies), sortedCopy(index.stepOrder)))
	}

	return pass(invariantDAGIntegrity)
}

// checkPlanProvenance: the compiled plan hash annotation exists on the template
// and matches the plan.
func checkPlanProvenance(tmpl *workflowTemplate, planHash string, plan *planpb.WorkflowPlan) target.Validation {
	annotation, ok := tmpl.Metadata.Annotations[PlanHashAnnotation]
	if !ok || annotation == "" {
		return fail(invariantPlanProvenance, fmt.Sprintf("template is missing the %q annotation", PlanHashAnnotation))
	}
	if annotation != planHash {
		return fail(invariantPlanProvenance, fmt.Sprintf("template annotation %q is %q, expected the plan hash %q", PlanHashAnnotation, annotation, planHash))
	}
	if planHash != plan.GetPlanHash() {
		return fail(invariantPlanProvenance, fmt.Sprintf("compile input plan hash %q does not match the plan's own hash %q", planHash, plan.GetPlanHash()))
	}
	return pass(invariantPlanProvenance)
}

// checkIdentitySet: every pod runs under a service account. The workflow-level
// serviceAccountName applies to all step pods, so it must be set.
func checkIdentitySet(tmpl *workflowTemplate) target.Validation {
	if strings.TrimSpace(tmpl.Spec.ServiceAccountName) == "" {
		return fail(invariantIdentitySet, "spec.serviceAccountName is empty; step pods have no bound identity")
	}
	return pass(invariantIdentitySet)
}

func findDAGTemplate(tmpl *workflowTemplate) *dagTemplate {
	for i := range tmpl.Spec.Templates {
		if tmpl.Spec.Templates[i].Name == tmpl.Spec.Entrypoint && tmpl.Spec.Templates[i].DAG != nil {
			return tmpl.Spec.Templates[i].DAG
		}
	}
	return nil
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa, sb := sortedCopy(a), sortedCopy(b)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func pass(name string) target.Validation { return target.Validation{Name: name, Passed: true} }

func fail(name, diagnostic string) target.Validation {
	return target.Validation{Name: name, Passed: false, Diagnostic: diagnostic}
}
