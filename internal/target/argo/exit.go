package argo

import (
	"encoding/json"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
)

// ExitStatus maps the exit handler's {{workflow.status}} and
// {{workflow.failures}} onto the run outcome's status and failed node. Argo
// reports a stopped workflow as Failed, so it never yields "cancelled". The
// failed node is the plan node whose pod failed first.
func ExitStatus(p *planpb.WorkflowPlan, status, failures string) (string, *string, error) {
	switch status {
	case "Succeeded":
		return "succeeded", nil, nil
	case "Failed", "Error":
	default:
		return "", nil, fmt.Errorf("argo exit handler: unexpected workflow status %q", status)
	}
	// Argo encodes no failures as null.
	var failed []struct {
		TemplateName string `json:"templateName"`
		FinishedAt   string `json:"finishedAt"`
	}
	if err := json.Unmarshal([]byte(failures), &failed); err != nil {
		return "", nil, fmt.Errorf("argo exit handler: decode workflow failures: %w", err)
	}
	nodes := templateNodes(p)
	var node *string
	first := ""
	for _, failure := range failed {
		id, ok := nodes[failure.TemplateName]
		// RFC 3339 timestamps in one zone order lexically.
		if ok && (node == nil || failure.FinishedAt < first) {
			node, first = &id, failure.FinishedAt
		}
	}
	return "failed", node, nil
}

// templateNodes names the plan node behind each pod template, using the
// names workflowTemplate generates.
func templateNodes(p *planpb.WorkflowPlan) map[string]string {
	nodes := map[string]string{}
	for _, node := range p.GetGraph().GetNodes() {
		switch node.GetKind() {
		case "step", "decision", "select":
			nodes[argoFieldName("step-"+node.GetId())] = node.GetId()
		case "map":
			for _, prefix := range []string{"map-expand-", "map-item-", "map-collect-"} {
				nodes[argoFieldName(prefix+node.GetId())] = node.GetId()
			}
		}
	}
	return nodes
}
