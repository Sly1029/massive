package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/artifact"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/graphgen"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/runjournal"
	"github.com/Sly1029/massive/internal/spec"
)

// FuzzGeneratedGraphExecution runs generated workflows through Run and
// compares each journal with an independent reference interpreter. Steps run
// in process behind the real StepInvoker boundary: the executor reads inputs
// from the datastore, validates them against plan schemas, evaluates the
// generated symbol, and publishes through the artifact runtime.
func FuzzGeneratedGraphExecution(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 3, 3, 2, 1, 0, 2, 5, 1, 4, 2, 9, 1, 0, 3, 3, 1, 2, 6, 4, 1})
	f.Add([]byte{1, 0, 5, 2, 0, 3, 5, 5, 1, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 1, 2, 3})
	f.Add([]byte{2, 1, 6, 30, 0, 3, 4, 2, 2, 1, 1, 5, 0, 1, 1, 3, 2, 0, 0, 1, 0, 0, 0, 1, 2, 3, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 512 {
			t.Skip()
		}
		executeGenerated(t, data)
	})
}

func executeGenerated(t *testing.T, data []byte) {
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
	prediction, err := workflow.Predict()
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, workflow.SourcePath), workflow.Source, 0o644); err != nil {
		t.Fatal(err)
	}
	storeRoot := newStoreRoot(t)
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	executor := &generatedExecutor{workflow: workflow, store: store, cancel: cancel, slots: map[string]bool{}, exports: map[string]string{}}
	for ref, symbol := range parsed.Symbols {
		executor.exports[symbol.Export] = ref
	}
	result, runErr := Run(ctx, RunConfig{
		Plan: compiled.Plan, DatastoreRoot: storeRoot, ProjectID: "fuzz/generated", RunID: "generated",
		SourcePackageRoot: sourceRoot, SourceManifests: manifestsFromSpec(parsed), StepInvoker: executor,
	}, workflow.Input)
	if executor.err != nil {
		t.Fatalf("executor rejected an invocation: %v", executor.err)
	}
	if result == nil {
		t.Fatalf("run returned no result: %v", runErr)
	}
	oracle := &executionOracle{t: t, workflow: workflow, prediction: prediction, storeRoot: storeRoot, executor: executor}
	oracle.check(result, runErr)
}

var errInfrastructure = errors.New("generated executor infrastructure failure")

type executedInvocation struct {
	invocation graphgen.Invocation
	fault      graphgen.Fault
	// published is the output manifest key the executor committed, if any.
	published string
	status    string
}

type generatedExecutor struct {
	workflow *graphgen.Workflow
	store    datastore.Datastore
	cancel   context.CancelCauseFunc
	exports  map[string]string
	// mu guards the mutable state below, so a concurrent dispatcher such as
	// a run-wide worker budget cannot race the executor's bookkeeping.
	mu      sync.Mutex
	batches int
	// fired records an interrupt that actually took effect.
	fired *graphgen.InterruptKind
	slots map[string]bool
	log   []executedInvocation
	err   error
}

func (e *generatedExecutor) InvokeSteps(ctx context.Context, batch StepInvocationBatch) ([]StepInvocationOutcome, error) {
	e.mu.Lock()
	batchIndex := e.batches
	e.batches++
	e.mu.Unlock()
	interrupt := e.workflow.Interrupt
	if interrupt == nil || interrupt.Batch != batchIndex {
		interrupt = nil
	}
	if interrupt != nil && interrupt.Kind == graphgen.CancelBefore {
		e.fire(interrupt.Kind)
		return nil, ctx.Err()
	}
	outcomes := make([]StepInvocationOutcome, 0, len(batch.Steps))
	for position, step := range batch.Steps {
		descriptor := step.Descriptor
		if interrupt != nil && interrupt.Position == position {
			switch interrupt.Kind {
			case graphgen.CancelDuring:
				e.fire(interrupt.Kind)
				outcomes = append(outcomes, StepInvocationOutcome{NodeID: descriptor.NodeID, Attempt: descriptor.Attempt, Scope: descriptor.Scope, Status: StatusCancelled, ExitCode: -1})
				return outcomes, ctx.Err()
			case graphgen.InfrastructureFailure:
				e.mu.Lock()
				e.fired = &interrupt.Kind
				e.mu.Unlock()
				outcomes = append(outcomes, StepInvocationOutcome{NodeID: descriptor.NodeID, Attempt: descriptor.Attempt, Scope: descriptor.Scope, Status: stepInvocationStatusInfraFailed, ExitCode: 1})
				return outcomes, errInfrastructure
			}
		}
		outcome, err := e.invoke(ctx, step, batch.MaxConcurrency)
		if err != nil {
			e.mu.Lock()
			e.err = errors.Join(e.err, err)
			e.mu.Unlock()
			return outcomes, err
		}
		outcomes = append(outcomes, outcome)
	}
	if interrupt != nil && (interrupt.Kind == graphgen.CancelAfter || interrupt.Kind == graphgen.CancelDuring) {
		e.fire(interrupt.Kind)
		return outcomes, ctx.Err()
	}
	return outcomes, nil
}

func (e *generatedExecutor) fire(kind graphgen.InterruptKind) {
	e.mu.Lock()
	e.fired = &kind
	e.mu.Unlock()
	e.cancel(errors.New("generated interrupt"))
}

// invoke implements the runner side of one descriptor.
func (e *generatedExecutor) invoke(ctx context.Context, step StepInvocation, maxConcurrency int) (StepInvocationOutcome, error) {
	descriptor := step.Descriptor
	node := e.workflow.Nodes[descriptor.NodeID]
	if node == nil || (node.Kind != spec.NodeKindStep && node.Kind != spec.NodeKindMap) {
		return StepInvocationOutcome{}, fmt.Errorf("descriptor for non-executable node %q", descriptor.NodeID)
	}
	item := -1
	if descriptor.Scope != nil {
		frames := descriptor.Scope.Frames
		if node.Kind != spec.NodeKindMap || len(frames) != 1 || frames[0].Kind != "map-item" || frames[0].MapID != node.ID {
			return StepInvocationOutcome{}, fmt.Errorf("descriptor %s has scope %#v", node.ID, descriptor.Scope)
		}
		item = frames[0].Index
	} else if node.Kind == spec.NodeKindMap {
		return StepInvocationOutcome{}, fmt.Errorf("map %s dispatched without an item scope", node.ID)
	}
	wantConcurrency := 0
	if node.Kind == spec.NodeKindMap {
		wantConcurrency = int(node.MaxConcurrency)
	}
	if maxConcurrency != wantConcurrency {
		return StepInvocationOutcome{}, fmt.Errorf("%s batch maxConcurrency = %d, want %d", node.ID, maxConcurrency, wantConcurrency)
	}
	if descriptor.MaxAttempts != node.MaxAttempts || descriptor.Attempt < 1 || descriptor.Attempt > descriptor.MaxAttempts {
		return StepInvocationOutcome{}, fmt.Errorf("%s attempt %d of %d, contract allows %d", node.ID, descriptor.Attempt, descriptor.MaxAttempts, node.MaxAttempts)
	}
	if want := time.Duration(node.TimeoutSeconds) * time.Second; step.Timeout != want {
		return StepInvocationOutcome{}, fmt.Errorf("%s attempt timeout = %s, want %s", node.ID, step.Timeout, want)
	}
	e.mu.Lock()
	reused := e.slots[descriptor.Output.ManifestKey]
	e.slots[descriptor.Output.ManifestKey] = true
	e.mu.Unlock()
	if reused {
		return StepInvocationOutcome{}, fmt.Errorf("%s reused output slot %s", node.ID, descriptor.Output.ManifestKey)
	}
	symbol := e.exports[descriptor.Symbol.Export]
	if symbol != node.Symbol {
		return StepInvocationOutcome{}, fmt.Errorf("%s dispatched symbol %q, want %q", node.ID, symbol, node.Symbol)
	}

	input, err := e.store.Get(ctx, datastore.MustKey(descriptor.Input.Artifact.Key))
	if err != nil {
		return StepInvocationOutcome{}, err
	}
	if canonical.DigestBytes(input.Body) != descriptor.Input.Artifact.Hash {
		return StepInvocationOutcome{}, fmt.Errorf("%s input hash does not match its descriptor", node.ID)
	}
	schemaKey, err := blobKeyForHash(descriptor.Input.Schema)
	if err != nil {
		return StepInvocationOutcome{}, err
	}
	schema, err := e.store.Get(ctx, schemaKey)
	if err != nil {
		return StepInvocationOutcome{}, err
	}
	if err := validateJSONAgainstSchema(string(schema.Body), input.Body); err != nil {
		return StepInvocationOutcome{}, fmt.Errorf("%s input violates its declared schema: %w", node.ID, err)
	}

	invocation := graphgen.Invocation{NodeID: node.ID, Item: item, Attempt: descriptor.Attempt}
	fault := e.workflow.Fault(invocation)
	record := executedInvocation{invocation: invocation, fault: fault, status: StatusFailed}
	outcome := StepInvocationOutcome{NodeID: node.ID, Attempt: descriptor.Attempt, Scope: descriptor.Scope, Status: StatusFailed, Diagnostic: "SECRET runner output for " + node.ID}
	if fault == graphgen.Succeed || fault == graphgen.OrphanFailure {
		output, err := e.workflow.Behaviors[symbol].Evaluate(symbol, input.Body)
		if err != nil {
			return StepInvocationOutcome{}, err
		}
		manifestKey, err := datastore.ParseKey(descriptor.Output.ManifestKey)
		if err != nil {
			return StepInvocationOutcome{}, err
		}
		if _, err := artifact.PublishJSON(ctx, e.store, artifact.Destination{ManifestKey: manifestKey, Schema: descriptor.Output.Schema}, artifact.Producer{
			ProjectKey: descriptor.ProjectKey, PlanHash: descriptor.PlanHash, RunID: descriptor.RunID,
			NodeID: descriptor.NodeID, Attempt: descriptor.Attempt, Scope: descriptor.Scope,
		}, output); err != nil {
			return StepInvocationOutcome{}, fmt.Errorf("%s publish output: %w", node.ID, err)
		}
		record.published = descriptor.Output.ManifestKey
	}
	switch fault {
	case graphgen.Succeed, graphgen.MissingOutput:
		outcome.Status, outcome.Diagnostic = StatusSucceeded, ""
	case graphgen.FailRetryable, graphgen.OrphanFailure:
		outcome.ExitCode = graphgen.ExitStepExecution
	case graphgen.Crash:
		outcome.ExitCode = graphgen.ExitKilled
	case graphgen.FailNonRetryable:
		outcome.ExitCode = graphgen.ExitNonRetryable
	case graphgen.FailSchema:
		outcome.ExitCode = graphgen.ExitSchemaValidation
	case graphgen.Timeout:
		outcome.ExitCode, outcome.TimedOutAfter = -1, step.Timeout
	}
	record.status = outcome.Status
	e.mu.Lock()
	e.log = append(e.log, record)
	e.mu.Unlock()
	return outcome, nil
}

type executionOracle struct {
	t          *testing.T
	workflow   *graphgen.Workflow
	prediction *graphgen.Prediction
	storeRoot  string
	executor   *generatedExecutor
	journal    runjournal.Manifest
}

func (o *executionOracle) check(result *RunResult, runErr error) {
	t := o.t
	t.Helper()
	body := getObject(t, o.storeRoot, result.ManifestKey).Body
	parsed, err := runjournal.Parse(body)
	if err != nil {
		t.Fatalf("published journal is invalid: %v\n%s", err, body)
	}
	o.journal = *parsed
	if encoded, err := canonical.Marshal(parsed); err != nil || !bytes.Equal(encoded, body) {
		t.Fatalf("journal does not round trip through runjournal.Parse: %v", err)
	}
	if bytes.Contains(body, []byte("SECRET")) {
		t.Fatal("journal persisted runner output")
	}
	if result.Status != o.journal.Status {
		t.Fatalf("result status %s, journal status %s", result.Status, o.journal.Status)
	}
	o.checkTerminalStatus(runErr)
	o.checkSteps()
	o.checkDecisions()
	o.checkExecutorLog()
	o.checkDependencies()
	if o.journal.Status == StatusSucceeded {
		result := getObject(t, o.storeRoot, o.journal.Result.Key).Body
		if !bytes.Equal(result, o.prediction.Result) {
			t.Fatalf("result = %s, want %s", result, o.prediction.Result)
		}
	}
}

func (o *executionOracle) checkTerminalStatus(runErr error) {
	t := o.t
	fired := o.executor.fired
	var runError *RunError
	switch {
	case fired != nil && *fired != graphgen.InfrastructureFailure:
		// Settling a success reported as cancellation arrived can still
		// expose a genuine failure, such as a missing output.
		genuine := errors.As(runErr, &runError) && o.prediction.Nodes[runError.StepID] != nil && o.prediction.Nodes[runError.StepID].Status == graphgen.Failed
		if o.journal.Status == StatusFailed && genuine {
			break
		}
		if o.journal.Status != StatusCancelled || !errors.Is(runErr, context.Canceled) {
			t.Fatalf("interrupted run status %s error %v, want cancelled", o.journal.Status, runErr)
		}
	case fired != nil:
		if o.journal.Status != StatusFailed || !errors.Is(runErr, errInfrastructure) {
			t.Fatalf("infrastructure failure status %s error %v", o.journal.Status, runErr)
		}
	case o.prediction.Succeeded:
		if o.journal.Status != StatusSucceeded || runErr != nil {
			t.Fatalf("run status %s error %v, want success", o.journal.Status, runErr)
		}
		return
	default:
		if o.journal.Status != StatusFailed || !errors.As(runErr, &runError) {
			t.Fatalf("run status %s error %v, want failure", o.journal.Status, runErr)
		}
		var failed []string
		for _, step := range o.journal.Steps {
			if step.Status == StatusFailed {
				failed = append(failed, step.NodeID)
			}
		}
		if len(failed) != 1 || runError.StepID != failed[0] || o.prediction.Nodes[failed[0]].Status != graphgen.Failed {
			t.Fatalf("failed steps %v attributed to %q", failed, runError.StepID)
		}
	}
	if !strings.HasPrefix(o.journal.Diagnostic, "run "+o.journal.Status) {
		t.Fatalf("root diagnostic %q", o.journal.Diagnostic)
	}
}

func (o *executionOracle) checkSteps() {
	t := o.t
	seen := map[string]bool{}
	for _, step := range o.journal.Steps {
		node := o.workflow.Nodes[step.NodeID]
		predicted := o.prediction.Nodes[step.NodeID]
		if node == nil || seen[step.NodeID] || (node.Kind != "step" && node.Kind != "map") {
			t.Fatalf("journal step %q is not a unique executable node", step.NodeID)
		}
		seen[step.NodeID] = true
		if step.Status == StatusSkipped {
			want := runjournal.SkipReason{Kind: "decision-not-selected", DecisionID: predicted.Skip.Decision, Case: predicted.Skip.Case}
			if predicted.Status != graphgen.Skipped || step.SkipReason == nil || *step.SkipReason != want || len(step.Attempts) != 0 {
				t.Fatalf("step %s skipped with %+v, prediction %+v", step.NodeID, step.SkipReason, predicted)
			}
			continue
		}
		if predicted.Status == graphgen.Skipped && step.Status != StatusNotStarted {
			t.Fatalf("unselected step %s has status %s", step.NodeID, step.Status)
		}
		if o.journal.Status == StatusSucceeded && step.Status != StatusSucceeded {
			t.Fatalf("successful run has step %s %s", step.NodeID, step.Status)
		}
		if step.Status == StatusNotStarted && len(step.Attempts) != 0 {
			t.Fatalf("not-started step %s has attempts", step.NodeID)
		}
		if node.Kind == "step" {
			o.checkAttempts(step.NodeID, node, step.Attempts, predicted.Attempts, predicted.Input, predicted.Value, step.Status, false)
			continue
		}
		o.checkMap(step, node, predicted)
	}
	for _, id := range o.workflow.NodeOrder {
		if kind := o.workflow.Nodes[id].Kind; (kind == "step" || kind == "map") && !seen[id] {
			t.Fatalf("journal omits executable node %s", id)
		}
	}
}

// checkAttempts compares a settled entry with its predicted fault sequence.
// Entries ended by an interrupt are checked for bounds and slot identity only.
func (o *executionOracle) checkAttempts(id string, node *graphgen.Node, attempts []runjournal.Attempt, faults []graphgen.Fault, input, value []byte, status string, item bool) {
	t := o.t
	if len(attempts) > node.MaxAttempts {
		t.Fatalf("%s has %d attempts, contract allows %d", id, len(attempts), node.MaxAttempts)
	}
	for index, attempt := range attempts {
		if attempt.Output != nil && !strings.HasSuffix(attempt.Output.Manifest.Key, fmt.Sprintf("/%d/output-manifest.json", attempt.Attempt)) {
			t.Fatalf("%s attempt %d published to slot %s", id, attempt.Attempt, attempt.Output.Manifest.Key)
		}
		if attempt.Input.Hash != canonical.DigestBytes(input) {
			t.Fatalf("%s attempt %d received input %s, want %s", id, index+1, attempt.Input.Hash, input)
		}
	}
	settled := status == StatusSucceeded || (status == StatusFailed && o.executor.fired == nil)
	if !settled {
		return
	}
	if len(attempts) != len(faults) {
		t.Fatalf("%s has %d attempts, prediction %v", id, len(attempts), faults)
	}
	for index, attempt := range attempts {
		fault := faults[index]
		if fault == graphgen.Succeed {
			if attempt.Status != StatusSucceeded || attempt.Output == nil {
				t.Fatalf("%s attempt %d = %+v, want success", id, index+1, attempt)
			}
			body := getObject(t, o.storeRoot, attempt.Output.Body.Key).Body
			if !bytes.Equal(body, value) {
				t.Fatalf("%s output %s, want %s", id, body, value)
			}
			continue
		}
		if want := expectedDiagnostic(fault, node.TimeoutSeconds, item); attempt.Status != StatusFailed || attempt.Diagnostic != want || attempt.Output != nil {
			t.Fatalf("%s attempt %d = %+v, want failure %q", id, index+1, attempt, want)
		}
	}
}

func expectedDiagnostic(fault graphgen.Fault, timeoutSeconds uint32, item bool) string {
	switch fault {
	case graphgen.FailRetryable, graphgen.OrphanFailure:
		return "step-execution-failure (exit 66)"
	case graphgen.Crash:
		return "runner-failure (exit 137)"
	case graphgen.FailNonRetryable:
		return "non-retryable-step-failure (exit 67)"
	case graphgen.FailSchema:
		return "schema-validation-failure (exit 65)"
	case graphgen.Timeout:
		return fmt.Sprintf("step-timeout (timed out after %s)", time.Duration(timeoutSeconds)*time.Second)
	case graphgen.MissingOutput:
		if item {
			return "map item output verification failed"
		}
		return "output verification failed"
	}
	return ""
}

func (o *executionOracle) checkMap(step runjournal.Step, node *graphgen.Node, predicted *graphgen.Outcome) {
	t := o.t
	items := *step.Items
	if len(step.Attempts) > 1 {
		t.Fatalf("map %s has %d collection attempts", step.NodeID, len(step.Attempts))
	}
	if len(step.Attempts) == 1 && step.Attempts[0].Input.Hash != canonical.DigestBytes(predicted.Input) {
		t.Fatalf("map %s received the wrong input", step.NodeID)
	}
	if len(items) != 0 && len(items) != len(predicted.Items) {
		t.Fatalf("map %s has %d items, want %d", step.NodeID, len(items), len(predicted.Items))
	}
	if step.Status == StatusSucceeded {
		if len(items) != len(predicted.Items) {
			t.Fatalf("successful map %s has %d items, want %d", step.NodeID, len(items), len(predicted.Items))
		}
		body := getObject(t, o.storeRoot, step.Attempts[0].Output.Body.Key).Body
		if !bytes.Equal(body, predicted.Value) {
			t.Fatalf("map %s collected %s, want %s", step.NodeID, body, predicted.Value)
		}
	}
	for index, item := range items {
		want := predicted.Items[index]
		id := fmt.Sprintf("%s#%d", step.NodeID, index)
		if item.Index != index {
			t.Fatalf("map %s item indexes are not dense: %d at %d", step.NodeID, item.Index, index)
		}
		if item.Status == StatusSucceeded && want.Status != graphgen.Succeeded {
			t.Fatalf("item %s succeeded, prediction %+v", id, want)
		}
		if item.Status == StatusNotStarted && len(item.Attempts) != 0 {
			t.Fatalf("not-started item %s has attempts", id)
		}
		if step.Status == StatusSucceeded || (step.Status == StatusFailed && o.executor.fired == nil) {
			wantStatus := StatusSucceeded
			if want.Status == graphgen.Failed {
				wantStatus = StatusFailed
			}
			if item.Status != wantStatus {
				t.Fatalf("item %s is %s, want %s", id, item.Status, wantStatus)
			}
		}
		o.checkAttempts(id, node, item.Attempts, want.Attempts, want.Input, want.Value, item.Status, true)
	}
	if step.Status == StatusFailed && o.executor.fired == nil && predicted.Status != graphgen.Failed {
		t.Fatalf("map %s failed, prediction %+v", step.NodeID, predicted)
	}
}

func (o *executionOracle) checkDecisions() {
	t := o.t
	seen := map[string]bool{}
	for _, decision := range o.journal.Decisions {
		predicted := o.prediction.Nodes[decision.NodeID]
		if predicted == nil || seen[decision.NodeID] {
			t.Fatalf("journal decision %q is not a unique decision node", decision.NodeID)
		}
		seen[decision.NodeID] = true
		switch decision.Status {
		case "selected":
			if predicted.Status != graphgen.Succeeded || decision.SelectedCase != predicted.SelectedCase {
				t.Fatalf("decision %s selected %q, prediction %+v", decision.NodeID, decision.SelectedCase, predicted)
			}
		case StatusSkipped:
			want := runjournal.SkipReason{Kind: "decision-not-selected", DecisionID: predicted.Skip.Decision, Case: predicted.Skip.Case}
			if predicted.Status != graphgen.Skipped || decision.SkipReason == nil || *decision.SkipReason != want {
				t.Fatalf("decision %s skipped with %+v, prediction %+v", decision.NodeID, decision.SkipReason, predicted)
			}
		default:
			t.Fatalf("decision %s has status %s", decision.NodeID, decision.Status)
		}
	}
	if o.journal.Status != StatusSucceeded {
		return
	}
	for _, id := range o.workflow.NodeOrder {
		if o.workflow.Nodes[id].Kind == "decision" && !seen[id] {
			t.Fatalf("successful run did not record decision %s", id)
		}
	}
}

// checkExecutorLog proves that every artifact the executor committed for a
// reported success survives in the journal, that failed attempts never adopt
// an output, and that the journal contains no attempt the executor never ran.
func (o *executionOracle) checkExecutorLog() {
	t := o.t
	attempts := map[graphgen.Invocation]runjournal.Attempt{}
	for _, step := range o.journal.Steps {
		node := o.workflow.Nodes[step.NodeID]
		if step.Items == nil {
			for _, attempt := range step.Attempts {
				attempts[graphgen.Invocation{NodeID: step.NodeID, Item: -1, Attempt: attempt.Attempt}] = attempt
			}
			continue
		}
		for _, item := range *step.Items {
			for _, attempt := range item.Attempts {
				attempts[graphgen.Invocation{NodeID: node.ID, Item: item.Index, Attempt: attempt.Attempt}] = attempt
			}
		}
	}
	logged := map[graphgen.Invocation]bool{}
	for _, record := range o.executor.log {
		logged[record.invocation] = true
		attempt, exists := attempts[record.invocation]
		if !exists {
			t.Fatalf("journal lost executed invocation %+v", record.invocation)
		}
		if record.status == StatusSucceeded && record.published != "" {
			if attempt.Status != StatusSucceeded || attempt.Output == nil || attempt.Output.Manifest.Key != record.published {
				t.Fatalf("completed artifact for %+v was not preserved: %+v", record.invocation, attempt)
			}
		}
		if record.status != StatusSucceeded && attempt.Output != nil {
			t.Fatalf("failed invocation %+v adopted an output", record.invocation)
		}
	}
	cancelledInFlight := o.executor.fired != nil && (*o.executor.fired == graphgen.CancelDuring || *o.executor.fired == graphgen.InfrastructureFailure)
	for invocation, attempt := range attempts {
		if logged[invocation] {
			continue
		}
		// The invocation interrupted in flight has no runner outcome. A static
		// step's collection attempt records the step itself.
		if cancelledInFlight && (attempt.Status == StatusCancelled || attempt.Status == StatusFailed) {
			continue
		}
		if o.workflow.Nodes[invocation.NodeID].Kind == "map" && invocation.Item == -1 {
			continue
		}
		t.Fatalf("journal records invocation %+v the executor never ran: %+v", invocation, attempt)
	}
}

// checkDependencies requires every executed node's value producers to have
// succeeded first, independent of the scheduler's order.
func (o *executionOracle) checkDependencies() {
	t := o.t
	statuses := map[string]string{}
	for _, step := range o.journal.Steps {
		statuses[step.NodeID] = step.Status
	}
	var producers func(string) []string
	producers = func(id string) []string {
		node := o.workflow.Nodes[id]
		switch node.Kind {
		case "step", "map":
			return []string{id}
		case "decision":
			return producers(node.Sources[0])
		case "select":
			return producers(node.SelectSources[o.prediction.Nodes[node.DecisionRef].SelectedCase])
		}
		return nil
	}
	for _, step := range o.journal.Steps {
		if len(step.Attempts) == 0 {
			continue
		}
		for _, source := range o.workflow.Nodes[step.NodeID].Sources {
			for _, producer := range producers(source) {
				if statuses[producer] != StatusSucceeded {
					t.Fatalf("%s ran before its producer %s succeeded (%s)", step.NodeID, producer, statuses[producer])
				}
			}
		}
	}
}
