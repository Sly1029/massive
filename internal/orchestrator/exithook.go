package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/runjournal"
)

// RunOutcome is an exit hook's input on every target. Its JSON schema is
// conformance/schema/run-outcome.schema.json, generated from the Python SDK.
type RunOutcome struct {
	RunID      string  `json:"run_id"`
	Status     string  `json:"status"`
	FailedNode *string `json:"failed_node"`
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at"`
}

// OutcomeTime renders an outcome timestamp in the form both targets use.
func OutcomeTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// runExitHook invokes the plan's exit hook once with the settled run's outcome
// and returns its journal record. The hook's failure is recorded, never
// returned, so it cannot change the run's status. Cancellation of the run does
// not stop the hook; its finite contract timeout and fresh signal context bound it.
func runExitHook(ctx context.Context, store datastore.Datastore, config RunConfig, invoker StepInvoker, index executionIndex, projectKey, runID string, hook *planpb.GraphNode, outcome RunOutcome) runjournal.Step {
	// Subscribe afresh: the CLI's run signal context may already be cancelled.
	ctx, stopSignals := signal.NotifyContext(context.WithoutCancel(ctx), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	policy := policyForContract(index.contractsByRef[hook.GetContractRef()])
	ctx, stop := context.WithTimeout(ctx, policy.timeout)
	defer stop()
	body, encodeErr := canonical.Marshal(outcome)
	attempt := runjournal.Attempt{
		Attempt: 1, Status: StatusFailed,
		Input: runjournal.DataArtifact{
			Key:  runInputKey(projectKey, runID, hook.GetId(), nil).String(),
			Hash: canonical.DigestBytes(body), ContentType: jsonContentType, Schema: hook.GetInputSchema(),
		},
	}
	record := func(diagnostic string) runjournal.Step {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			diagnostic = fmt.Sprintf("exit hook exceeded its %d-second timeout", int64(policy.timeout/time.Second))
		case context.Canceled:
			diagnostic = "exit hook cancelled"
		}
		status := StatusFailed
		if diagnostic == "" {
			status = StatusSucceeded
		}
		attempt.Status, attempt.Diagnostic = status, diagnostic
		return runjournal.Step{NodeID: hook.GetId(), Status: status, Attempts: []runjournal.Attempt{attempt}}
	}
	if encodeErr != nil {
		return record("exit hook input could not be encoded")
	}
	// A run may expire while preparing its sources. The detached hook still
	// needs a verified archive; prepare it under the hook's own lifetime.
	if index.packagesByID == nil {
		packages, err := materializePrerequisites(ctx, store, config)
		if err != nil {
			return record("exit hook source materialization failed")
		}
		index.packagesByID = packages
	}
	if _, err := store.Put(ctx, datastore.MustKey(attempt.Input.Key), body, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return record("exit hook input could not be written")
	}
	descriptor, err := descriptorForStep(config.Plan.GetPlanHash(), LocalDatastoreDescriptor{Kind: "local", Path: config.DatastoreRoot}, projectKey, runID, hook, attempt.Input, index, 1)
	if err != nil {
		return record("exit hook descriptor could not be built")
	}
	outcomes, err := invoker.InvokeSteps(ctx, StepInvocationBatch{Steps: []StepInvocation{{Descriptor: descriptor, Timeout: policy.timeout}}})
	if err != nil || len(outcomes) != 1 {
		return record("invocation infrastructure failed")
	}
	if outcomes[0].Status != StatusSucceeded {
		return record(durableRunnerDiagnostic(outcomes[0]))
	}
	output, err := settleInvocation(ctx, store, descriptor, index, config.Hooks)
	if err != nil {
		return record("output verification failed")
	}
	attempt.Output = &output.Published
	return record("")
}
