package argo

import (
	"encoding/json"
	"fmt"

	"github.com/Sly1029/massive/internal/canonical"
)

func marshalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal JSON value: %w", err)
	}
	return data, nil
}

// marshalCanonicalTemplateJSON renders the WorkflowTemplate as canonical JSON
// (sorted keys, deterministic) — the form validated against the Argo schema and
// converted to the emitted YAML.
func marshalCanonicalTemplateJSON(tmpl *workflowTemplate) ([]byte, error) {
	raw, err := marshalJSON(tmpl)
	if err != nil {
		return nil, err
	}
	canonicalJSON, err := canonical.CanonicalizeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workflow template JSON: %w", err)
	}
	return canonicalJSON, nil
}
