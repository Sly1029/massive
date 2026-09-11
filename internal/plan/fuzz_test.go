package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/Sly1029/massive/internal/spec"
)

// Raw-byte mutation reaches schema parsing, semantic validation, compilation,
// and canonical plan verification. Real fixtures keep the initial corpus deep.
func FuzzWorkflowParsing(f *testing.F) {
	for _, name := range []string{"passthrough", "linear-chain", "diamond", "python-linear", "exhaustive-decision", "finite-map"} {
		body, err := os.ReadFile("../../conformance/fixtures/specs/" + name + "/workflow-spec.json")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 128*1024 {
			t.Skip()
		}
		parsed, err := spec.Parse(body)
		if err != nil {
			return
		}
		compiled, err := Compile(parsed, body)
		if err != nil {
			t.Fatalf("accepted spec cannot compile: %v", err)
		}
		if _, err := VerifyCanonicalJSON(compiled.CanonicalJSON, compiled.PlanHash); err != nil {
			t.Fatalf("compiled plan cannot verify: %v", err)
		}
		again, err := Compile(parsed, body)
		if err != nil || !slices.Equal(compiled.CanonicalJSON, again.CanonicalJSON) {
			t.Fatalf("compilation is not deterministic: %v", err)
		}
	})
}

// Structure-aware generation does not rely on byte mutations getting through
// a schema and content hash. Every generated DAG has an independent ordering oracle.
func FuzzGraphShapes(f *testing.F) {
	f.Add([]byte{4, 0, 1, 0, 2, 1, 3, 2, 3})
	f.Add([]byte{1})
	f.Add([]byte{16, 1, 2, 5, 8, 13, 15})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 2048 {
			t.Skip()
		}
		count := int(data[0]%32) + 1
		graph := spec.Graph{}
		for i := 0; i < count; i++ {
			graph.Nodes = append(graph.Nodes, spec.GraphNode{ID: fmt.Sprintf("n%02d", i), Kind: spec.NodeKindStep})
		}
		seen := map[[2]int]bool{}
		for i := 1; i+1 < len(data); i += 2 {
			a, b := int(data[i])%count, int(data[i+1])%count
			if a > b {
				a, b = b, a
			}
			if a == b || seen[[2]int{a, b}] {
				continue
			}
			seen[[2]int{a, b}] = true
			graph.Edges = append(graph.Edges, spec.GraphEdge{From: graph.Nodes[a].ID, To: graph.Nodes[b].ID})
		}
		schedule, err := BuildSchedule(graph)
		if err != nil {
			t.Fatal(err)
		}
		if len(schedule.NodeOrder) != count {
			t.Fatal("nodes were lost")
		}
		positions, depths := map[string]int{}, map[string]int{}
		for i, id := range schedule.NodeOrder {
			if _, exists := positions[id]; exists {
				t.Fatal("duplicate scheduled node")
			}
			positions[id] = i
		}
		// Edges always point from a smaller generated index to a larger index.
		for i := 0; i < count; i++ {
			id := graph.Nodes[i].ID
			for _, edge := range graph.Edges {
				if edge.To == id {
					depths[id] = max(depths[id], depths[edge.From]+1)
				}
				if positions[edge.From] >= positions[edge.To] {
					t.Fatal("dependency scheduled after consumer")
				}
			}
		}
		for _, depth := range schedule.Depths {
			if depth.Depth != depths[depth.NodeID] {
				t.Fatal("incorrect longest-path depth")
			}
		}
		slices.Reverse(graph.Nodes)
		slices.Reverse(graph.Edges)
		reordered, err := BuildSchedule(graph)
		if err != nil || !reflect.DeepEqual(schedule, reordered) {
			t.Fatalf("input order changed schedule: %v", err)
		}
		graph.Edges = append(graph.Edges, spec.GraphEdge{From: graph.Nodes[0].ID, To: graph.Nodes[0].ID})
		if _, err := BuildSchedule(graph); err == nil {
			t.Fatal("cycle was accepted")
		}
	})
}

// Generate exhaustive decisions with varying branch counts and unequal branch
// depths, then deliberately break closure. This exercises semantic parsing well
// beyond the seed fixture while preserving a correct spec hash.
func FuzzDecisionGraphs(f *testing.F) {
	fixture, err := os.ReadFile("../../conformance/fixtures/specs/exhaustive-decision/workflow-spec.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte{2, 1, 3})
	f.Add([]byte{12, 4, 0, 2, 7})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 256 {
			t.Skip()
		}
		workflow, err := spec.Parse(fixture)
		if err != nil {
			t.Fatal(err)
		}
		var route, choose, task spec.GraphNode
		nodes := []spec.GraphNode{}
		for _, n := range workflow.Graph.Nodes {
			switch n.ID {
			case "route":
				route = n
			case "choose":
				choose = n
			case "accept":
				task = n
			case "reject":
			default:
				nodes = append(nodes, n)
			}
		}
		route.Cases, choose.SelectInputs = nil, nil
		edges := []spec.GraphEdge{{From: "__start", To: "classify"}, {From: "classify", To: "route"}, {From: "choose", To: "__end"}}
		count := int(data[0]%12) + 2
		for i := 0; i < count; i++ {
			tag := fmt.Sprintf("case%02d", i)
			schemaRef := fmt.Sprintf("sha256:%064x", i+100)
			workflow.Schemas[schemaRef] = json.RawMessage(fmt.Sprintf(`{"type":"object","required":["kind","value"],"properties":{"kind":{"const":%q},"value":{"type":"integer"}},"additionalProperties":false}`, tag))
			route.Cases = append(route.Cases, spec.DecisionCase{Tag: tag, Schema: schemaRef})
			previous := "route"
			depth := int(data[i%len(data)]%8) + 1
			for j := 0; j < depth; j++ {
				n := task
				n.ID = fmt.Sprintf("branch%02d-step%02d", i, j)
				edge := spec.GraphEdge{From: previous, To: n.ID}
				if j == 0 {
					n.InputSchema = schemaRef
					edge.Case = tag
				} else {
					n.InputSchema = task.OutputSchema
				}
				nodes = append(nodes, n)
				edges = append(edges, edge)
				previous = n.ID
			}
			choose.SelectInputs = append(choose.SelectInputs, spec.SelectInput{Case: tag, Source: previous})
			edges = append(edges, spec.GraphEdge{From: previous, To: "choose"})
		}
		workflow.Graph.Nodes = append(nodes, route, choose)
		workflow.Graph.Edges = edges
		body := fuzzSpecJSON(t, workflow)
		parsed, err := spec.Parse(body)
		if err != nil {
			t.Fatalf("valid generated decision rejected: %v", err)
		}
		compiled, err := Compile(parsed, body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyCanonicalJSON(compiled.CanonicalJSON, compiled.PlanHash); err != nil {
			t.Fatal(err)
		}
		// Selecting another mutually exclusive branch must be rejected even with
		// valid types, valid references, and a freshly computed content hash.
		last := &workflow.Graph.Nodes[len(workflow.Graph.Nodes)-1]
		last.SelectInputs[0].Source = last.SelectInputs[1].Source
		if _, err := spec.Parse(fuzzSpecJSON(t, workflow)); err == nil {
			t.Fatal("cross-branch select accepted")
		}
	})
}

func fuzzSpecJSON(t *testing.T, workflow *spec.WorkflowSpec) []byte {
	t.Helper()
	body, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	workflow.SpecHash, err = spec.RecomputedSpecHash(body)
	if err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
