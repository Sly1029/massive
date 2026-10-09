// Package graphgen derives valid Graph IR workflows from fuzz input and
// predicts how they execute. It is test support shared by the compiler,
// executor, and target fuzzers; production packages must not import it.
//
// Generation has two phases. The fuzz bytes first decode into a region tree,
// so a child graph can be instantiated at several call sites with identical
// structure and shared symbols. Emission then lowers the tree into a hashed
// WorkflowSpec the way frontends do: calls become scoped node IDs.
package graphgen

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/sourceidentity"
	"github.com/Sly1029/massive/internal/spec"
)

// Runner exit codes shared by every language adapter.
const (
	ExitSchemaValidation = 65
	ExitStepExecution    = 66
	ExitNonRetryable     = 67
	ExitKilled           = 137
)

// Fault is the scripted outcome of one invocation attempt.
type Fault uint8

const (
	Succeed Fault = iota
	// FailRetryable is an author exception.
	FailRetryable
	// Crash is a runner killed by a signal.
	Crash
	// FailNonRetryable is the author's explicit opt-out of retries.
	FailNonRetryable
	// FailSchema is a deterministic contract violation.
	FailSchema
	// Timeout exceeds the contract's per-attempt deadline.
	Timeout
	// OrphanFailure publishes an output and then fails. The output must not
	// be adopted, and a retry must use a new slot.
	OrphanFailure
	// MissingOutput reports success without publishing anything.
	MissingOutput
	faultCount
)

// Retryable reports whether another attempt may fix the failure.
func (f Fault) Retryable() bool {
	return f == FailRetryable || f == Crash || f == Timeout || f == OrphanFailure
}

// Invocation identifies one attempt of a step, or of a map item when Item is
// not negative.
type Invocation struct {
	NodeID  string
	Item    int
	Attempt int
}

// InterruptKind stops a run from inside the executor.
type InterruptKind uint8

const (
	// CancelBefore cancels the run before the batch starts any invocation.
	CancelBefore InterruptKind = iota
	// CancelDuring completes the invocations before Position, then cancels
	// while the invocation at Position runs.
	CancelDuring
	// CancelAfter completes the whole batch and cancels as it returns.
	CancelAfter
	// InfrastructureFailure fails the invocation at Position without a
	// runner outcome, after completing the earlier ones.
	InfrastructureFailure
	interruptCount
)

// Interrupt applies to the Batch'th StepInvoker call, counting from zero.
type Interrupt struct {
	Batch    int
	Position int
	Kind     InterruptKind
}

// Requirement is one decision case a node needs in order to be active.
type Requirement struct {
	Decision string
	Case     string
}

// Node is the generator's semantic record of one emitted graph node.
type Node struct {
	ID   string
	Kind string
	// Symbol names the behavior of a step or map item.
	Symbol string
	// Sources are the value inputs in delivery order: one for ordinary
	// nodes, mergeInputs order for merges. Decisions forward their source.
	Sources []string
	// Merge steps receive the array of their source values.
	Merge bool
	// Context lists enclosing decision cases from outermost to innermost.
	Context []Requirement
	// Decision fields.
	Cases []string
	// Select fields.
	DecisionRef   string
	SelectSources map[string]string
	// Executable fields.
	MaxAttempts    int
	TimeoutSeconds uint32
	MaxConcurrency uint32
	// CollectItemFailures makes a map succeed with one outcome per item.
	CollectItemFailures bool
}

// Workflow is a generated spec plus everything needed to execute and predict it.
type Workflow struct {
	Spec *spec.WorkflowSpec
	// JSON is the hashed spec as a frontend would write it.
	JSON []byte
	// SourcePath and Source form the single file of the source package.
	SourcePath string
	Source     []byte
	Input      []byte
	Behaviors  map[string]Behavior
	Nodes      map[string]*Node
	// NodeOrder lists nodes in emission order.
	NodeOrder []string
	Faults    map[Invocation]Fault
	Interrupt *Interrupt
}

// Fault returns the scripted outcome of an invocation.
func (w *Workflow) Fault(invocation Invocation) Fault {
	return w.Faults[invocation]
}

type valueType uint8

const (
	objectValue valueType = iota
	listValue
)

const objectSchemaJSON = `{"type":"object","required":["kind","value"],"properties":{"kind":{"type":"string"},"value":{"type":"string"}},"additionalProperties":false}`

func caseSchemaJSON(tag string) string {
	quoted, _ := json.Marshal(tag)
	return `{"type":"object","required":["kind","value"],"properties":{"kind":{"const":` + string(quoted) + `},"value":{"type":"string"}},"additionalProperties":false}`
}

const listSchemaJSON = `{"type":"array","items":` + objectSchemaJSON + `}`

// outcomeListSchemaJSON is the output of a map that collects item failures,
// spelled as Pydantic emits it: definitions behind references, with titles
// and a discriminator that validation ignores.
const outcomeListSchemaJSON = `{"$defs":{` +
	`"Failed":{"additionalProperties":false,"properties":{"failure":{"$ref":"#/$defs/Failure"},"status":{"const":"failed","title":"Status","type":"string"}},"required":["failure","status"],"title":"Failed","type":"object"},` +
	`"Failure":{"additionalProperties":false,"properties":{"attempts":{"minimum":1,"type":"integer"},"diagnostic":{"maxLength":1024,"type":"string"},"kind":{"enum":["error","killed","non-retryable","timeout"],"type":"string"}},"required":["attempts","diagnostic","kind"],"title":"Failure","type":"object"},` +
	`"Item":` + objectSchemaJSON + `,` +
	`"Succeeded":{"additionalProperties":false,"properties":{"status":{"const":"succeeded","type":"string"},"value":{"$ref":"#/$defs/Item"}},"required":["status","value"],"type":"object"}},` +
	`"items":{"discriminator":{"propertyName":"status"},"oneOf":[{"$ref":"#/$defs/Succeeded"},{"$ref":"#/$defs/Failed"}]},"type":"array"}`

// A merge step receives exactly one value per declared input, in order.
func mergeSchemaJSON(arity int) string {
	return fmt.Sprintf(`{"type":"array","minItems":%d,"maxItems":%d}`, arity, arity)
}

const (
	pinnedImage = "ghcr.io/massive-dev/generated-runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	packageID   = "generated"
	sourcePath  = "workflow.py"
	selector    = "kind"
)

// Generate decodes fuzz bytes into a valid workflow, a fault script, and an
// optional interrupt. Every byte sequence produces a workflow.
//
// Map item-failure policies are read last, after the fault script, so seeds
// written before the policy existed keep their meaning. The workflow is then
// emitted again with those policies; node IDs do not depend on them.
func Generate(data []byte) (*Workflow, error) {
	c := &choices{data: data}
	style := idStyle(c.intn(int(idStyleCount)))
	main := parseTemplate(c, "main", 0, &parseState{})
	b, workflow, err := emitWorkflow(main, style, nil)
	if err != nil {
		return nil, err
	}
	workflow.Input, err = canonical.Marshal(map[string]string{"kind": "input", "value": fmt.Sprintf("%02x", c.byte())})
	if err != nil {
		return nil, err
	}
	scriptFaults(c, workflow)
	collect := map[*mapRegion]bool{}
	for _, region := range b.mapRegions {
		if c.chance(1, 2) {
			collect[region] = true
		}
	}
	if len(collect) == 0 {
		return workflow, nil
	}
	_, collecting, err := emitWorkflow(main, style, collect)
	if err != nil {
		return nil, err
	}
	collecting.Input, collecting.Faults, collecting.Interrupt = workflow.Input, workflow.Faults, workflow.Interrupt
	return collecting, nil
}

func emitWorkflow(main *template, style idStyle, collect map[*mapRegion]bool) (*builder, *Workflow, error) {
	environment := spec.Environment{Kind: "container", Image: pinnedImage, Platform: "linux/amd64"}
	environmentRef, err := digestValue(environment)
	if err != nil {
		return nil, nil, err
	}
	b := &builder{
		style:          style,
		collect:        collect,
		environmentRef: environmentRef,
		schemas:        map[string]json.RawMessage{},
		symbols:        map[string]spec.Symbol{},
		contracts:      map[string]spec.ExecutionContract{},
		behaviors:      map[string]Behavior{},
		nodes:          map[string]*Node{},
	}
	b.addNode(spec.GraphNode{ID: "__start", Kind: spec.NodeKindStart}, &Node{ID: "__start", Kind: spec.NodeKindStart})
	start := value{id: "__start", typ: objectValue}
	exit := b.emitRegion(main.body, start, entry{}, &scope{template: "main"})
	b.addNode(spec.GraphNode{ID: "__end", Kind: spec.NodeKindEnd}, &Node{ID: "__end", Kind: spec.NodeKindEnd, Sources: []string{exit.id}})
	b.edges = append(b.edges, spec.GraphEdge{From: exit.id, To: "__end"})

	source := []byte("# generated workflow source\n")
	sourceHash := canonical.DigestBytes(source)
	packageHash, err := sourceidentity.Digest([]sourceidentity.File{{Path: sourcePath, Hash: sourceHash}})
	if err != nil {
		return nil, nil, err
	}
	workflowSpec := &spec.WorkflowSpec{
		Kind: "WorkflowSpec", SchemaVersion: 0, Encoding: "json-v0",
		Hashing: spec.HashingSpec{Algorithm: "sha256", Canonicalization: "canonical-json-v0", Recipe: "workflow-spec", RecipeVersion: 1},
		Workflow: spec.Workflow{
			Name: "generated", InputSchema: b.schema(objectSchemaJSON), OutputSchema: b.schema(typeSchemaJSON(exit.typ)),
		},
		Graph:   spec.Graph{IRVersion: "0.3", Start: "__start", End: "__end", Nodes: b.graphNodes, Edges: b.edges},
		Schemas: b.schemas,
		Symbols: b.symbols,
		SourcePackages: map[string]spec.SourcePackage{packageID: {
			PackageID: packageID, Language: "python", PackageHash: packageHash,
			Hashing: spec.HashingSpec{Algorithm: "sha256", Canonicalization: "canonical-json-v0", Recipe: "source-package", RecipeVersion: 1},
			Files:   []spec.SourcePackageFile{{Path: sourcePath, Hash: sourceHash}},
		}},
		Environments: map[string]spec.Environment{environmentRef: environment},
		Contracts:    b.contracts,
	}
	body, err := HashSpec(workflowSpec)
	if err != nil {
		return nil, nil, err
	}
	return b, &Workflow{
		Spec: workflowSpec, JSON: body, SourcePath: sourcePath, Source: source,
		Behaviors: b.behaviors, Nodes: b.nodes, NodeOrder: b.order, Faults: map[Invocation]Fault{},
	}, nil
}

// HashSpec embeds the recomputed spec hash and returns the spec bytes.
func HashSpec(workflowSpec *spec.WorkflowSpec) ([]byte, error) {
	workflowSpec.SpecHash = ""
	body, err := json.Marshal(workflowSpec)
	if err != nil {
		return nil, err
	}
	if workflowSpec.SpecHash, err = spec.RecomputedSpecHash(body); err != nil {
		return nil, err
	}
	return json.Marshal(workflowSpec)
}

func digestValue(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return canonical.DigestJSON(body)
}

func typeSchemaJSON(typ valueType) string {
	if typ == listValue {
		return listSchemaJSON
	}
	return objectSchemaJSON
}

// scriptFaults assigns attempt outcomes to a few executable nodes and items.
func scriptFaults(c *choices, w *Workflow) {
	for _, id := range w.NodeOrder {
		node := w.Nodes[id]
		if node.Kind == spec.NodeKindStep && !c.chance(1, 4) || node.Kind == spec.NodeKindMap && !c.chance(1, 2) {
			continue
		}
		items := []int{-1}
		if node.Kind == spec.NodeKindMap {
			// Several items may fail in one round, so a terminal failure meets
			// siblings that are waiting for a retry.
			items = items[:0]
			for range 2 + c.intn(maxMapItems-1) {
				items = append(items, c.intn(maxMapItems+1))
			}
		}
		for _, item := range items {
			for attempt := 1; attempt <= node.MaxAttempts; attempt++ {
				fault := Fault(c.intn(int(faultCount)))
				if fault == Timeout && node.TimeoutSeconds == 0 {
					fault = FailRetryable
				}
				w.Faults[Invocation{NodeID: id, Item: item, Attempt: attempt}] = fault
				if !fault.Retryable() || c.chance(1, 2) {
					break
				}
			}
		}
	}
	if c.chance(1, 4) {
		w.Interrupt = &Interrupt{Batch: c.intn(12), Position: c.intn(maxMapItems + 2), Kind: InterruptKind(c.intn(int(interruptCount)))}
	}
}

// choices reads fuzz bytes as bounded decisions. Exhausted input reads as
// zero, which always selects the smallest construct, so generation ends.
type choices struct {
	data []byte
	next int
}

func (c *choices) byte() byte {
	if c.next >= len(c.data) {
		return 0
	}
	value := c.data[c.next]
	c.next++
	return value
}

func (c *choices) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(c.byte()) % n
}

// chance is false for exhausted input, so trailing zeros add no constructs.
func (c *choices) chance(numerator, denominator int) bool {
	return c.intn(denominator) >= denominator-numerator
}

// take reserves the next n bytes for a child graph definition.
func (c *choices) take(n int) []byte {
	start := min(c.next, len(c.data))
	end := min(start+n, len(c.data))
	c.next = end
	return c.data[start:end]
}

type idStyle uint8

const (
	plainIDs idStyle = iota
	// mixedIDs uses mixed case and the full safe segment punctuation.
	mixedIDs
	// longIDs pads every leaf node ID to the 128-character segment limit.
	longIDs
	idStyleCount
)

const maxSegmentLength = 128

var decorations = []string{".v@1", ":Q#z", "_A-b", "#@:.", "-Z_", "@x.Y"}

// callID keeps call prefixes short so scoped leaf IDs fit one segment.
func (style idStyle) callID(prefix, local string) string {
	if style == mixedIDs {
		return prefix + strings.ToUpper(local[:1]) + local[1:] + ":k"
	}
	return prefix + local
}

func (style idStyle) nodeID(prefix, local string, index int) string {
	switch style {
	case mixedIDs:
		return prefix + strings.ToUpper(local[:1]) + local[1:] + decorations[index%len(decorations)]
	case longIDs:
		id := prefix + local + "."
		filler := "AbC_d-E.f@G:h#"
		for len(id) < maxSegmentLength {
			id += filler[len(id)%len(filler) : len(id)%len(filler)+1]
		}
		return strings.TrimRight(id[:maxSegmentLength], "-.")
	default:
		return prefix + local
	}
}
