// Package schema exposes the frozen conformance contracts to Go consumers,
// so binaries embed them instead of depending on repo-relative paths at
// runtime.
//
//go:generate ../../scripts/generate-proto.sh
package schema

import _ "embed"

// WorkflowSpecSchemaJSON is the frozen WorkflowSpec JSON Schema
// (draft 2020-12) that frontend SDK emissions must validate against.
//
//go:embed workflow-spec.schema.json
var WorkflowSpecSchemaJSON []byte

// DeploymentSpecSchemaJSON is the frozen DeploymentSpec JSON Schema
// (draft 2020-12) for target-specific profile bindings.
//
//go:embed deployment-spec.schema.json
var DeploymentSpecSchemaJSON []byte

// DataArtifactManifestSchemaJSON is the frozen manifest-last publication
// contract for canonical JSON step outputs.
//
//go:embed data-artifact-manifest.schema.json
var DataArtifactManifestSchemaJSON []byte

// ArgoWorkflowsCRDVersion and ArgoWorkflowsCRDSchemaJSON pin the upstream Argo
// WorkflowTemplate schema used for offline target validation.
const ArgoWorkflowsCRDVersion = "v3.7.16"

//go:embed argo-workflows-v3.7.16.schema.json
var ArgoWorkflowsCRDSchemaJSON []byte
