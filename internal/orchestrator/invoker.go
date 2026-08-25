package orchestrator

import (
	"bytes"
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
)

const (
	descriptorPathToken             = "{descriptor}"
	defaultLocalProcessConcurrency  = 32
	stepInvocationStatusCancelled   = "cancelled"
	stepInvocationStatusInfraFailed = "infrastructure-failed"
)

type DefaultRunnerCommandInputs struct {
	Language      string
	WorkingDir    string
	DescriptorDir string
	DatastoreRoot string
}

func DefaultRunnerCommand(inputs DefaultRunnerCommandInputs) ([]string, error) {
	workingDir := inputs.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}
	if inputs.DescriptorDir == "" {
		return nil, fmt.Errorf("descriptor directory is required")
	}
	if inputs.DatastoreRoot == "" {
		return nil, fmt.Errorf("datastore root is required")
	}
	workingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve runner working directory: %w", err)
	}
	descriptorDir, err := filepath.Abs(inputs.DescriptorDir)
	if err != nil {
		return nil, fmt.Errorf("resolve descriptor directory: %w", err)
	}
	datastoreRoot, err := filepath.Abs(inputs.DatastoreRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve datastore root: %w", err)
	}

	if inputs.Language == "python" {
		return []string{
			"massive-python-runner",
			descriptorPathToken,
		}, nil
	}
	if inputs.Language != "typescript" {
		return nil, fmt.Errorf("unsupported runner language %q", inputs.Language)
	}

	readRoots := []string{workingDir, descriptorDir, datastoreRoot}
	return []string{
		"deno",
		"run",
		"--config",
		"deno.json",
		"--allow-read=" + strings.Join(readRoots, ","),
		"--allow-write=" + datastoreRoot,
		"packages/sdk/src/runner/main.ts",
		descriptorPathToken,
	}, nil
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
		localDatastore, ok := descriptor.Datastore.(LocalDatastoreDescriptor)
		if !ok {
			return infrastructureFailure(fmt.Errorf("build runner command for %s: local process invoker requires a local datastore descriptor, got %T", descriptor.NodeID, descriptor.Datastore))
		}
		inputs := DefaultRunnerCommandInputs{
			Language:      descriptor.Symbol.Language,
			WorkingDir:    i.WorkingDir,
			DescriptorDir: descriptorDir,
			DatastoreRoot: localDatastore.Path,
		}
		argv, err = DefaultRunnerCommand(inputs)
		if err != nil {
			return infrastructureFailure(fmt.Errorf("build runner command for %s: %w", descriptor.NodeID, err))
		}
		argv = substituteDescriptorPath(argv, descriptorPath)
	} else {
		argv = substituteDescriptorPath(i.CommandTemplate, descriptorPath)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = i.WorkingDir

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err = cmd.Run()
	diagnostic := strings.TrimSpace(combined.String())

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
			Status:   stepInvocationStatusCancelled,
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
	if !validSafePathSegment(descriptor.RunID) || !validSafePathSegment(descriptor.NodeID) || descriptor.Attempt < 1 || int64(descriptor.Attempt) > artifact.MaxJSONSafeInteger {
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
