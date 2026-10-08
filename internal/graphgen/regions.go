package graphgen

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/spec"
)

const (
	maxDepth       = 4
	maxMapItems    = 3
	templateBudget = 10
	childBytes     = 48
)

// The fuzzer controls which decision case each classifier tends to select;
// tags include characters that need quoting in target expressions.
var tagPool = []string{"a", "B", "case two", `quo"te`, "é", "{{item}}", "0", "-", "selection", "a.b", "x'y"}

var itemCounts = []int{0, 1, 2, 3, 7, 64}

var maxConcurrencies = []uint32{1, 2, 3, 7, 64, 1<<31 - 1, 1<<32 - 1}

// Contracts cover no policy, retries, a per-attempt deadline, both, and
// resource requests. Retry delays are zero so fuzzing never sleeps.
var contractMenu = []spec.ExecutionContract{
	{},
	{Retry: &spec.RetryPolicy{MaxAttempts: 3, BackoffFactor: 2}},
	{TimeoutSeconds: 7},
	{Retry: &spec.RetryPolicy{MaxAttempts: 2, BackoffFactor: 1}, TimeoutSeconds: 3},
	{Retry: &spec.RetryPolicy{MaxAttempts: 5, BackoffFactor: 1}, Network: &spec.NetworkPolicy{Egress: "any"}},
	{Resources: &spec.ResourceRequirements{CPU: "250m", Memory: "64Mi"}},
}

type region interface {
	outType() valueType
}

type stepRegion struct {
	local    string
	behavior Behavior
	contract int
}

type chainRegion struct {
	parts []region
}

// fanRegion sends one value to independent branches and merges their exits,
// optionally with the source value itself, in a generated order.
type fanRegion struct {
	branches      []region
	includeSource bool
	order         []int
	merge         *stepRegion
}

type decisionRegion struct {
	classify    *stepRegion
	local       string
	tags        []string
	branches    []region
	selectLocal string
	order       []int
	typ         valueType
}

// mapRegion expands an object into a list unless its input is already a list.
type mapRegion struct {
	expand         *stepRegion
	local          string
	contract       int
	maxConcurrency uint32
}

// callRegion instantiates a child graph once or twice in sequence, as a
// frontend expands a reusable graph call into scoped node IDs.
type callRegion struct {
	locals []string
	child  *template
}

type template struct {
	name string
	body region
}

func (r *stepRegion) outType() valueType     { return r.behavior.outType() }
func (r *chainRegion) outType() valueType    { return r.parts[len(r.parts)-1].outType() }
func (r *fanRegion) outType() valueType      { return r.merge.outType() }
func (r *decisionRegion) outType() valueType { return r.typ }
func (r *mapRegion) outType() valueType      { return listValue }
func (r *callRegion) outType() valueType     { return r.child.body.outType() }

type parseState struct {
	templates []*template
	named     int
}

type parser struct {
	c      *choices
	state  *parseState
	locals int
	budget int
}

func parseTemplate(c *choices, name string, depth int, state *parseState) *template {
	p := &parser{c: c, state: state, budget: templateBudget}
	// A graph has one entry step so that the start node, or a call site,
	// has exactly one successor.
	body := &chainRegion{parts: []region{p.step(p.behavior(false))}}
	if !p.c.chance(1, 8) {
		body.parts = append(body.parts, p.region(depth))
	}
	return &template{name: name, body: body}
}

func (p *parser) local(kind string) string {
	p.locals++
	return fmt.Sprintf("%s%d", kind, p.locals-1)
}

func (p *parser) step(behavior Behavior) *stepRegion {
	return &stepRegion{local: p.local("s"), behavior: behavior, contract: p.c.intn(len(contractMenu))}
}

func (p *parser) behavior(listOutput bool) Behavior {
	if listOutput || p.c.chance(1, 6) {
		return Behavior{Kind: Expand, Items: itemCounts[p.c.intn(len(itemCounts))]}
	}
	return Behavior{Kind: Transform}
}

func (p *parser) region(depth int) region {
	if p.budget <= 0 || depth >= maxDepth {
		return p.step(p.behavior(false))
	}
	p.budget--
	switch p.c.intn(7) {
	case 1:
		chain := &chainRegion{}
		for range 2 + p.c.intn(2) {
			chain.parts = append(chain.parts, p.region(depth+1))
		}
		return chain
	case 2:
		return p.fan(depth)
	case 3, 4:
		return p.decision(depth)
	case 5:
		return p.mapRegion(depth)
	case 6:
		return p.call(depth)
	default:
		return p.step(p.behavior(false))
	}
}

func (p *parser) fan(depth int) region {
	fan := &fanRegion{includeSource: p.c.chance(1, 3)}
	for range 1 + p.c.intn(3) {
		fan.branches = append(fan.branches, p.region(depth+1))
	}
	inputs := len(fan.branches) + 1
	fan.order = p.permutation(inputs)
	fan.merge = p.step(p.behavior(false))
	return fan
}

func (p *parser) decision(depth int) region {
	decision := &decisionRegion{classify: p.step(Behavior{Kind: Classify}), local: p.local("d")}
	for range 1 + p.c.intn(4) {
		tag := tagPool[p.c.intn(len(tagPool))]
		if slices.Contains(decision.tags, tag) {
			continue
		}
		decision.tags = append(decision.tags, tag)
	}
	decision.classify.behavior.Tags = decision.tags
	decision.classify.behavior.Bias = p.c.intn(len(decision.tags))
	for index := range decision.tags {
		// A conditional edge targets exactly one step: the branch entry.
		branch := &chainRegion{parts: []region{p.step(p.behavior(false))}}
		if p.c.chance(2, 3) {
			branch.parts = append(branch.parts, p.region(depth+1))
		}
		if index == 0 {
			decision.typ = branch.outType()
		} else if branch.outType() != decision.typ {
			adapter := Behavior{Kind: Transform}
			if decision.typ == listValue {
				adapter = Behavior{Kind: Expand, Items: itemCounts[p.c.intn(len(itemCounts))]}
			}
			branch.parts = append(branch.parts, p.step(adapter))
		}
		decision.branches = append(decision.branches, branch)
	}
	decision.selectLocal = p.local("j")
	decision.order = p.permutation(len(decision.tags))
	return decision
}

func (p *parser) mapRegion(depth int) region {
	return &mapRegion{
		expand:         p.step(p.behavior(true)),
		local:          p.local("m"),
		contract:       p.c.intn(len(contractMenu)),
		maxConcurrency: maxConcurrencies[p.c.intn(len(maxConcurrencies))],
	}
}

func (p *parser) call(depth int) region {
	call := &callRegion{}
	if existing := len(p.state.templates); existing > 0 && p.c.chance(1, 2) {
		call.child = p.state.templates[p.c.intn(existing)]
	} else {
		// Reserve the name before parsing: nested children are named first
		// but only completed templates may be reused, which rules out recursion.
		name := fmt.Sprintf("g%d", p.state.named)
		p.state.named++
		childChoices := &choices{data: p.c.take(int(p.c.byte()) % childBytes)}
		call.child = parseTemplate(childChoices, name, depth+1, p.state)
		p.state.templates = append(p.state.templates, call.child)
	}
	for range 1 + p.c.intn(2) {
		call.locals = append(call.locals, p.local("c"))
	}
	return call
}

func (p *parser) permutation(n int) []int {
	order := make([]int, n)
	for index := range order {
		order[index] = index
	}
	for index := n - 1; index > 0; index-- {
		other := p.c.intn(index + 1)
		order[index], order[other] = order[other], order[index]
	}
	return order
}

type value struct {
	id  string
	typ valueType
}

// entry carries a conditional edge to the first node a region emits.
type entry struct {
	edgeCase    string
	inputSchema string
}

type scope struct {
	prefix   string
	template string
	context  []Requirement
}

type builder struct {
	style          idStyle
	environmentRef string
	schemas        map[string]json.RawMessage
	symbols        map[string]spec.Symbol
	contracts      map[string]spec.ExecutionContract
	behaviors      map[string]Behavior
	nodes          map[string]*Node
	order          []string
	graphNodes     []spec.GraphNode
	edges          []spec.GraphEdge
	emitted        int
}

func (b *builder) schema(document string) string {
	ref, err := canonical.DigestJSON([]byte(document))
	if err != nil {
		panic(err)
	}
	b.schemas[ref] = json.RawMessage(document)
	return ref
}

// contract records a menu entry under its content digest.
func (b *builder) contract(index int) (string, spec.ExecutionContract) {
	contract := contractMenu[index]
	contract.EnvironmentRef = b.environmentRef
	ref, err := digestValue(contract)
	if err != nil {
		panic(err)
	}
	b.contracts[ref] = contract
	return ref, contract
}

func (b *builder) addNode(graphNode spec.GraphNode, node *Node) {
	b.graphNodes = append(b.graphNodes, graphNode)
	b.nodes[node.ID] = node
	b.order = append(b.order, node.ID)
}

func (sc *scope) id(b *builder, local string) string {
	b.emitted++
	return b.style.nodeID(sc.prefix, local, b.emitted)
}

func (b *builder) emitRegion(r region, source value, in entry, sc *scope) value {
	switch r := r.(type) {
	case *stepRegion:
		return b.emitStep(r, []value{source}, in, sc, false)
	case *chainRegion:
		current := source
		for index, part := range r.parts {
			if index > 0 {
				in = entry{}
			}
			current = b.emitRegion(part, current, in, sc)
		}
		return current
	case *fanRegion:
		candidates := make([]value, 0, len(r.branches)+1)
		for _, branch := range r.branches {
			candidates = append(candidates, b.emitRegion(branch, source, in, sc))
		}
		// The start node is not a value producer that a merge may name.
		includeSource := r.includeSource && source.id != "__start"
		if includeSource {
			candidates = append(candidates, source)
		}
		inputs := make([]value, 0, len(candidates))
		for _, index := range r.order {
			if index < len(candidates) {
				inputs = append(inputs, candidates[index])
			}
		}
		return b.emitStep(r.merge, inputs, entry{}, sc, true)
	case *decisionRegion:
		classifier := b.emitStep(r.classify, []value{source}, in, sc, false)
		decisionID := sc.id(b, r.local)
		cases := make([]spec.DecisionCase, 0, len(r.tags))
		for _, tag := range r.tags {
			cases = append(cases, spec.DecisionCase{Tag: tag, Schema: b.schema(caseSchemaJSON(tag))})
		}
		b.addNode(
			spec.GraphNode{ID: decisionID, Kind: spec.NodeKindDecision, InputSchema: b.schema(objectSchemaJSON), Selector: selector, Cases: cases},
			&Node{ID: decisionID, Kind: spec.NodeKindDecision, Sources: []string{classifier.id}, Context: sc.context, Cases: r.tags},
		)
		b.edges = append(b.edges, spec.GraphEdge{From: classifier.id, To: decisionID})
		exits := make([]string, len(r.tags))
		for index, tag := range r.tags {
			branchScope := &scope{prefix: sc.prefix, template: sc.template, context: append(slices.Clip(sc.context), Requirement{Decision: decisionID, Case: tag})}
			exits[index] = b.emitRegion(r.branches[index], value{id: decisionID, typ: objectValue}, entry{edgeCase: tag, inputSchema: b.schema(caseSchemaJSON(tag))}, branchScope).id
		}
		selectID := sc.id(b, r.selectLocal)
		inputs := make([]spec.SelectInput, 0, len(r.tags))
		sources := make(map[string]string, len(r.tags))
		for _, index := range r.order {
			inputs = append(inputs, spec.SelectInput{Case: r.tags[index], Source: exits[index]})
			sources[r.tags[index]] = exits[index]
			b.edges = append(b.edges, spec.GraphEdge{From: exits[index], To: selectID})
		}
		b.addNode(
			spec.GraphNode{ID: selectID, Kind: spec.NodeKindSelect, DecisionRef: decisionID, OutputSchema: b.schema(typeSchemaJSON(r.typ)), SelectInputs: inputs},
			&Node{ID: selectID, Kind: spec.NodeKindSelect, Context: sc.context, DecisionRef: decisionID, SelectSources: sources},
		)
		return value{id: selectID, typ: r.typ}
	case *mapRegion:
		input := source
		if source.typ != listValue || in != (entry{}) {
			input = b.emitStep(r.expand, []value{source}, in, sc, false)
		}
		mapID := sc.id(b, r.local)
		symbol := b.symbol(sc, r.local, Behavior{Kind: Transform})
		contractRef, contract := b.contract(r.contract)
		b.addNode(
			spec.GraphNode{
				ID: mapID, Kind: spec.NodeKindMap, InputSchema: b.schema(listSchemaJSON),
				ItemInputSchema: b.schema(objectSchemaJSON), ItemOutputSchema: b.schema(objectSchemaJSON),
				OutputSchema: b.schema(listSchemaJSON), SymbolRef: symbol, ContractRef: contractRef, MaxConcurrency: r.maxConcurrency,
			},
			&Node{
				ID: mapID, Kind: spec.NodeKindMap, Symbol: symbol, Sources: []string{input.id}, Context: sc.context,
				MaxAttempts: maxAttempts(contract), TimeoutSeconds: contract.TimeoutSeconds, MaxConcurrency: r.maxConcurrency,
			},
		)
		b.edges = append(b.edges, spec.GraphEdge{From: input.id, To: mapID})
		return value{id: mapID, typ: listValue}
	case *callRegion:
		current := source
		for index, local := range r.locals {
			if index > 0 {
				in = entry{}
			}
			childScope := &scope{prefix: b.style.callID(sc.prefix, local) + "--", template: r.child.name, context: sc.context}
			current = b.emitRegion(r.child.body, current, in, childScope)
		}
		return current
	default:
		panic(fmt.Sprintf("unknown region %T", r))
	}
}

// emitStep emits an ordinary step, or a merge step that declares its inputs
// even when there is only one.
func (b *builder) emitStep(r *stepRegion, inputs []value, in entry, sc *scope, merge bool) value {
	id := sc.id(b, r.local)
	sources := make([]string, 0, len(inputs))
	for _, input := range inputs {
		sources = append(sources, input.id)
		b.edges = append(b.edges, spec.GraphEdge{From: input.id, To: id, Case: in.edgeCase})
	}
	inputSchema := in.inputSchema
	var mergeInputs []string
	if merge {
		mergeInputs = sources
		inputSchema = b.schema(mergeSchemaJSON(len(inputs)))
	} else if inputSchema == "" {
		inputSchema = b.schema(typeSchemaJSON(inputs[0].typ))
	}
	symbol := b.symbol(sc, r.local, r.behavior)
	contractRef, contract := b.contract(r.contract)
	b.addNode(
		spec.GraphNode{
			ID: id, Kind: spec.NodeKindStep, InputSchema: inputSchema, OutputSchema: b.schema(typeSchemaJSON(r.outType())),
			SymbolRef: symbol, ContractRef: contractRef, MergeInputs: mergeInputs,
		},
		&Node{
			ID: id, Kind: spec.NodeKindStep, Symbol: symbol, Sources: sources, Merge: merge, Context: sc.context,
			MaxAttempts: maxAttempts(contract), TimeoutSeconds: contract.TimeoutSeconds,
		},
	)
	return value{id: id, typ: r.outType()}
}

func (b *builder) symbol(sc *scope, local string, behavior Behavior) string {
	ref := sc.template + "/" + local
	b.symbols[ref] = spec.Symbol{PackageID: packageID, Language: "python", Module: sourcePath, Export: sc.template + "_" + local}
	b.behaviors[ref] = behavior
	return ref
}

func maxAttempts(contract spec.ExecutionContract) int {
	if contract.Retry == nil {
		return 1
	}
	return int(contract.Retry.MaxAttempts)
}
