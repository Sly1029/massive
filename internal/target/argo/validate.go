package argo

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The vendored Argo schema's own $id; internal $refs resolve against it, so the
// WorkflowTemplate definition is addressed as a fragment of this URI.
const (
	argoSchemaID        = "https://raw.githubusercontent.com/argoproj/argo-workflows/HEAD/api/jsonschema/schema.json"
	workflowTemplateRef = argoSchemaID + "#/definitions/io.argoproj.workflow.v1alpha1.WorkflowTemplate"
)

var (
	argoSchemaOnce     sync.Once
	argoSchemaCompiled *jsonschema.Schema
	argoSchemaErr      error
)

// validateStructure validates the generated WorkflowTemplate JSON against the
// vendored Argo Workflows CRD schema (WS-7.3), fully offline. A schema violation
// is returned as an error carrying the validation detail.
func validateStructure(templateJSON []byte) error {
	schema, err := compileArgoSchema()
	if err != nil {
		return fmt.Errorf("argo target: load Argo CRD schema: %w", err)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(templateJSON))
	if err != nil {
		return fmt.Errorf("argo target: decode generated template for validation: %w", err)
	}

	if err := schema.Validate(instance); err != nil {
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			return fmt.Errorf("argo target: generated WorkflowTemplate failed Argo %s schema validation: %s", schemacontract.ArgoWorkflowsCRDVersion, validation)
		}
		return fmt.Errorf("argo target: validate generated WorkflowTemplate: %w", err)
	}
	return nil
}

func compileArgoSchema() (*jsonschema.Schema, error) {
	argoSchemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.ArgoWorkflowsCRDSchemaJSON))
		if err != nil {
			argoSchemaErr = fmt.Errorf("decode vendored Argo schema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(argoSchemaID, document); err != nil {
			argoSchemaErr = fmt.Errorf("register vendored Argo schema: %w", err)
			return
		}
		argoSchemaCompiled, argoSchemaErr = compiler.Compile(workflowTemplateRef)
	})
	return argoSchemaCompiled, argoSchemaErr
}
