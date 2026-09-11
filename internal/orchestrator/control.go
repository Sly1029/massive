package orchestrator

import (
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
)

// ResolveControlValue validates a data-only decision or selected value without
// loading source packages or invoking user code. The target owns readiness and
// passes only the chosen select input; both runtimes share decision validation.
type ControlValue struct {
	Value     []byte
	CaseIndex *int
}

func ResolveControlValue(p *planpb.WorkflowPlan, nodeID string, inputJSON []byte) (*ControlValue, error) {
	input, err := canonical.CanonicalizeJSON(inputJSON)
	if err != nil {
		return nil, err
	}
	schemas := make(map[string]string, len(p.GetSchemas()))
	for _, schema := range p.GetSchemas() {
		schemas[schema.GetHash()] = schema.GetCanonicalJson()
	}
	for _, node := range p.GetGraph().GetNodes() {
		if node.GetId() != nodeID {
			continue
		}
		switch node.GetKind() {
		case "decision":
			tag, err := decisionCaseForValue(node, input, schemas)
			if err != nil {
				return nil, err
			}
			for index, candidate := range node.GetCases() {
				if candidate.GetTag() == tag {
					return &ControlValue{Value: input, CaseIndex: &index}, nil
				}
			}
			return nil, fmt.Errorf("decision %q selected an unavailable case", nodeID)
		case "select":
			schema, exists := schemas[node.GetOutputSchema()]
			if !exists {
				return nil, fmt.Errorf("select %q output schema is unavailable", nodeID)
			}
			if err := validateJSONAgainstSchema(schema, input); err != nil {
				return nil, fmt.Errorf("select %q value does not satisfy its output schema", nodeID)
			}
		default:
			return nil, fmt.Errorf("node %q is not a decision or select", nodeID)
		}
		return &ControlValue{Value: input}, nil
	}
	return nil, fmt.Errorf("control node %q is unavailable in the plan", nodeID)
}
