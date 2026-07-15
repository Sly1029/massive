// Package schema exposes the frozen conformance contracts to Go consumers,
// so binaries embed them instead of depending on repo-relative paths at
// runtime.
package schema

import _ "embed"

// WorkflowSpecSchemaJSON is the frozen WorkflowSpec JSON Schema
// (draft 2020-12) that frontend SDK emissions must validate against.
//
//go:embed workflow-spec.schema.json
var WorkflowSpecSchemaJSON []byte

// ArgoWorkflowsCRDVersion is the pinned Argo Workflows release whose generated
// JSON Schema is vendored below. See argo-workflows-schema.README.md for the
// exact upstream source and update procedure.
const ArgoWorkflowsCRDVersion = "v3.7.16"

// ArgoWorkflowsCRDSchemaJSON is the vendored Argo Workflows JSON Schema
// (draft 2020-12, self-contained internal $refs) used to validate generated
// WorkflowTemplate manifests offline. The Argo backend validates against the
// io.argoproj.workflow.v1alpha1.WorkflowTemplate definition inside it.
//
//go:embed argo-workflows-v3.7.16.schema.json
var ArgoWorkflowsCRDSchemaJSON []byte
