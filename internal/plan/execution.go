package plan

import (
	"bytes"
	"encoding/json"
	"fmt"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateExecution checks plan-level execution semantics at every reader,
// including callers passing a typed plan directly to a target. A valid hash
// does not prove that execution requirements are meaningful.
func ValidateExecution(p *planpb.WorkflowPlan) error {
	graph := p.GetGraph()
	if graph == nil {
		return fmt.Errorf("workflow plan is missing graph; rebuild the plan")
	}
	if graph.DeadlineSeconds != nil {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.WorkflowSpecSchemaJSON))
		if err != nil {
			return err
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("workflow-spec.schema.json", document); err != nil {
			return err
		}
		schema, err := compiler.Compile("workflow-spec.schema.json#/properties/workflow/properties/deadlineSeconds")
		if err != nil {
			return err
		}
		if err := schema.Validate(json.Number(fmt.Sprint(graph.GetDeadlineSeconds()))); err != nil {
			return fmt.Errorf("workflow plan graph.deadlineSeconds violates the workflow deadline schema; rebuild the plan: %w", err)
		}
	}
	return nil
}
