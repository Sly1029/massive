package argo

import (
	"fmt"
	"strconv"

	"github.com/Sly1029/massive/conformance/schema/planpb"
)

func argoCaseExpression(decision *planpb.GraphNode, tag string, names map[string]string) (string, error) {
	for index, candidate := range decision.GetCases() {
		if candidate.GetTag() == tag {
			return "tasks[" + strconv.Quote(names[decision.GetId()]) + "].outputs.parameters.selection == " + strconv.Quote(strconv.Itoa(index)), nil
		}
	}
	return "", fmt.Errorf("argo target: decision %q has no case %q", decision.GetId(), tag)
}

func argoSelectExpression(node *planpb.GraphNode, nodes []*planpb.GraphNode, names map[string]string) (string, error) {
	var decision *planpb.GraphNode
	for _, candidate := range nodes {
		if candidate.GetId() == node.GetDecisionRef() {
			decision = candidate
			break
		}
	}
	if decision == nil || len(node.GetSelectInputs()) == 0 {
		return "", fmt.Errorf("argo target: select %q requires a decision and exhaustive inputs", node.GetId())
	}
	// Every alternative stays inside one expression tag: the controller must not
	// substitute output references belonging to skipped branches eagerly.
	expression := "nil"
	for i := len(node.GetSelectInputs()) - 1; i >= 0; i-- {
		input := node.GetSelectInputs()[i]
		condition, err := argoCaseExpression(decision, input.GetCase(), names)
		if err != nil {
			return "", err
		}
		expression = condition + " ? tasks[" + strconv.Quote(names[input.GetSource()]) + "].outputs.parameters.result : (" + expression + ")"
	}
	return "{{=" + expression + "}}", nil
}

func controlEnvironmentSource(id string, nodes []*planpb.GraphNode, inbound map[string][]string) *planpb.GraphNode {
	for _, node := range nodes {
		if node.GetId() == id && (node.GetKind() == "step" || node.GetKind() == "map") {
			return node
		}
	}
	sources := append([]string(nil), inbound[id]...)
	// Inbound edges in the canonical plan already have deterministic order.
	for _, source := range sources {
		if node := controlEnvironmentSource(source, nodes, inbound); node != nil {
			return node
		}
	}
	return nil
}
