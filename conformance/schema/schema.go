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

// StepInvocationDescriptorSchemaJSON is the frozen cross-language runner
// descriptor transport contract.
//
//go:embed step-invocation-descriptor.schema.json
var StepInvocationDescriptorSchemaJSON []byte

// DataArtifactManifestSchemaJSON is the frozen manifest-last publication
// contract for canonical JSON step outputs.
//
//go:embed data-artifact-manifest.schema.json
var DataArtifactManifestSchemaJSON []byte

// ArgoWorkflowsCRDVersion and ArgoWorkflowsCRDSchemaJSON pin the upstream Argo
// WorkflowTemplate schema used for offline target validation.
const ArgoWorkflowsCRDVersion = "v3.7.16"

// ArgoWorkflowsSchemaID is the pinned schema's own $id. References into it,
// including the Kubernetes definitions placement uses, resolve against it.
const ArgoWorkflowsSchemaID = "https://raw.githubusercontent.com/argoproj/argo-workflows/HEAD/api/jsonschema/schema.json"

//go:embed argo-workflows-v3.7.16.schema.json
var ArgoWorkflowsCRDSchemaJSON []byte

// RunManifestSchemaJSON is the current durable run journal transport.
//
//go:embed run-manifest.schema.json
var RunManifestSchemaJSON []byte

// EnvironmentProbeSchemaJSON is the Python environment probe's report of an
// interpreter and its installed distributions.
//
//go:embed environment-probe.schema.json
var EnvironmentProbeSchemaJSON []byte

// RunOutcomeSchemaJSON is the input every exit hook receives once its run
// settles. It is generated from the Python SDK's RunOutcome model.
//
//go:embed run-outcome.schema.json
var RunOutcomeSchemaJSON []byte

// ValueReferenceSchemaJSON is the reference that replaces a large canonical
// JSON value in a target scheduler parameter.
//
//go:embed value-reference.schema.json
var ValueReferenceSchemaJSON []byte
