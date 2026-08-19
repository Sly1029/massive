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
)

const descriptorPathToken = "{descriptor}"

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
}

func (i ProcessStepInvoker) InvokeSteps(ctx context.Context, batch StepInvocationBatch) ([]StepInvocationOutcome, error) {
	if len(batch.Steps) == 0 {
		return nil, nil
	}
	for _, step := range batch.Steps {
		if err := validateDescriptorFileIdentity(step.Descriptor); err != nil {
			return nil, err
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
	outcomes := make([]StepInvocationOutcome, len(batch.Steps))
	errs := make([]error, len(batch.Steps))
	jobs := make(chan int)
	var group sync.WaitGroup
	for range maxConcurrency {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				outcomes[index], errs[index] = i.invokeOne(ctx, descriptorDir, batch.Steps[index].Descriptor)
			}
		}()
	}
	for index := range batch.Steps {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return outcomes, nil
}

func (i ProcessStepInvoker) invokeOne(ctx context.Context, descriptorDir string, descriptor StepInvocationDescriptor) (StepInvocationOutcome, error) {
	descriptorBytes, err := marshalCanonicalJSON(descriptor)
	if err != nil {
		return StepInvocationOutcome{}, fmt.Errorf("marshal descriptor for %s: %w", descriptor.NodeID, err)
	}

	descriptorPath, err := descriptorFilePath(descriptorDir, descriptor)
	if err != nil {
		return StepInvocationOutcome{}, err
	}
	if err := os.MkdirAll(filepath.Dir(descriptorPath), 0o755); err != nil {
		return StepInvocationOutcome{}, fmt.Errorf("create descriptor scope directory: %w", err)
	}
	if err := os.WriteFile(descriptorPath, descriptorBytes, 0o644); err != nil {
		return StepInvocationOutcome{}, fmt.Errorf("write descriptor %q: %w", descriptorPath, err)
	}

	var argv []string
	if len(i.CommandTemplate) == 0 {
		localDatastore, ok := descriptor.Datastore.(LocalDatastoreDescriptor)
		if !ok {
			return StepInvocationOutcome{}, fmt.Errorf("build runner command for %s: local process invoker requires a local datastore descriptor, got %T", descriptor.NodeID, descriptor.Datastore)
		}
		inputs := DefaultRunnerCommandInputs{
			Language:      descriptor.Symbol.Language,
			WorkingDir:    i.WorkingDir,
			DescriptorDir: descriptorDir,
			DatastoreRoot: localDatastore.Path,
		}
		argv, err = DefaultRunnerCommand(inputs)
		if err != nil {
			return StepInvocationOutcome{}, fmt.Errorf("build runner command for %s: %w", descriptor.NodeID, err)
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

	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return StepInvocationOutcome{}, fmt.Errorf("invoke runner for %s: %w", descriptor.NodeID, err)
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
