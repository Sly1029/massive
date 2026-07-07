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
