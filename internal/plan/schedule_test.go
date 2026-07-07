package plan

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/Sly1029/massive/internal/spec"
)

func TestBuildScheduleStableOrders(t *testing.T) {
	tests := []struct {
		name      string
		graph     spec.Graph
		nodeOrder []string
		stepIDs   []string
		depths    []NodeDepth
	}{
		{
			name:      "linear",
			graph:     graphFromEdges([]string{"double", "increment", "label"}, edges("__start", "double", "double", "increment", "increment", "label", "label", "__end")),
			nodeOrder: []string{"__start", "double", "increment", "label", "__end"},
			stepIDs:   []string{"double", "increment", "label"},
			depths: []NodeDepth{
				{NodeID: "__start", Depth: 0},
				{NodeID: "double", Depth: 1},
				{NodeID: "increment", Depth: 2},
				{NodeID: "label", Depth: 3},
				{NodeID: "__end", Depth: 4},
			},
		},
		{
			name:      "diamond",
			graph:     graphFromEdges([]string{"split", "left", "right", "merge"}, edges("__start", "split", "split", "left", "split", "right", "left", "merge", "right", "merge", "merge", "__end")),
			nodeOrder: []string{"__start", "split", "left", "right", "merge", "__end"},
			stepIDs:   []string{"split", "left", "right", "merge"},
			depths: []NodeDepth{
				{NodeID: "__start", Depth: 0},
				{NodeID: "split", Depth: 1},
				{NodeID: "left", Depth: 2},
				{NodeID: "right", Depth: 2},
				{NodeID: "merge", Depth: 3},
				{NodeID: "__end", Depth: 4},
			},
		},
		{
			name:      "uneven branch-depth fan-in",
			graph:     graphFromEdges([]string{"split", "short", "long", "long-tail", "merge"}, edges("__start", "split", "split", "short", "short", "merge", "split", "long", "long", "long-tail", "long-tail", "merge", "merge", "__end")),
			nodeOrder: []string{"__start", "split", "long", "long-tail", "short", "merge", "__end"},
			stepIDs:   []string{"split", "long", "long-tail", "short", "merge"},
			depths: []NodeDepth{
				{NodeID: "__start", Depth: 0},
				{NodeID: "split", Depth: 1},
				{NodeID: "long", Depth: 2},
				{NodeID: "long-tail", Depth: 3},
				{NodeID: "short", Depth: 2},
				{NodeID: "merge", Depth: 4},
				{NodeID: "__end", Depth: 5},
			},
		},
		{
			name:      "multi-stage fan-in",
			graph:     graphFromEdges([]string{"split", "a1", "a2", "b1", "b2", "merge-a", "merge-b", "merge-final"}, edges("__start", "split", "split", "a1", "split", "a2", "split", "b1", "split", "b2", "a1", "merge-a", "a2", "merge-a", "b1", "merge-b", "b2", "merge-b", "merge-a", "merge-final", "merge-b", "merge-final", "merge-final", "__end")),
			nodeOrder: []string{"__start", "split", "a1", "a2", "b1", "b2", "merge-a", "merge-b", "merge-final", "__end"},
			stepIDs:   []string{"split", "a1", "a2", "b1", "b2", "merge-a", "merge-b", "merge-final"},
			depths: []NodeDepth{
				{NodeID: "__start", Depth: 0},
				{NodeID: "split", Depth: 1},
				{NodeID: "a1", Depth: 2},
				{NodeID: "a2", Depth: 2},
				{NodeID: "b1", Depth: 2},
				{NodeID: "b2", Depth: 2},
				{NodeID: "merge-a", Depth: 3},
				{NodeID: "merge-b", Depth: 3},
				{NodeID: "merge-final", Depth: 4},
				{NodeID: "__end", Depth: 5},
			},
		},
		{
			name:      "100-way split/merge",
			graph:     batchMerge100Graph(),
			nodeOrder: batchMerge100NodeOrder(),
			stepIDs:   batchMerge100StepIDs(),
			depths:    batchMerge100Depths(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schedule, err := BuildSchedule(test.graph)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(schedule.NodeOrder, test.nodeOrder) {
				t.Fatalf("node order mismatch\nactual:   %#v\nexpected: %#v", schedule.NodeOrder, test.nodeOrder)
			}
			if !reflect.DeepEqual(schedule.StepIDs, test.stepIDs) {
				t.Fatalf("step IDs mismatch\nactual:   %#v\nexpected: %#v", schedule.StepIDs, test.stepIDs)
			}
			if !reflect.DeepEqual(schedule.Depths, test.depths) {
				t.Fatalf("depths mismatch\nactual:   %#v\nexpected: %#v", schedule.Depths, test.depths)
			}
		})
	}
}

func graphFromEdges(stepIDs []string, graphEdges []spec.GraphEdge) spec.Graph {
	nodes := []spec.GraphNode{
		{ID: "__start", Kind: spec.NodeKindStart},
		{ID: "__end", Kind: spec.NodeKindEnd},
	}
	for _, stepID := range stepIDs {
		nodes = append(nodes, spec.GraphNode{ID: stepID, Kind: spec.NodeKindStep})
	}
	return spec.Graph{Start: "__start", End: "__end", Nodes: nodes, Edges: graphEdges}
}

func edges(ids ...string) []spec.GraphEdge {
	if len(ids)%2 != 0 {
		panic("edges requires from/to pairs")
	}

	graphEdges := make([]spec.GraphEdge, 0, len(ids)/2)
	for index := 0; index < len(ids); index += 2 {
		graphEdges = append(graphEdges, spec.GraphEdge{From: ids[index], To: ids[index+1]})
	}
	return graphEdges
}

func batchMerge100Graph() spec.Graph {
	stepIDs := batchMerge100StepIDs()
	graphEdges := []spec.GraphEdge{{From: "__start", To: "split"}}
	for index := 0; index < 100; index++ {
		workerID := fmt.Sprintf("worker-%03d", index)
		graphEdges = append(graphEdges, spec.GraphEdge{From: "split", To: workerID})
		graphEdges = append(graphEdges, spec.GraphEdge{From: workerID, To: "merge"})
	}
	graphEdges = append(graphEdges, spec.GraphEdge{From: "merge", To: "__end"})
	return graphFromEdges(stepIDs, graphEdges)
}

func batchMerge100NodeOrder() []string {
	order := []string{"__start", "split"}
	for index := 0; index < 100; index++ {
		order = append(order, fmt.Sprintf("worker-%03d", index))
	}
	return append(order, "merge", "__end")
}

func batchMerge100StepIDs() []string {
	stepIDs := []string{"split"}
	for index := 0; index < 100; index++ {
		stepIDs = append(stepIDs, fmt.Sprintf("worker-%03d", index))
	}
	return append(stepIDs, "merge")
}

func batchMerge100Depths() []NodeDepth {
	depths := []NodeDepth{
		{NodeID: "__start", Depth: 0},
		{NodeID: "split", Depth: 1},
	}
	for index := 0; index < 100; index++ {
		depths = append(depths, NodeDepth{NodeID: fmt.Sprintf("worker-%03d", index), Depth: 2})
	}
	depths = append(depths, NodeDepth{NodeID: "merge", Depth: 3}, NodeDepth{NodeID: "__end", Depth: 4})
	return depths
}
