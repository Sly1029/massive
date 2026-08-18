// Package deployment validates the target-specific artifact that binds a
// compiled plan to one deployment profile.
package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Spec struct {
	Kind           string  `json:"kind"`
	SchemaVersion  uint32  `json:"schemaVersion"`
	Encoding       string  `json:"encoding"`
	DeploymentHash string  `json:"deploymentHash"`
	PlanHash       string  `json:"planHash"`
	Profile        Profile `json:"profile"`
}

type Profile struct {
	Name                 string `json:"name"`
	ArtifactStoreBinding string `json:"artifactStoreBinding"`
	Target               Target `json:"target"`
}

type Target struct {
	Kind                 string `json:"kind"`
	Namespace            string `json:"namespace,omitempty"`
	ServiceAccountName   string `json:"serviceAccountName,omitempty"`
	WorkflowTemplateName string `json:"workflowTemplateName,omitempty"`
}

type Diagnostic struct {
	Path    string
	Ref     string
	Message string
}

type DiagnosticsError struct {
	Diagnostics []Diagnostic
}

func (e *DiagnosticsError) Error() string {
	if len(e.Diagnostics) == 0 {
		return "deployment spec diagnostics"
	}
	return e.Diagnostics[0].String()
}

func (d Diagnostic) String() string {
	if d.Ref == "" {
		return fmt.Sprintf("%s: %s", d.Path, d.Message)
	}
	return fmt.Sprintf("%s (%s): %s", d.Path, d.Ref, d.Message)
}

func ReadFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read deployment spec %q: %w", path, err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Spec, error) {
	if err := validateSchema(data); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var parsed Spec
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode deployment spec: %w", err)
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return nil, fmt.Errorf("decode deployment spec: trailing JSON content")
	}

	hash, err := RecomputedHash(data)
	if err != nil {
		return nil, err
	}
	if parsed.DeploymentHash != hash {
		return nil, &DiagnosticsError{Diagnostics: []Diagnostic{{
			Path: "$.deploymentHash", Ref: parsed.DeploymentHash,
			Message: fmt.Sprintf("does not match canonical deployment content; expected %s", hash),
		}}}
	}
	return &parsed, nil
}

func RecomputedHash(data []byte) (string, error) {
	hash, err := canonical.DigestJSONWithRootMemberExcluded(data, "deploymentHash")
	if err != nil {
		return "", fmt.Errorf("compute deployment hash: %w", err)
	}
	return hash, nil
}

func validateSchema(data []byte) error {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode deployment spec for schema validation: %w", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.DeploymentSpecSchemaJSON))
	if err != nil {
		return fmt.Errorf("decode embedded deployment spec schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("deployment-spec.schema.json", document); err != nil {
		return fmt.Errorf("register deployment spec schema: %w", err)
	}
	schema, err := compiler.Compile("deployment-spec.schema.json")
	if err != nil {
		return fmt.Errorf("compile deployment spec schema: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			return &DiagnosticsError{Diagnostics: schemaDiagnostics(validation)}
		}
		return fmt.Errorf("validate deployment spec schema: %w", err)
	}
	return nil
}

func schemaDiagnostics(validation *jsonschema.ValidationError) []Diagnostic {
	basic := validation.BasicOutput()
	var diagnostics []Diagnostic
	collectSchemaDiagnostics(basic, &diagnostics)
	if len(diagnostics) == 0 {
		return []Diagnostic{{Path: "$", Ref: "deployment-spec.schema.json", Message: validation.Error()}}
	}
	return diagnostics
}

func collectSchemaDiagnostics(unit *jsonschema.OutputUnit, diagnostics *[]Diagnostic) {
	if unit == nil {
		return
	}
	if unit.Error != nil {
		path := unit.InstanceLocation
		if path == "" {
			path = "$"
		}
		*diagnostics = append(*diagnostics, Diagnostic{Path: path, Ref: unit.KeywordLocation, Message: unit.Error.String()})
	}
	for index := range unit.Errors {
		collectSchemaDiagnostics(&unit.Errors[index], diagnostics)
	}
}
