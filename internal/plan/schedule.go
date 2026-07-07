package plan

import (
	"fmt"
	"slices"
	"sort"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/spec"
)

type Schedule struct {
	NodeOrder []string
	StepIDs   []string
	Depths    []NodeDepth
}

type NodeDepth struct {
	NodeID string
	Depth  int
}

func BuildSchedule(graph spec.Graph) (Schedule, error) {
	nodeByID := make(map[string]spec.GraphNode, len(graph.Nodes))
	indegree := make(map[string]int, len(graph.Nodes))
	adjacency := make(map[string][]string, len(graph.Nodes))

	for _, node := range graph.Nodes {
		nodeByID[node.ID] = node
		indegree[node.ID] = 0
	}
	for _, edge := range graph.Edges {
		if _, exists := nodeByID[edge.From]; !exists {
			return Schedule{}, fmt.Errorf("schedule graph: edge source %q does not exist", edge.From)
		}
		if _, exists := nodeByID[edge.To]; !exists {
			return Schedule{}, fmt.Errorf("schedule graph: edge target %q does not exist", edge.To)
		}
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		indegree[edge.To]++
	}
	for nodeID := range adjacency {
		sort.Slice(adjacency[nodeID], func(i, j int) bool { return canonical.LessUTF16(adjacency[nodeID][i], adjacency[nodeID][j]) })
	}

	ready := make([]string, 0, len(graph.Nodes))
	for nodeID, degree := range indegree {
		if degree == 0 {
			ready = append(ready, nodeID)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return canonical.LessUTF16(ready[i], ready[j]) })

	depthByID := make(map[string]int, len(graph.Nodes))
	nodeOrder := make([]string, 0, len(graph.Nodes))
	stepIDs := make([]string, 0, len(graph.Nodes))

	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		nodeOrder = append(nodeOrder, current)
		if nodeByID[current].Kind == spec.NodeKindStep {
			stepIDs = append(stepIDs, current)
		}

		for _, next := range adjacency[current] {
			if depthByID[next] < depthByID[current]+1 {
				depthByID[next] = depthByID[current] + 1
			}
			indegree[next]--
			if indegree[next] != 0 {
				continue
			}
			// Sorted insert keeps the ready queue in UTF-16 order without
			// re-sorting the whole queue on every pop.
			position, _ := slices.BinarySearchFunc(ready, next, compareUTF16)
			ready = slices.Insert(ready, position, next)
		}
	}

	if len(nodeOrder) != len(graph.Nodes) {
		return Schedule{}, fmt.Errorf("schedule graph: graph contains a cycle")
	}

	depths := make([]NodeDepth, 0, len(nodeOrder))
	for _, nodeID := range nodeOrder {
		depths = append(depths, NodeDepth{NodeID: nodeID, Depth: depthByID[nodeID]})
	}

	return Schedule{NodeOrder: nodeOrder, StepIDs: stepIDs, Depths: depths}, nil
}

func compareUTF16(left, right string) int {
	if canonical.LessUTF16(left, right) {
		return -1
	}
	if canonical.LessUTF16(right, left) {
		return 1
	}
	return 0
}
