package controlplane

import (
	"errors"
	"fmt"

	"github.com/Sly1029/massive/internal/deployment"
	"github.com/Sly1029/massive/internal/materialization"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target/argo"
)

// ArgoInputs is the portable compilation boundary. A transport supplies these
// bytes, not a FrontendResult, checkout path, Python process, or output directory.
// SourceArchives is keyed by semantic source-package identity.
type ArgoInputs struct {
	WorkflowSpec        []byte
	MaterializationSpec []byte
	SourceArchives      map[string][]byte
}

type ArgoCompilation struct {
	Plan           *plan.CompileResult
	Deployment     *deployment.Spec
	DeploymentJSON []byte
	Bundle         *argo.Bundle
}

// PrepareArgo is the client-side packaging operation. Compilation here derives
// content-addressed environment references; the receiving compiler independently
// validates/recompiles the spec instead of trusting a client-produced plan.
func PrepareArgo(frontend *FrontendResult) (*ArgoInputs, error) {
	if frontend == nil {
		return nil, errors.New("frontend result is required")
	}
	workflowSpec, err := spec.Parse(frontend.Canonical)
	if err != nil {
		return nil, err
	}
	compiled, err := plan.Compile(workflowSpec, frontend.Canonical)
	if err != nil {
		return nil, fmt.Errorf("compile workflow plan: %w", err)
	}
	archives := make(map[string][]byte, len(workflowSpec.SourcePackages))
	for _, sourcePackage := range workflowSpec.SourcePackages {
		files := make([]orchestrator.SourcePackageFile, 0, len(sourcePackage.Files))
		for _, file := range sourcePackage.Files {
			files = append(files, orchestrator.SourcePackageFile{Path: file.Path, Hash: file.Hash})
		}
		archive, err := orchestrator.BuildSourceArchive(frontend.PackageRoot, files)
		if err != nil {
			return nil, fmt.Errorf("package Argo source %q: %w", sourcePackage.PackageID, err)
		}
		archives[sourcePackage.PackageHash] = archive
	}
	inputs, err := materialization.ForPlan(compiled.Plan, archives)
	if err != nil {
		return nil, err
	}
	inputJSON, err := materialization.MarshalCanonical(inputs)
	if err != nil {
		return nil, err
	}
	return &ArgoInputs{
		WorkflowSpec:        append([]byte(nil), frontend.Canonical...),
		MaterializationSpec: inputJSON, SourceArchives: archives,
	}, nil
}

// CompileArgo is shared by local build and a future server. It is entirely
// data-in/data-out: no author-code evaluation, dependency installation, registry
// access, filesystem output, or Kubernetes publication occurs here.
func CompileArgo(inputs ArgoInputs, profile deployment.Profile) (*ArgoCompilation, error) {
	workflowSpec, err := spec.Parse(inputs.WorkflowSpec)
	if err != nil {
		return nil, fmt.Errorf("parse workflow spec: %w", err)
	}
	compiled, err := plan.Compile(workflowSpec, inputs.WorkflowSpec)
	if err != nil {
		return nil, fmt.Errorf("compile workflow plan: %w", err)
	}
	manifest, err := materialization.Resolve(compiled.Plan, inputs.MaterializationSpec, inputs.SourceArchives)
	if err != nil {
		return nil, err
	}
	binding, bindingJSON, err := deployment.New(compiled.PlanHash, profile, manifest.GetManifestHash())
	if err != nil {
		return nil, fmt.Errorf("construct deployment spec: %w", err)
	}
	bundle, err := argo.Compile(compiled.CanonicalJSON, binding, argo.RuntimeAssets{
		SourceArchives: inputs.SourceArchives, MaterializationSpec: inputs.MaterializationSpec,
	})
	if err != nil {
		return nil, err
	}
	return &ArgoCompilation{Plan: compiled, Deployment: binding, DeploymentJSON: bindingJSON, Bundle: bundle}, nil
}
