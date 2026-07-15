// Package argo is the first target.Backend: it lowers a compiled WorkflowPlan
// into an Argo Workflows deploy bundle (a WorkflowTemplate plus the plan and a
// bundle manifest). It implements only the v0 executable wedge from
// docs/spec/argo-backend.md — plan, materialize tree, validate structure,
// validate minimal invariants, emit bundle. Presets, plugins, user patches,
// system mediation, and field-level provenance are deferred (WS-10).
//
// The backend is plan-driven: it reads everything it needs from the
// target.CompileInput (the typed plan, its canonical bytes, its hash, and the
// resolved target request) and never reaches back into the WorkflowSpec.
package argo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target"
	"sigs.k8s.io/yaml"
)

// Kind is the target id this backend compiles.
const Kind = "argo"

// Backend compiles the Argo target.
type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Kind() string { return Kind }

// argoConfig is the Argo backend's own target configuration, decoded from the
// neutral CompileInput.TargetConfig. The neutral target package never sees these
// Kubernetes-specific fields.
type argoConfig struct {
	Namespace            string `json:"namespace"`
	ServiceAccountName   string `json:"serviceAccountName"`
	WorkflowTemplateName string `json:"workflowTemplateName"`
}

// decodeConfig decodes and validates the Argo target config with strict
// unknown-field rejection. The spec-level JSON-schema validation already
// enforces the argo target shape at parse; this is the backend owning its own
// config so the neutral package carries none of it.
func decodeConfig(configJSON []byte) (argoConfig, error) {
	var config argoConfig
	decoder := json.NewDecoder(bytes.NewReader(configJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return argoConfig{}, fmt.Errorf("argo target: decode target config: %w", err)
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return argoConfig{}, fmt.Errorf("argo target: target config has trailing JSON content")
	}
	if config.Namespace == "" {
		return argoConfig{}, fmt.Errorf("argo target: config requires a namespace")
	}
	if config.ServiceAccountName == "" {
		return argoConfig{}, fmt.Errorf("argo target: config requires a serviceAccountName")
	}
	return config, nil
}

// compileContext is the argo-internal view of a compile request, so the
// template builders never depend on the target package.
type compileContext struct {
	plan     *planpb.WorkflowPlan
	planHash string
	config   argoConfig
}

// TargetCompatibilityError is the documented diagnostic for a workflow feature
// the Argo v0 wedge cannot represent — currently, any step whose environment is
// not the container escape hatch.
type TargetCompatibilityError struct {
	NodeID  string
	EnvKind string
	Message string
}

func (e *TargetCompatibilityError) Error() string {
	return fmt.Sprintf("argo target: step %q uses environment kind %q: %s", e.NodeID, e.EnvKind, e.Message)
}

// Compile runs the v0 wedge: env gate, materialize the WorkflowTemplate tree,
// validate its structure against the Argo CRD schema, enforce the minimal
// invariants, and emit the bundle. Any hard failure returns a clear diagnostic
// and no bundle.
func (b *Backend) Compile(input target.CompileInput) (*target.Bundle, error) {
	if input.Plan == nil {
		return nil, fmt.Errorf("argo target: compile input has no plan")
	}
	if input.TargetKind != Kind {
		return nil, fmt.Errorf("argo target: compile input target kind is %q, expected %q", input.TargetKind, Kind)
	}
	config, err := decodeConfig(input.TargetConfig)
	if err != nil {
		return nil, err
	}

	index := buildPlanIndex(input.Plan)
	ctx := compileContext{plan: input.Plan, planHash: input.PlanHash, config: config}

	if err := gateEnvironments(index); err != nil {
		return nil, err
	}

	tmpl, err := buildWorkflowTemplate(index, ctx)
	if err != nil {
		return nil, fmt.Errorf("argo target: materialize workflow template: %w", err)
	}

	templateJSON, err := marshalCanonicalTemplateJSON(tmpl)
	if err != nil {
		return nil, err
	}
	if err := validateStructure(templateJSON); err != nil {
		return nil, err
	}

	validations := runInvariants(tmpl, index, input.PlanHash, input.Plan)
	if failure := firstFailedValidation(validations); failure != nil {
		return nil, fmt.Errorf("argo target: invariant %q failed: %s", failure.Name, failure.Diagnostic)
	}
	// Structure validation passed to reach here; record it alongside the invariants.
	validations = append([]target.Validation{{Name: "structure-validation", Passed: true}}, validations...)

	templateYAML, err := yaml.JSONToYAML(templateJSON)
	if err != nil {
		return nil, fmt.Errorf("argo target: render workflow template YAML: %w", err)
	}

	artifacts := []target.Artifact{
		{
			Path:        "workflow-template.yaml",
			Bytes:       templateYAML,
			ContentType: "application/yaml",
			Role:        "workflow-template",
		},
		{
			Path:        "massive-plan.json",
			Bytes:       input.PlanJSON,
			ContentType: "application/json",
			Role:        "plan",
		},
	}

	bundle, err := target.BuildBundle(input, artifacts, validations)
	if err != nil {
		return nil, fmt.Errorf("argo target: build bundle: %w", err)
	}
	return bundle, nil
}

// gateEnvironments enforces WS-7.1: every step's environment must be the
// container escape hatch with an image. Node (and any non-container) kinds are
// rejected with a target-compatibility diagnostic naming the step and kind.
func gateEnvironments(index planIndex) error {
	for _, nodeID := range index.stepOrder {
		node := index.nodesByID[nodeID]
		contract, ok := index.contractsByRef[node.GetContractRef()]
		if !ok {
			return fmt.Errorf("argo target: step %q references unknown contract %q", nodeID, node.GetContractRef())
		}
		environment, ok := index.environmentsByRef[contract.GetEnvironmentRef()]
		if !ok {
			return fmt.Errorf("argo target: step %q references unknown environment %q", nodeID, contract.GetEnvironmentRef())
		}
		kind := environment.GetKind()
		if kind != spec.EnvironmentKindContainer {
			return &TargetCompatibilityError{
				NodeID:  nodeID,
				EnvKind: kind,
				Message: "the argo v0 wedge supports env.container(...) only; env.node(...) needs Kubernetes dependency materialization (WS-9)",
			}
		}
		if environment.GetContainer().GetImage() == "" {
			return &TargetCompatibilityError{
				NodeID:  nodeID,
				EnvKind: kind,
				Message: "container environment has no image; the runtime image is required for the argo target",
			}
		}
	}
	return nil
}

func firstFailedValidation(validations []target.Validation) *target.Validation {
	for i := range validations {
		if !validations[i].Passed {
			return &validations[i]
		}
	}
	return nil
}

// IsTargetCompatibilityError reports whether err is (or wraps) a
// TargetCompatibilityError, for callers that want to map it to an exit code.
func IsTargetCompatibilityError(err error) bool {
	var compat *TargetCompatibilityError
	return errors.As(err, &compat)
}

func sortStrings(values []string) {
	sort.Slice(values, func(i, j int) bool { return canonical.LessUTF16(values[i], values[j]) })
}
