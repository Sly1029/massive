package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Sly1029/massive/internal/artifact"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/taskprocess"
)

const (
	descriptorPathToken             = "{descriptor}"
	defaultLocalProcessConcurrency  = 32
	stepInvocationStatusInfraFailed = "infrastructure-failed"
)

// DefaultRunnerCommand selects a language adapter without imposing a different
// language on other nodes in the same portable plan.
func DefaultRunnerCommand(language string) ([]string, error) {
	switch language {
	case "python":
		if python := os.Getenv("MASSIVE_PYTHON"); python != "" {
			return []string{python, "-m", "massive.runner", descriptorPathToken}, nil
		}
		return []string{"massive-python-runner", descriptorPathToken}, nil
	case "typescript":
		runner := os.Getenv("MASSIVE_TYPESCRIPT_RUNNER")
		if runner == "" {
			runner = "massive-typescript-runner"
		}
		return []string{runner, descriptorPathToken}, nil
	default:
		return nil, fmt.Errorf("unsupported runner language %q", language)
	}
}

type ProcessStepInvoker struct {
	CommandTemplate []string
	WorkingDir      string
	DescriptorDir   string
	// ProcessLimit is the executor-owned ceiling applied after a workflow's
	// maxConcurrency. Zero uses a conservative local default.
	ProcessLimit int
}

func (i ProcessStepInvoker) InvokeSteps(ctx context.Context, batch StepInvocationBatch) ([]StepInvocationOutcome, error) {
	if len(batch.Steps) == 0 {
		return nil, nil
	}
	for _, step := range batch.Steps {
		if err := validateDescriptorFileIdentity(step.Descriptor); err != nil {
			return nil, err
		}
		manifestKey, err := datastore.ParseKey(step.Descriptor.Output.ManifestKey)
		if err != nil {
			return nil, fmt.Errorf("invalid descriptor output manifest key: %w", err)
		}
		if err := artifact.ValidateDestination(
			artifact.Destination{ManifestKey: manifestKey, Schema: step.Descriptor.Output.Schema},
			artifact.Producer{
				ProjectKey: step.Descriptor.ProjectKey,
				PlanHash:   step.Descriptor.PlanHash,
				RunID:      step.Descriptor.RunID,
				NodeID:     step.Descriptor.NodeID,
				Attempt:    step.Descriptor.Attempt,
				Scope:      step.Descriptor.Scope,
			},
		); err != nil {
			return nil, fmt.Errorf("invalid descriptor output destination: %w", err)
		}
	}

	descriptorDir := i.DescriptorDir
	var cleanup func()
	if descriptorDir == "" {
		created, err := os.MkdirTemp("", "massive-step-descriptors-*")
		if err != nil {
			return nil, fmt.Errorf("create descriptor directory: %w", err)
		}
		descriptorDir = created
		cleanup = func() { _ = os.RemoveAll(created) }
	} else if err := os.MkdirAll(descriptorDir, 0o755); err != nil {
		return nil, fmt.Errorf("create descriptor directory %q: %w", descriptorDir, err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	maxConcurrency := batch.MaxConcurrency
	if maxConcurrency <= 0 || maxConcurrency > len(batch.Steps) {
		maxConcurrency = len(batch.Steps)
	}
	processLimit := i.ProcessLimit
	if processLimit <= 0 {
		processLimit = defaultLocalProcessConcurrency
	}
	if maxConcurrency > processLimit {
		maxConcurrency = processLimit
	}
	batchContext, cancel := context.WithCancel(ctx)
	defer cancel()
	outcomes := make([]StepInvocationOutcome, len(batch.Steps))
	started := make([]bool, len(batch.Steps))
	jobs := make(chan int)
	var group sync.WaitGroup
	var firstError error
	var firstErrorOnce sync.Once
	for range maxConcurrency {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-batchContext.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					if batchContext.Err() != nil {
						return
					}
					started[index] = true
					outcome, err := i.invokeOne(batchContext, descriptorDir, batch.Steps[index].Descriptor)
					outcomes[index] = outcome
					if err != nil {
						firstErrorOnce.Do(func() {
							firstError = err
							cancel()
						})
					}
				}
			}
		}()
	}
	dispatching := true
	for index := range batch.Steps {
		select {
		case jobs <- index:
		case <-batchContext.Done():
			dispatching = false
		}
		if !dispatching {
			break
		}
	}
	close(jobs)
	group.Wait()
	completed := make([]StepInvocationOutcome, 0, len(batch.Steps))
	for index, wasStarted := range started {
		if wasStarted {
			completed = append(completed, outcomes[index])
		}
	}
	if firstError != nil {
		return completed, firstError
	}
	if err := ctx.Err(); err != nil {
		return completed, err
	}
	return completed, nil
}

func (i ProcessStepInvoker) invokeOne(ctx context.Context, descriptorDir string, descriptor StepInvocationDescriptor) (StepInvocationOutcome, error) {
	infrastructureFailure := func(err error) (StepInvocationOutcome, error) {
		return StepInvocationOutcome{
			NodeID:   descriptor.NodeID,
			Attempt:  descriptor.Attempt,
			Scope:    descriptor.Scope,
			Status:   stepInvocationStatusInfraFailed,
			ExitCode: 1,
		}, err
	}
	descriptorBytes, err := canonical.Marshal(descriptor)
	if err != nil {
		return infrastructureFailure(fmt.Errorf("marshal descriptor for %s: %w", descriptor.NodeID, err))
	}

	descriptorPath, err := descriptorFilePath(descriptorDir, descriptor)
	if err != nil {
		return infrastructureFailure(err)
	}
	if err := os.MkdirAll(filepath.Dir(descriptorPath), 0o755); err != nil {
		return infrastructureFailure(fmt.Errorf("create descriptor scope directory: %w", err))
	}
	if err := os.WriteFile(descriptorPath, descriptorBytes, 0o644); err != nil {
		return infrastructureFailure(fmt.Errorf("write descriptor %q: %w", descriptorPath, err))
	}

	var argv []string
	if len(i.CommandTemplate) == 0 {
		argv, err = DefaultRunnerCommand(descriptor.Symbol.Language)
		if err != nil {
			return infrastructureFailure(fmt.Errorf("build runner command for %s: %w", descriptor.NodeID, err))
		}
		argv = substituteDescriptorPath(argv, descriptorPath)
	} else {
		argv = substituteDescriptorPath(i.CommandTemplate, descriptorPath)
	}
	output, err := taskprocess.Run(ctx, argv, i.WorkingDir)
	diagnostic := strings.TrimSpace(output)

	if err == nil {
		return StepInvocationOutcome{
			NodeID:     descriptor.NodeID,
			Attempt:    descriptor.Attempt,
			Scope:      descriptor.Scope,
			Status:     StatusSucceeded,
			ExitCode:   0,
			Diagnostic: diagnostic,
		}, nil
	}
	if contextError := ctx.Err(); contextError != nil {
		return StepInvocationOutcome{
			NodeID:   descriptor.NodeID,
			Attempt:  descriptor.Attempt,
			Scope:    descriptor.Scope,
			Status:   StatusCancelled,
			ExitCode: -1,
		}, contextError
	}

	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return infrastructureFailure(fmt.Errorf("invoke runner for %s: %w", descriptor.NodeID, err))
	}

	return StepInvocationOutcome{
		NodeID:     descriptor.NodeID,
		Attempt:    descriptor.Attempt,
		Scope:      descriptor.Scope,
		Status:     StatusFailed,
		ExitCode:   exitError.ExitCode(),
		Diagnostic: diagnostic,
	}, nil
}

func descriptorFilePath(descriptorDir string, descriptor StepInvocationDescriptor) (string, error) {
	if err := validateDescriptorFileIdentity(descriptor); err != nil {
		return "", err
	}
	parts := []string{descriptorDir, descriptor.RunID, descriptor.NodeID}
	if descriptor.Scope != nil {
		parts = append(parts, "scopes")
		for _, frame := range descriptor.Scope.Frames {
			parts = append(parts, "maps", frame.MapID, "items", fmt.Sprint(frame.Index))
		}
	}
	parts = append(parts, fmt.Sprint(descriptor.Attempt)+".json")
	return filepath.Join(parts...), nil
}

func validateDescriptorFileIdentity(descriptor StepInvocationDescriptor) error {
	if !ValidSafePathSegment(descriptor.RunID) || !ValidSafePathSegment(descriptor.NodeID) || descriptor.Attempt < 1 || int64(descriptor.Attempt) > artifact.MaxJSONSafeInteger {
		return fmt.Errorf("invalid descriptor identity for filename")
	}
	if descriptor.Scope != nil {
		if err := artifact.ValidateExecutionScope(descriptor.Scope); err != nil {
			return fmt.Errorf("invalid descriptor scope: %w", err)
		}
	}
	return nil
}

func substituteDescriptorPath(command []string, descriptorPath string) []string {
	argv := make([]string, 0, len(command)+1)
	substituted := false
	for _, arg := range command {
		if arg == descriptorPathToken {
			argv = append(argv, descriptorPath)
			substituted = true
			continue
		}
		argv = append(argv, arg)
	}
	if !substituted {
		argv = append(argv, descriptorPath)
	}
	return argv
}
