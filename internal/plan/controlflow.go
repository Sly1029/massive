package plan

import (
	"encoding/json"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/spec"
)

// ValidateControlFlow applies the same exhaustive-branch and activation-lineage
// rules to compiled plans as to authoring specs. Hash validity alone is not
// semantic validity for callers that supply their own canonical plan.
func ValidateControlFlow(p *planpb.WorkflowPlan) error {
	g := p.GetGraph()
	graph := spec.Graph{IRVersion: g.GetIrVersion(), Start: g.GetStartNode(), End: g.GetEndNode()}
	for _, n := range g.GetNodes() {
		node := spec.GraphNode{ID: n.GetId(), Kind: n.GetKind(), InputSchema: n.GetInputSchema(), OutputSchema: n.GetOutputSchema(), Selector: n.GetSelector(), DecisionRef: n.GetDecisionRef(), SymbolRef: n.GetSymbolRef(), ContractRef: n.GetContractRef(), MergeInputs: n.GetMergeInputs(), ItemInputSchema: n.GetItemInputSchema(), ItemOutputSchema: n.GetItemOutputSchema(), MaxConcurrency: n.GetMaxConcurrency()}
		for _, c := range n.GetCases() {
			node.Cases = append(node.Cases, spec.DecisionCase{Tag: c.GetTag(), Schema: c.GetSchema()})
		}
		for _, i := range n.GetSelectInputs() {
			node.SelectInputs = append(node.SelectInputs, spec.SelectInput{Case: i.GetCase(), Source: i.GetSource()})
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	for _, e := range g.GetEdges() {
		graph.Edges = append(graph.Edges, spec.GraphEdge{From: e.GetFrom(), To: e.GetTo(), Case: e.GetCase()})
	}
	schemas := make(map[string]json.RawMessage, len(p.GetSchemas()))
	for _, s := range p.GetSchemas() {
		schemas[s.GetHash()] = json.RawMessage(s.GetCanonicalJson())
	}
	return spec.ValidateControlFlow(graph, schemas)
}
