package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/graphgen"
	"github.com/Sly1029/massive/internal/spec"
	"google.golang.org/protobuf/proto"
)

// specMutation breaks one semantic rule of a valid generated spec. It
// reports false when the spec lacks the construct it needs.
type specMutation struct {
	name   string
	mutate func(w *spec.WorkflowSpec, pick int) bool
}

// Every mutation is rehashed before parsing, so only semantic validation can
// reject it. The schema rejects a few as well; either layer must say no.
var specMutations = []specMutation{
	{"duplicate node id", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep)
		if !ok {
			return false
		}
		w.Graph.Nodes = append(w.Graph.Nodes, *node)
		return true
	}},
	{"dangling edge", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep)
		if !ok {
			return false
		}
		w.Graph.Edges = append(w.Graph.Edges, spec.GraphEdge{From: node.ID, To: "missing-node"})
		return true
	}},
	{"cycle", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep)
		if !ok {
			return false
		}
		w.Graph.Edges = append(w.Graph.Edges, spec.GraphEdge{From: node.ID, To: successorOf(w, w.Graph.Start)})
		return node.ID != successorOf(w, w.Graph.Start)
	}},
	{"self loop", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep)
		if !ok {
			return false
		}
		w.Graph.Edges = append(w.Graph.Edges, spec.GraphEdge{From: node.ID, To: node.ID})
		return true
	}},
	{"non-exhaustive select", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindSelect)
		if !ok {
			return false
		}
		node.SelectInputs = node.SelectInputs[1:]
		return true
	}},
	{"missing case edge", func(w *spec.WorkflowSpec, pick int) bool {
		index := pickEdge(w, pick, func(edge spec.GraphEdge) bool { return edge.Case != "" })
		if index < 0 {
			return false
		}
		w.Graph.Edges = slices.Delete(w.Graph.Edges, index, index+1)
		return true
	}},
	{"undeclared case", func(w *spec.WorkflowSpec, pick int) bool {
		index := pickEdge(w, pick, func(edge spec.GraphEdge) bool { return edge.Case != "" })
		if index < 0 {
			return false
		}
		w.Graph.Edges[index].Case = "undeclared case"
		return true
	}},
	{"duplicate case tag", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindDecision)
		if !ok {
			return false
		}
		node.Cases = append(node.Cases, node.Cases[0])
		return true
	}},
	{"unconditional decision output", func(w *spec.WorkflowSpec, pick int) bool {
		index := pickEdge(w, pick, func(edge spec.GraphEdge) bool { return edge.Case != "" })
		if index < 0 {
			return false
		}
		w.Graph.Edges[index].Case = ""
		return true
	}},
	{"merge input is not upstream", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep, func(node spec.GraphNode) bool { return len(node.MergeInputs) > 0 })
		if !ok {
			return false
		}
		node.MergeInputs[0] = node.ID
		return true
	}},
	{"undeclared merge", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep, func(node spec.GraphNode) bool { return len(node.MergeInputs) > 1 })
		if !ok {
			return false
		}
		node.MergeInputs = nil
		return true
	}},
	{"duplicate merge input", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep, func(node spec.GraphNode) bool { return len(node.MergeInputs) > 1 })
		if !ok {
			return false
		}
		node.MergeInputs[1] = node.MergeInputs[0]
		return true
	}},
	{"map without concurrency", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindMap)
		if !ok {
			return false
		}
		node.MaxConcurrency = 0
		return true
	}},
	{"map item schema mismatch", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindMap)
		if !ok {
			return false
		}
		node.ItemInputSchema = node.InputSchema
		return true
	}},
	{"map predecessor mismatch", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindMap)
		if !ok {
			return false
		}
		node.InputSchema, node.ItemInputSchema = node.ItemInputSchema, node.InputSchema
		return true
	}},
	{"decision input mismatch", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindDecision)
		if !ok {
			return false
		}
		node.InputSchema = node.Cases[0].Schema
		return true
	}},
	{"case target schema mismatch", func(w *spec.WorkflowSpec, pick int) bool {
		index := pickEdge(w, pick, func(edge spec.GraphEdge) bool { return edge.Case != "" })
		if index < 0 {
			return false
		}
		target, _ := pickNode(w, 0, spec.NodeKindStep, func(node spec.GraphNode) bool { return node.ID == w.Graph.Edges[index].To })
		target.InputSchema = w.Workflow.InputSchema
		return true
	}},
	{"select source swap", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindSelect, func(node spec.GraphNode) bool { return len(node.SelectInputs) > 1 })
		if !ok {
			return false
		}
		node.SelectInputs[0].Source, node.SelectInputs[1].Source = node.SelectInputs[1].Source, node.SelectInputs[0].Source
		return true
	}},
	{"select output mismatch", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindSelect)
		if !ok {
			return false
		}
		for ref := range w.Schemas {
			if ref != node.OutputSchema {
				node.OutputSchema = ref
				return true
			}
		}
		return false
	}},
	{"select of a step", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindSelect)
		if !ok {
			return false
		}
		node.DecisionRef = successorOf(w, w.Graph.Start)
		return true
	}},
	{"unreachable step", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep)
		if !ok {
			return false
		}
		orphan := *node
		orphan.ID, orphan.MergeInputs = "orphan", nil
		w.Graph.Nodes = append(w.Graph.Nodes, orphan)
		return true
	}},
	{"second start", func(w *spec.WorkflowSpec, pick int) bool {
		w.Graph.Nodes = append(w.Graph.Nodes, spec.GraphNode{ID: "second-start", Kind: spec.NodeKindStart})
		w.Graph.Edges = append(w.Graph.Edges, spec.GraphEdge{From: "second-start", To: successorOf(w, w.Graph.Start)})
		return true
	}},
	{"start fan-out", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep)
		if !ok {
			return false
		}
		w.Graph.Edges = append(w.Graph.Edges, spec.GraphEdge{From: w.Graph.Start, To: node.ID})
		return node.ID != successorOf(w, w.Graph.Start)
	}},
	{"end fan-in", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep, func(node spec.GraphNode) bool { return !slices.Contains(predecessorsOf(w, w.Graph.End), node.ID) })
		if !ok {
			return false
		}
		w.Graph.Edges = append(w.Graph.Edges, spec.GraphEdge{From: node.ID, To: w.Graph.End})
		return true
	}},
	{"missing symbol", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindStep)
		if !ok {
			return false
		}
		node.SymbolRef = "missing/symbol"
		return true
	}},
	{"missing contract", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindMap)
		if !ok {
			return false
		}
		node.ContractRef = "sha256:" + strings.Repeat("ab", 32)
		return true
	}},
	{"missing environment", func(w *spec.WorkflowSpec, pick int) bool {
		for ref, contract := range w.Contracts {
			contract.EnvironmentRef = "sha256:" + strings.Repeat("cd", 32)
			w.Contracts[ref] = contract
			return true
		}
		return false
	}},
	{"inverted retry delay", func(w *spec.WorkflowSpec, pick int) bool {
		for ref, contract := range w.Contracts {
			contract.Retry = &spec.RetryPolicy{MaxAttempts: 2, DelaySeconds: 5, BackoffFactor: 1, MaxDelaySeconds: 1}
			w.Contracts[ref] = contract
			return true
		}
		return false
	}},
	{"missing symbol package", func(w *spec.WorkflowSpec, pick int) bool {
		for ref, symbol := range w.Symbols {
			symbol.PackageID = "missing-package"
			w.Symbols[ref] = symbol
			return true
		}
		return false
	}},
	{"missing workflow output schema", func(w *spec.WorkflowSpec, pick int) bool {
		w.Workflow.OutputSchema = "sha256:" + strings.Repeat("ef", 32)
		return true
	}},
	{"unsafe node id", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindMap)
		if !ok {
			return false
		}
		renameNode(w, node.ID, "nested/"+node.ID)
		return true
	}},
	{"map item failure policy without its outcome schema", func(w *spec.WorkflowSpec, pick int) bool {
		node, ok := pickNode(w, pick, spec.NodeKindMap)
		if !ok {
			return false
		}
		// The collected list must change shape with the policy.
		if node.ItemFailures == "" {
			node.ItemFailures = spec.MapItemFailuresCollect
		} else {
			node.ItemFailures = ""
		}
		return true
	}},
}

// FuzzGeneratedSpecMutations starts from a valid generated workflow, breaks
// one semantic rule in the parsed struct, and recomputes the spec hash. The
// byte-level mutator rarely gets past the hash; this reaches the validator.
func FuzzGeneratedSpecMutations(f *testing.F) {
	for index := range specMutations {
		f.Add(uint8(index), uint8(0), []byte{0, 3, 2, 1, 5, 2, 1, 1, 4, 1, 2})
		f.Add(uint8(index), uint8(1), []byte{1, 2, 5, 3, 0, 1, 2, 1, 6, 1, 3, 3, 1})
	}
	f.Fuzz(func(t *testing.T, mutation uint8, pick uint8, data []byte) {
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
		compiled, err := Compile(parsed, workflow.JSON)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyCanonicalJSON(compiled.CanonicalJSON, compiled.PlanHash); err != nil {
			t.Fatal(err)
		}
		if err := ValidateControlFlow(compiled.Plan); err != nil {
			t.Fatalf("compiled plan fails target control-flow validation: %v", err)
		}

		chosen := specMutations[int(mutation)%len(specMutations)]
		mutated := cloneSpec(t, workflow.Spec)
		if !chosen.mutate(mutated, int(pick)) {
			return
		}
		body, err := graphgen.HashSpec(mutated)
		if err != nil {
			t.Fatal(err)
		}
		_, err = spec.Parse(body)
		var diagnostics *spec.DiagnosticsError
		if !errors.As(err, &diagnostics) {
			t.Fatalf("%s: rehashed invalid spec was accepted (error %v)", chosen.name, err)
		}
		for _, diagnostic := range diagnostics.Diagnostics {
			if diagnostic.Path == "$.specHash" {
				t.Fatalf("%s: rejected for its hash rather than its semantics", chosen.name)
			}
		}
	})
}

// FuzzPlanHashMetamorphic checks that the plan body depends on the graph,
// not on how a frontend ordered or named things. The spec hash covers the
// spec bytes, so the comparison excludes it and the plan hash over it.
func FuzzPlanHashMetamorphic(f *testing.F) {
	f.Add([]byte{0, 3, 2, 1, 5, 2, 1, 1, 4, 1, 2}, []byte{3, 1, 4, 1, 5})
	f.Add([]byte{1, 2, 5, 3, 0, 1, 2, 1, 6, 1, 3, 3, 1, 2, 2, 0, 1}, []byte{9, 2, 6})
	f.Fuzz(func(t *testing.T, data []byte, shuffle []byte) {
		if len(data) > 512 {
			t.Skip()
		}
		workflow, err := graphgen.Generate(data)
		if err != nil {
			t.Fatal(err)
		}
		original := compileGenerated(t, workflow.Spec)

		// Declaration order of nodes and edges is not part of the graph.
		permuted := cloneSpec(t, workflow.Spec)
		shuffleSlice(permuted.Graph.Nodes, shuffle)
		shuffleSlice(permuted.Graph.Edges, shuffle)
		if got, want := identityFree(t, compileGenerated(t, permuted)), identityFree(t, original); got != want {
			t.Fatalf("declaration order changed the plan:\n got %s\nwant %s", got, want)
		}

		// Spec-local references are replaced by content identities, so a
		// renamed or aliased schema, contract, or environment changes nothing.
		aliased := cloneSpec(t, workflow.Spec)
		aliasReferences(aliased)
		if got, want := identityFree(t, compileGenerated(t, aliased)), identityFree(t, original); got != want {
			t.Fatalf("spec-local references changed the plan:\n got %s\nwant %s", got, want)
		}

		// Renaming nodes and symbols is an isomorphism: mapping the names back
		// must reproduce the original graph.
		renamed := cloneSpec(t, workflow.Spec)
		names := map[string]string{}
		for _, node := range workflow.Spec.Graph.Nodes {
			if node.Kind != spec.NodeKindStart && node.Kind != spec.NodeKindEnd {
				names[node.ID] = renamedID(node.ID)
				renameNode(renamed, node.ID, names[node.ID])
			}
		}
		symbols := renameSymbols(renamed)
		restored := compileGenerated(t, renamed)
		restoreNames(restored.Plan, invert(names), invert(symbols))
		if got, want := sortedGraph(t, restored.Plan), sortedGraph(t, original.Plan); got != want {
			t.Fatalf("renaming changed the plan beyond its names:\n got %s\nwant %s", got, want)
		}
	})
}

func compileGenerated(t *testing.T, workflowSpec *spec.WorkflowSpec) *CompileResult {
	t.Helper()
	body, err := graphgen.HashSpec(workflowSpec)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := spec.Parse(body)
	if err != nil {
		t.Fatalf("equivalent spec rejected: %v", err)
	}
	compiled, err := Compile(parsed, body)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

// identityFree renders a plan without the identities derived from spec
// bytes, which legitimately change with declaration order.
func identityFree(t *testing.T, compiled *CompileResult) string {
	t.Helper()
	plan := proto.Clone(compiled.Plan).(*planpb.WorkflowPlan)
	plan.PlanHash, plan.SpecHash, plan.Provenance.SourceSpecHash = nil, nil, nil
	body, err := MarshalCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// sortedGraph additionally orders nodes and symbols by name, because the
// schedule breaks ties between independent nodes by their IDs.
func sortedGraph(t *testing.T, plan *planpb.WorkflowPlan) string {
	t.Helper()
	plan = proto.Clone(plan).(*planpb.WorkflowPlan)
	plan.PlanHash, plan.SpecHash, plan.Provenance.SourceSpecHash = nil, nil, nil
	sort.Slice(plan.Graph.Nodes, func(i, j int) bool { return plan.Graph.Nodes[i].GetId() < plan.Graph.Nodes[j].GetId() })
	sort.Slice(plan.Graph.Edges, func(i, j int) bool {
		left, right := plan.Graph.Edges[i], plan.Graph.Edges[j]
		return left.GetFrom()+"\x00"+left.GetTo() < right.GetFrom()+"\x00"+right.GetTo()
	})
	sort.Slice(plan.Symbols, func(i, j int) bool { return plan.Symbols[i].GetSymbolRef() < plan.Symbols[j].GetSymbolRef() })
	body, err := MarshalCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func cloneSpec(t *testing.T, workflowSpec *spec.WorkflowSpec) *spec.WorkflowSpec {
	t.Helper()
	body, err := json.Marshal(workflowSpec)
	if err != nil {
		t.Fatal(err)
	}
	var clone spec.WorkflowSpec
	if err := json.Unmarshal(body, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func pickNode(w *spec.WorkflowSpec, pick int, kind string, filters ...func(spec.GraphNode) bool) (*spec.GraphNode, bool) {
	var candidates []int
	for index, node := range w.Graph.Nodes {
		if node.Kind == kind && !slices.ContainsFunc(filters, func(filter func(spec.GraphNode) bool) bool { return !filter(node) }) {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	return &w.Graph.Nodes[candidates[pick%len(candidates)]], true
}

func pickEdge(w *spec.WorkflowSpec, pick int, filter func(spec.GraphEdge) bool) int {
	var candidates []int
	for index, edge := range w.Graph.Edges {
		if filter(edge) {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[pick%len(candidates)]
}

func successorOf(w *spec.WorkflowSpec, id string) string {
	for _, edge := range w.Graph.Edges {
		if edge.From == id {
			return edge.To
		}
	}
	return ""
}

func predecessorsOf(w *spec.WorkflowSpec, id string) []string {
	var sources []string
	for _, edge := range w.Graph.Edges {
		if edge.To == id {
			sources = append(sources, edge.From)
		}
	}
	return sources
}

// renameNode rewrites every reference to a node, as a frontend scoping pass does.
func renameNode(w *spec.WorkflowSpec, from, to string) {
	rename := func(id *string) {
		if *id == from {
			*id = to
		}
	}
	for index := range w.Graph.Nodes {
		node := &w.Graph.Nodes[index]
		rename(&node.ID)
		rename(&node.DecisionRef)
		for position := range node.MergeInputs {
			rename(&node.MergeInputs[position])
		}
		for position := range node.SelectInputs {
			rename(&node.SelectInputs[position].Source)
		}
	}
	for index := range w.Graph.Edges {
		rename(&w.Graph.Edges[index].From)
		rename(&w.Graph.Edges[index].To)
	}
}

// renamedID keeps the safe segment charset and length limit.
func renamedID(id string) string {
	digest := sha256.Sum256([]byte(id))
	return "R:" + hex.EncodeToString(digest[:6])
}

func renameSymbols(w *spec.WorkflowSpec) map[string]string {
	names := map[string]string{}
	symbols := map[string]spec.Symbol{}
	for ref, symbol := range w.Symbols {
		names[ref] = "renamed/" + renamedID(ref)
		symbols[names[ref]] = symbol
	}
	w.Symbols = symbols
	for index := range w.Graph.Nodes {
		if renamed, ok := names[w.Graph.Nodes[index].SymbolRef]; ok {
			w.Graph.Nodes[index].SymbolRef = renamed
		}
	}
	return names
}

func restoreNames(plan *planpb.WorkflowPlan, nodes, symbols map[string]string) {
	restore := func(value **string, names map[string]string) {
		if *value != nil {
			if original, ok := names[**value]; ok {
				*value = proto.String(original)
			}
		}
	}
	graph := plan.Graph
	for _, node := range graph.Nodes {
		restore(&node.Id, nodes)
		restore(&node.DecisionRef, nodes)
		restore(&node.SymbolRef, symbols)
		for position, source := range node.MergeInputs {
			if original, ok := nodes[source]; ok {
				node.MergeInputs[position] = original
			}
		}
		for _, input := range node.SelectInputs {
			restore(&input.Source, nodes)
		}
	}
	for _, edge := range graph.Edges {
		restore(&edge.From, nodes)
		restore(&edge.To, nodes)
	}
	for _, symbol := range plan.Symbols {
		restore(&symbol.SymbolRef, symbols)
	}
}

func invert(names map[string]string) map[string]string {
	inverted := make(map[string]string, len(names))
	for from, to := range names {
		inverted[to] = from
	}
	return inverted
}

// aliasReferences renames every schema, contract, and environment reference
// and declares an unused, identical second copy of each.
func aliasReferences(w *spec.WorkflowSpec) {
	alias := func(ref string, copy byte) string {
		digest := sha256.Sum256(append([]byte{copy}, ref...))
		return "sha256:" + hex.EncodeToString(digest[:])
	}
	rename := func(ref *string) {
		if *ref != "" {
			*ref = alias(*ref, 1)
		}
	}
	environments := map[string]spec.Environment{}
	for ref, environment := range w.Environments {
		environments[alias(ref, 1)], environments[alias(ref, 2)] = environment, environment
	}
	w.Environments = environments
	contracts := map[string]spec.ExecutionContract{}
	for ref, contract := range w.Contracts {
		rename(&contract.EnvironmentRef)
		contracts[alias(ref, 1)], contracts[alias(ref, 2)] = contract, contract
	}
	w.Contracts = contracts
	schemas := map[string]json.RawMessage{}
	for ref, document := range w.Schemas {
		schemas[alias(ref, 1)], schemas[alias(ref, 2)] = document, document
	}
	w.Schemas = schemas
	rename(&w.Workflow.InputSchema)
	rename(&w.Workflow.OutputSchema)
	for index := range w.Graph.Nodes {
		node := &w.Graph.Nodes[index]
		for _, ref := range []*string{&node.InputSchema, &node.OutputSchema, &node.ItemInputSchema, &node.ItemOutputSchema, &node.ContractRef} {
			rename(ref)
		}
		for position := range node.Cases {
			rename(&node.Cases[position].Schema)
		}
	}
}

func shuffleSlice[T any](values []T, shuffle []byte) {
	for index := len(values) - 1; index > 0; index-- {
		other := 0
		if len(shuffle) > 0 {
			other = int(shuffle[index%len(shuffle)]) % (index + 1)
		}
		values[index], values[other] = values[other], values[index]
	}
}
