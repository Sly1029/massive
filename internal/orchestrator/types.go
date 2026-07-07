package orchestrator

import (
	"context"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
)

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

const SourceDirectoryContentType = "application/vnd.massive.source-directory+json"

type RunConfig struct {
	Plan              *planpb.WorkflowPlan
	DatastoreRoot     string
	ProjectID         string
	RunID             string
	RunnerCommand     []string
	RunnerWorkingDir  string
	SourcePackageRoot string
	StepInvoker       StepInvoker
	Hooks             RunHooks
}

type RunHooks struct {
	AfterStepInvocation func(context.Context, StepInvocationDescriptor) error
}

type RunResult struct {
	RunID       string
	ProjectKey  string
	Status      string
	ManifestKey string
	ResultKey   string
	Steps       []StepSummary
}

type StepSummary struct {
	NodeID     string
	Status     string
	Diagnostic string
}

type RunError struct {
	StepID     string
	Diagnostic string
	Result     *RunResult
}

func (e *RunError) Error() string {
	if e.StepID == "" {
		return e.Diagnostic
	}
	return fmt.Sprintf("step %s failed: %s", e.StepID, e.Diagnostic)
}

type StepInvoker interface {
	InvokeSteps(context.Context, StepInvocationBatch) ([]StepInvocationOutcome, error)
}

type StepInvocationBatch struct {
	Steps []StepInvocation
}

type StepInvocation struct {
	Descriptor StepInvocationDescriptor
}

type StepInvocationOutcome struct {
	NodeID             string
	Attempt            int
	Status             string
	ExitCode           int
	Diagnostic         string
	ExpectedOutputHash string
}
