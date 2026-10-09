package graphgen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/spec"
)

type BehaviorKind uint8

const (
	// Transform digests its input into an object.
	Transform BehaviorKind = iota
	// Classify digests its input into an object whose kind is a decision tag.
	Classify
	// Expand digests its input into a list of Items objects.
	Expand
)

// Behavior is a deterministic step implementation: its output depends only
// on its symbol and canonical input, like a pure author function.
type Behavior struct {
	Kind  BehaviorKind
	Tags  []string
	Bias  int
	Items int
}

func (b Behavior) outType() valueType {
	if b.Kind == Expand {
		return listValue
	}
	return objectValue
}

// Evaluate computes the canonical JSON output for one canonical JSON input.
func (b Behavior) Evaluate(symbol string, input []byte) ([]byte, error) {
	digest := sha256.Sum256(append([]byte(symbol+"\x00"), input...))
	text := hex.EncodeToString(digest[:8])
	switch b.Kind {
	case Classify:
		return canonical.Marshal(map[string]string{"kind": b.Tags[(int(digest[0])+b.Bias)%len(b.Tags)], "value": text})
	case Expand:
		items := make([]map[string]string, b.Items)
		for index := range items {
			itemDigest := sha256.Sum256(fmt.Appendf(digest[:], "%d", index))
			items[index] = map[string]string{"kind": "item", "value": hex.EncodeToString(itemDigest[:8])}
		}
		return canonical.Marshal(items)
	default:
		return canonical.Marshal(map[string]string{"kind": "value", "value": text})
	}
}

type Status uint8

const (
	Succeeded Status = iota
	// Failed means the node fails whenever it runs.
	Failed
	// Blocked means an upstream node fails, so this node never runs.
	Blocked
	// Skipped means an enclosing decision did not select this node's case.
	Skipped
)

// Outcome is the interpreter's prediction for one node.
type Outcome struct {
	Status Status
	// Skip is the outermost unselected case for a skipped node.
	Skip  Requirement
	Input []byte
	Value []byte
	// Attempts lists each attempt's scripted fault for a step.
	Attempts []Fault
	// Items lists each source-indexed map item when the map runs.
	Items        []ItemOutcome
	SelectedCase string
}

type ItemOutcome struct {
	Input    []byte
	Value    []byte
	Attempts []Fault
	Status   Status
	// Failure is the record a collecting map keeps for a failed item.
	Failure *ItemFailure
}

// ItemFailure is the failure record of a collected item outcome.
type ItemFailure struct {
	Attempts   int    `json:"attempts"`
	Diagnostic string `json:"diagnostic"`
	Kind       string `json:"kind"`
}

// collectedFailure states which terminal faults a collecting map keeps, and
// their records. Schema failures and missing outputs violate the contract,
// so they still fail the map.
func collectedFailure(fault Fault, attempt int, timeoutSeconds uint32) *ItemFailure {
	failure := &ItemFailure{Attempts: attempt}
	switch fault {
	case FailRetryable, OrphanFailure:
		failure.Kind, failure.Diagnostic = "error", "step-execution-failure (exit 66)"
	case Crash:
		failure.Kind, failure.Diagnostic = "killed", "runner-failure (exit 137)"
	case FailNonRetryable:
		failure.Kind, failure.Diagnostic = "non-retryable", "non-retryable-step-failure (exit 67)"
	case Timeout:
		failure.Kind, failure.Diagnostic = "timeout", fmt.Sprintf("step-timeout (timed out after %ds)", timeoutSeconds)
	default:
		return nil
	}
	return failure
}

type Prediction struct {
	Nodes     map[string]*Outcome
	Succeeded bool
	Result    []byte
}

// Predict interprets the workflow directly from the generator's records. It
// shares no scheduling, activation, or retry code with the orchestrator.
func (w *Workflow) Predict() (*Prediction, error) {
	interpreter := &interpreter{workflow: w, outcomes: map[string]*Outcome{}}
	prediction := &Prediction{Nodes: interpreter.outcomes, Succeeded: true}
	for _, id := range w.NodeOrder {
		outcome, err := interpreter.evaluate(id)
		if err != nil {
			return nil, err
		}
		if outcome.Status == Failed {
			prediction.Succeeded = false
		}
	}
	end := interpreter.outcomes["__end"]
	if prediction.Succeeded {
		prediction.Result = end.Value
	}
	return prediction, nil
}

type interpreter struct {
	workflow *Workflow
	outcomes map[string]*Outcome
}

func (in *interpreter) evaluate(id string) (*Outcome, error) {
	if outcome, done := in.outcomes[id]; done {
		return outcome, nil
	}
	outcome, err := in.compute(in.workflow.Nodes[id])
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", id, err)
	}
	in.outcomes[id] = outcome
	return outcome, nil
}

func (in *interpreter) compute(node *Node) (*Outcome, error) {
	if node.Kind == spec.NodeKindStart {
		return &Outcome{Value: in.workflow.Input}, nil
	}
	// Requirements are ordered outermost first. An inner decision only runs
	// when every enclosing case was selected, so the first unselected case
	// is the reason a node is skipped.
	for _, requirement := range node.Context {
		decision, err := in.evaluate(requirement.Decision)
		if err != nil {
			return nil, err
		}
		switch {
		case decision.Status == Blocked:
			return &Outcome{Status: Blocked}, nil
		case decision.Status == Skipped:
			return nil, fmt.Errorf("decision %s is skipped inside an active context", requirement.Decision)
		case decision.SelectedCase != requirement.Case:
			return &Outcome{Status: Skipped, Skip: requirement}, nil
		}
	}
	if node.Kind == spec.NodeKindSelect {
		decision, err := in.evaluate(node.DecisionRef)
		if err != nil {
			return nil, err
		}
		if decision.Status != Succeeded {
			return &Outcome{Status: Blocked}, nil
		}
		source, err := in.evaluate(node.SelectSources[decision.SelectedCase])
		if err != nil {
			return nil, err
		}
		if source.Status != Succeeded {
			return &Outcome{Status: Blocked}, nil
		}
		return &Outcome{Value: source.Value}, nil
	}

	values := make([]json.RawMessage, 0, len(node.Sources))
	for _, sourceID := range node.Sources {
		source, err := in.evaluate(sourceID)
		if err != nil {
			return nil, err
		}
		switch source.Status {
		case Failed, Blocked:
			return &Outcome{Status: Blocked}, nil
		case Skipped:
			return nil, fmt.Errorf("active node reads skipped source %s", sourceID)
		}
		values = append(values, source.Value)
	}
	input := []byte(values[0])
	if node.Merge {
		var err error
		if input, err = canonical.Marshal(values); err != nil {
			return nil, err
		}
	}

	switch node.Kind {
	case spec.NodeKindEnd:
		return &Outcome{Value: input}, nil
	case spec.NodeKindDecision:
		var classified struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(input, &classified); err != nil {
			return nil, err
		}
		return &Outcome{Input: input, Value: input, SelectedCase: classified.Kind}, nil
	case spec.NodeKindStep:
		outcome := &Outcome{Input: input}
		outcome.Attempts, outcome.Status = in.attempts(node, -1)
		if outcome.Status == Succeeded {
			var err error
			if outcome.Value, err = in.workflow.Behaviors[node.Symbol].Evaluate(node.Symbol, input); err != nil {
				return nil, err
			}
		}
		return outcome, nil
	case spec.NodeKindMap:
		return in.mapOutcome(node, input)
	default:
		return nil, fmt.Errorf("unsupported node kind %q", node.Kind)
	}
}

// attempts applies the retry contract to a scripted fault sequence.
func (in *interpreter) attempts(node *Node, item int) ([]Fault, Status) {
	var attempts []Fault
	for attempt := 1; ; attempt++ {
		fault := in.workflow.Fault(Invocation{NodeID: node.ID, Item: item, Attempt: attempt})
		attempts = append(attempts, fault)
		if fault == Succeed {
			return attempts, Succeeded
		}
		if !fault.Retryable() || attempt == node.MaxAttempts {
			return attempts, Failed
		}
	}
}

// mapOutcome dispatches items in rounds. A terminal item failure fails the
// map, so siblings awaiting a retry in that round are not retried. A map that
// collects item failures instead keeps each collectable failure as that
// item's outcome and lets its siblings finish.
func (in *interpreter) mapOutcome(node *Node, input []byte) (*Outcome, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, err
	}
	behavior := in.workflow.Behaviors[node.Symbol]
	outcome := &Outcome{Input: input, Items: make([]ItemOutcome, len(items))}
	pending := make([]int, len(items))
	for index, item := range items {
		pending[index] = index
		outcome.Items[index].Input = item
	}
	for attempt := 1; len(pending) > 0; attempt++ {
		var retry []int
		terminal := false
		for _, index := range pending {
			item := &outcome.Items[index]
			fault := in.workflow.Fault(Invocation{NodeID: node.ID, Item: index, Attempt: attempt})
			item.Attempts = append(item.Attempts, fault)
			switch {
			case fault == Succeed:
				value, err := behavior.Evaluate(node.Symbol, item.Input)
				if err != nil {
					return nil, err
				}
				item.Value = value
			case fault.Retryable() && attempt < node.MaxAttempts:
				retry = append(retry, index)
			default:
				item.Status = Failed
				if node.CollectItemFailures {
					if item.Failure = collectedFailure(fault, attempt, node.TimeoutSeconds); item.Failure != nil {
						continue
					}
				}
				terminal = true
			}
		}
		if terminal {
			for _, index := range retry {
				outcome.Items[index].Status = Failed
			}
			retry = nil
		}
		pending = retry
	}
	values := make([]any, len(items))
	for index, item := range outcome.Items {
		switch {
		case item.Status == Failed && item.Failure == nil:
			outcome.Status = Failed
		case !node.CollectItemFailures:
			values[index] = json.RawMessage(item.Value)
		case item.Failure != nil:
			values[index] = map[string]any{"status": "failed", "failure": item.Failure}
		default:
			values[index] = map[string]any{"status": "succeeded", "value": json.RawMessage(item.Value)}
		}
	}
	if outcome.Status == Succeeded {
		var err error
		if outcome.Value, err = canonical.Marshal(values); err != nil {
			return nil, err
		}
	}
	return outcome, nil
}
