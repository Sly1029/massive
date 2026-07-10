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
	// SourceManifests carries the per-package file manifest (relative path +
	// sha256 content hash) from the compiled spec, keyed by package id. The
	// plan proto records only the package hash, so the orchestrator threads the
	// manifest separately to verify on-disk source against it before running.
	SourceManifests map[string]SourcePackageManifest
	StepInvoker     StepInvoker
	Hooks           RunHooks
}

// SourcePackageManifest is the compiled spec's view of one source package: the
// absolute root its files are resolved against and the per-file content
// manifest. Root may be empty, in which case RunConfig.SourcePackageRoot is the
// fallback.
type SourcePackageManifest struct {
	Root  string
	Files []SourcePackageFile
}

type SourcePackageFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
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
