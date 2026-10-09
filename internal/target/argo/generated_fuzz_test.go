package argo

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/graphgen"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/sourceidentity"
	"github.com/Sly1029/massive/internal/spec"
)

// FuzzGeneratedPlanLowering lowers every generated plan, including scoped,
// mixed-case, and 128-character node IDs, and checks the DAG against the
// plan it came from. With collide set, one step is renamed to another node's
// projected task name, which the target must reject.
func FuzzGeneratedPlanLowering(f *testing.F) {
	f.Add([]byte{0, 3, 2, 1, 5, 2, 1, 1, 4, 1, 2}, false)
	f.Add([]byte{1, 2, 5, 3, 0, 1, 2, 1, 6, 1, 3, 3, 1, 2, 2, 0, 1}, false)
	f.Add([]byte{2, 1, 6, 30, 0, 3, 4, 2, 2, 1, 1, 5, 0, 1, 1, 3, 2, 0}, true)
	// A fan-in step: each merge source is its own task parameter.
	f.Add([]byte{1, 0, 7, 5, 2, 2, 1, 7, 5, 0, 0, 1, 5, 4, 5, 2}, false)
	f.Fuzz(func(t *testing.T, data []byte, collide bool) {
		if len(data) > 512 {
			t.Skip()
		}
		workflow, err := graphgen.Generate(data)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := spec.Parse(workflow.JSON)
		if err != nil {
			t.Fatalf("generated spec rejected: %v", err)
		}
		compiled, err := plan.Compile(parsed, workflow.JSON)
		if err != nil {
			t.Fatal(err)
		}
		packageHash, err := sourceidentity.Digest([]sourceidentity.File{{Path: "fixture.txt", Hash: canonical.DigestBytes([]byte("source fixture\n"))}})
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range compiled.Plan.SourcePackages {
			source.PackageHash = pointer(packageHash)
		}
		if collide {
			collideTaskNames(compiled.Plan)
		}
		planJSON, _ := rehashPlan(t, compiled.Plan)
		bundle, err := Compile(planJSON, deploymentForPlan(t, planJSON), runtimeAssetsForPlan(t, compiled.Plan))
		collision := generatedNameCollision(compiled.Plan)
		if err != nil {
			if collision != "" && strings.Contains(err.Error(), "collides") {
				return
			}
			if collectsItemFailures(compiled.Plan) && strings.Contains(err.Error(), "collects item failures") {
				return
			}
			t.Fatalf("valid plan was not lowered: %v", err)
		}
		if collision != "" {
			t.Fatalf("lowering accepted colliding generated name %q", collision)
		}
		var template map[string]any
		if err := json.Unmarshal(fileByPath(t, bundle, "workflow-template.json").Bytes, &template); err != nil {
			t.Fatal(err)
		}
		checkLoweredDAG(t, compiled.Plan, template["spec"].(map[string]any)["templates"].([]any))
	})
}

// collideTaskNames renames a step to another executable node's projected
// task name when that projection differs from the node's own ID.
func collideTaskNames(p *planpb.WorkflowPlan) {
	var step *planpb.GraphNode
	for _, node := range p.Graph.Nodes {
		if node.GetKind() == "step" {
			step = node
			break
		}
	}
	for _, node := range p.Graph.Nodes {
		projected := argoFieldName(node.GetId())
		if node == step || node.GetKind() == "start" || node.GetKind() == "end" || projected == node.GetId() {
			continue
		}
		from := step.GetId()
		for _, other := range p.Graph.Nodes {
			for index, source := range other.MergeInputs {
				if source == from {
					other.MergeInputs[index] = projected
				}
			}
			for _, input := range other.SelectInputs {
				if input.GetSource() == from {
					input.Source = pointer(projected)
				}
			}
		}
		for _, edge := range p.Graph.Edges {
			if edge.GetFrom() == from {
				edge.From = pointer(projected)
			}
			if edge.GetTo() == from {
				edge.To = pointer(projected)
			}
		}
		step.Id = pointer(projected)
		return
	}
}

// generatedNameCollision returns a task or template name that two plan
// nodes would share after projection onto Kubernetes names.
func generatedNameCollision(p *planpb.WorkflowPlan) string {
	// The workflow-entry task and template name is reserved for lowering.
	tasks, templates := map[string]bool{entryTaskName: true}, map[string]bool{"main": true, entryTaskName: true}
	for _, node := range p.Graph.Nodes {
		names := []string{}
		switch node.GetKind() {
		case "start", "end":
			continue
		case "map":
			names = []string{"map-", "map-expand-", "map-item-", "map-collect-"}
		default:
			names = []string{"step-"}
		}
		task := argoFieldName(node.GetId())
		if tasks[task] {
			return task
		}
		tasks[task] = true
		for _, prefix := range names {
			name := argoFieldName(prefix + node.GetId())
			if templates[name] {
				return name
			}
			templates[name] = true
		}
	}
	return ""
}

func checkLoweredDAG(t *testing.T, p *planpb.WorkflowPlan, templates []any) {
	t.Helper()
	graph := p.GetGraph()
	nodes := map[string]*planpb.GraphNode{}
	inbound := map[string][]*planpb.GraphEdge{}
	for _, node := range graph.GetNodes() {
		nodes[node.GetId()] = node
	}
	for _, edge := range graph.GetEdges() {
		inbound[edge.GetTo()] = append(inbound[edge.GetTo()], edge)
	}
	tasks := map[string]map[string]any{}
	for _, value := range templates[0].(map[string]any)["dag"].(map[string]any)["tasks"].([]any) {
		task := value.(map[string]any)
		name := task["name"].(string)
		if tasks[name] != nil || len(name) > 63 || !argoFieldNamePattern.MatchString(name) {
			t.Fatalf("task name %q is duplicated or not a DNS label", name)
		}
		tasks[name] = task
	}
	templateByName := map[string]map[string]any{}
	for _, value := range templates {
		template := value.(map[string]any)
		templateByName[template["name"].(string)] = template
	}
	executable := 0
	entered := false
	for _, node := range graph.GetNodes() {
		if node.GetKind() == "start" || node.GetKind() == "end" {
			continue
		}
		executable++
		name := argoFieldName(node.GetId())
		task := tasks[name]
		if task == nil {
			t.Fatalf("node %q has no task %q", node.GetId(), name)
		}
		// A decision or map fed by the workflow input reads it through the
		// workflow-entry task, which normalizes it once; a step normalizes its own.
		fromStart := inbound[node.GetId()][0].GetFrom() == graph.GetStartNode()
		throughEntry := fromStart && (node.GetKind() == "decision" || node.GetKind() == "map")
		sources := []string{}
		var caseEdge *planpb.GraphEdge
		for _, edge := range inbound[node.GetId()] {
			if edge.GetFrom() != graph.GetStartNode() {
				sources = append(sources, argoFieldName(edge.GetFrom()))
			}
			if edge.GetCase() != "" {
				caseEdge = edge
			}
		}
		if throughEntry {
			entered = true
			sources = append(sources, entryTaskName)
		}
		checkDepends(t, node, task, sources)

		// A branch runs only when its decision selected the case's index.
		when, conditional := task["when"].(string)
		if caseEdge == nil && conditional {
			t.Fatalf("unconditional task %s has when %q", name, when)
		}
		if caseEdge != nil {
			index := slices.IndexFunc(nodes[caseEdge.GetFrom()].GetCases(), func(c *planpb.DecisionCase) bool { return c.GetTag() == caseEdge.GetCase() })
			want := "{{=tasks[" + strconv.Quote(argoFieldName(caseEdge.GetFrom())) + "].outputs.parameters.selection == " + strconv.Quote(strconv.Itoa(index)) + "}}"
			if when != want {
				t.Fatalf("task %s when = %q, want %q", name, when, want)
			}
		}

		parameters := task["arguments"].(map[string]any)["parameters"].([]any)
		input := parameters[0].(map[string]any)["value"].(string)
		var stepArgs []any
		if node.GetKind() == "step" {
			stepArgs = templateByName[argoFieldName("step-"+node.GetId())]["container"].(map[string]any)["args"].([]any)
		}
		switch {
		case node.GetKind() == "select":
			if !strings.HasPrefix(input, "{{=") {
				t.Fatalf("select %s input %q is not lazy", name, input)
			}
		case len(node.GetMergeInputs()) > 0:
			// One parameter per source: a value reference is not JSON text, so
			// sources are never concatenated into an array expression.
			if len(parameters) != len(node.GetMergeInputs()) {
				t.Fatalf("merge %s has %d parameters for %d sources", name, len(parameters), len(node.GetMergeInputs()))
			}
			for index, source := range node.GetMergeInputs() {
				parameter := fmt.Sprintf("input-%d", index)
				want := map[string]any{"name": parameter, "value": "{{tasks." + argoFieldName(source) + ".outputs.parameters.result}}"}
				if got := parameters[index].(map[string]any); got["name"] != want["name"] || got["value"] != want["value"] {
					t.Fatalf("merge %s parameter %d = %v, want %v", name, index, got, want)
				}
				if !containsArgs(stepArgs, "--merge-input={{inputs.parameters."+parameter+"}}") {
					t.Fatalf("merge %s args %v lack source %d", name, stepArgs, index)
				}
			}
		case throughEntry:
			if input != "{{tasks."+entryTaskName+".outputs.parameters.result}}" {
				t.Fatalf("%s input %q does not come from %s", name, input, entryTaskName)
			}
		case fromStart:
			if input != "{{workflow.parameters.input}}" || (stepArgs != nil && !containsArgs(stepArgs, "--workflow-input={{inputs.parameters.input}}")) {
				t.Fatalf("first task %s input = %q, args %v", name, input, stepArgs)
			}
		default:
			if want := "{{tasks." + argoFieldName(inbound[node.GetId()][0].GetFrom()) + ".outputs.parameters.result}}"; input != want {
				t.Fatalf("task %s input = %q, want %q", name, input, want)
			}
		}

		if node.GetKind() == "map" {
			mapTemplate := templateByName[argoFieldName("map-"+node.GetId())]
			if parallelism, _ := mapTemplate["parallelism"].(float64); parallelism != float64(node.GetMaxConcurrency()) {
				t.Fatalf("map %s parallelism = %v, want %d", name, mapTemplate["parallelism"], node.GetMaxConcurrency())
			}
		}
		if node.GetKind() == "step" {
			checkRetry(t, p, node, templateByName[argoFieldName("step-"+node.GetId())])
		}
	}
	if entered {
		executable++
		entry := tasks[entryTaskName]
		if entry == nil || entry["arguments"].(map[string]any)["parameters"].([]any)[0].(map[string]any)["value"] != "{{workflow.parameters.input}}" {
			t.Fatalf("%s task = %v", entryTaskName, entry)
		}
	} else if tasks[entryTaskName] != nil {
		t.Fatalf("%s task without a control consumer", entryTaskName)
	}
	if len(tasks) != executable {
		t.Fatalf("DAG has %d tasks for %d executable nodes", len(tasks), executable)
	}
}

// checkDepends requires DAG readiness to equal the plan's inbound edges. A
// select also waits for its decision and for at least one successful source.
func checkDepends(t *testing.T, node *planpb.GraphNode, task map[string]any, sources []string) {
	t.Helper()
	depends, _ := task["depends"].(string)
	if len(sources) == 0 {
		if depends != "" {
			t.Fatalf("entry task for %s depends on %q", node.GetId(), depends)
		}
		return
	}
	want := []string{}
	for _, source := range sources {
		if node.GetKind() == "select" {
			want = append(want, "("+source+".Succeeded || "+source+".Skipped || "+source+".Omitted)")
		} else {
			want = append(want, source+".Succeeded")
		}
	}
	clauses := strings.Split(depends, " && ")
	if node.GetKind() == "select" {
		want = append(want, argoFieldName(node.GetDecisionRef())+".Succeeded")
		last := clauses[len(clauses)-1]
		successful := strings.Split(strings.TrimSuffix(strings.TrimPrefix(last, "("), ")"), " || ")
		expected := make([]string, 0, len(sources))
		for _, source := range sources {
			expected = append(expected, source+".Succeeded")
		}
		slices.Sort(successful)
		slices.Sort(expected)
		if !slices.Equal(successful, expected) {
			t.Fatalf("select %s requires success from %q, want %q", node.GetId(), last, expected)
		}
		clauses = clauses[:len(clauses)-1]
	}
	slices.Sort(clauses)
	slices.Sort(want)
	if !slices.Equal(clauses, want) {
		t.Fatalf("%s depends = %q, want clauses %q", node.GetId(), depends, want)
	}
}

func checkRetry(t *testing.T, p *planpb.WorkflowPlan, node *planpb.GraphNode, template map[string]any) {
	t.Helper()
	var contract *planpb.ExecutionContract
	for _, candidate := range p.GetContracts() {
		if candidate.GetContractRef() == node.GetContractRef() {
			contract = candidate
		}
	}
	strategy, retries := template["retryStrategy"].(map[string]any)
	attempts := contract.GetRetry().GetMaxAttempts()
	if retries != (attempts > 1) || (retries && strategy["limit"] != fmt.Sprint(attempts-1)) {
		t.Fatalf("step %s retry strategy %v for %d attempts", node.GetId(), strategy, attempts)
	}
}

func collectsItemFailures(p *planpb.WorkflowPlan) bool {
	for _, node := range p.Graph.Nodes {
		if node.GetItemFailures() != "" {
			return true
		}
	}
	return false
}
