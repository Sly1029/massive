package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/mapexec"
)

// Runner exit codes shared by every language adapter.
const (
	runnerExitDescriptorResolution = 64
	runnerExitSchemaValidation     = 65
	runnerExitStepExecution        = 66
	runnerExitNonRetryable         = 67
	runnerExitInfrastructure       = 70
)

// executionPolicy is the executor view of a contract's retry and timeout
// intent. A contract without retry runs once; one without timeoutSeconds has
// no per-attempt deadline.
type executionPolicy struct {
	maxAttempts   int
	delay         time.Duration
	backoffFactor int
	maxDelay      time.Duration
	timeout       time.Duration
}

func policyForContract(contract *planpb.ExecutionContract) executionPolicy {
	policy := executionPolicy{maxAttempts: 1, backoffFactor: 1}
	if retry := contract.GetRetry(); retry != nil && retry.GetMaxAttempts() > 0 {
		policy.maxAttempts = int(retry.GetMaxAttempts())
		policy.delay = time.Duration(retry.GetDelaySeconds()) * time.Second
		policy.backoffFactor = max(int(retry.GetBackoffFactor()), 1)
		policy.maxDelay = time.Duration(retry.GetMaxDelaySeconds()) * time.Second
	}
	policy.timeout = time.Duration(contract.GetTimeoutSeconds()) * time.Second
	return policy
}

// delayBefore returns min(delay * backoffFactor^(attempt-2), maxDelay) for a
// retry attempt (attempt >= 2).
func (policy executionPolicy) delayBefore(attempt int) time.Duration {
	delay := policy.delay
	for range attempt - 2 {
		if delay >= policy.maxDelay {
			break
		}
		delay *= time.Duration(policy.backoffFactor)
	}
	return min(delay, policy.maxDelay)
}

// retryableOutcome admits failures that another attempt may fix: author
// exceptions, timeouts, and runner crashes. Descriptor and schema failures are
// deterministic contract violations, and authors opt out explicitly with the
// non-retryable exit. Cancellation and infrastructure failures are never
// reported as retryable outcomes.
func retryableOutcome(outcome StepInvocationOutcome) bool {
	if outcome.Status != StatusFailed {
		return false
	}
	switch outcome.ExitCode {
	case runnerExitDescriptorResolution, runnerExitSchemaValidation, runnerExitNonRetryable, FatalRunExitCode, runnerExitInfrastructure:
		return false
	}
	return true
}

// mapItemFailure classifies a terminal runner failure that a collecting map
// keeps as an item outcome. Descriptor and schema failures violate the plan's
// contract rather than describe one item, so they still fail the map, as do
// cancellation, infrastructure, and output verification failures, which have
// no runner outcome to classify.
func mapItemFailure(outcome StepInvocationOutcome, attempt int) (mapexec.Failure, bool) {
	summary := runnerFailureSummary(outcome, "runner-failure")
	switch {
	case outcome.TimedOutAfter > 0:
		return mapexec.NewFailure(mapexec.FailureTimeout, attempt, summary), true
	case outcome.ExitCode == runnerExitDescriptorResolution || outcome.ExitCode == runnerExitSchemaValidation || outcome.ExitCode == runnerExitInfrastructure || outcome.ExitCode == FatalRunExitCode:
		return mapexec.Failure{}, false
	case outcome.ExitCode == runnerExitNonRetryable:
		return mapexec.NewFailure(mapexec.FailureNonRetryable, attempt, withRunnerMessage(summary, outcome.RunnerMessage)), true
	case outcome.ExitCode == runnerExitStepExecution:
		return mapexec.NewFailure(mapexec.FailureError, attempt, withRunnerMessage(summary, outcome.RunnerMessage)), true
	// The process owner reports signal termination as a negative exit code.
	// Positive exits, including 128+signal, are ordinary process exits.
	case outcome.ExitCode < 0:
		return mapexec.NewFailure(mapexec.FailureKilled, attempt, summary), true
	default:
		return mapexec.NewFailure(mapexec.FailureError, attempt, summary), true
	}
}

// withRunnerMessage uses only the separately validated runner result. Captured
// author logs never supply a failure message.
func withRunnerMessage(summary, message string) string {
	if message == "" {
		return summary
	}
	return summary + ": " + message
}

func waitBeforeRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// attemptSuffix names the attempt in caller diagnostics once retries exist.
func attemptSuffix(attempt int, policy executionPolicy) string {
	if policy.maxAttempts <= 1 {
		return ""
	}
	return fmt.Sprintf(" (attempt %d of %d)", attempt, policy.maxAttempts)
}
