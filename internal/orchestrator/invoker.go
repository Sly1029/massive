package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
)

const descriptorPathToken = "{descriptor}"

type DefaultRunnerCommandInputs struct {
	WorkingDir        string
	DescriptorDir     string
	DatastoreRoot     string
	SourcePackageRoot string
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

	readRoots := []string{workingDir, descriptorDir, datastoreRoot}
	if inputs.SourcePackageRoot != "" {
		sourcePackageRoot, err := filepath.Abs(inputs.SourcePackageRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve source package root: %w", err)
		}
		relative, err := filepath.Rel(workingDir, sourcePackageRoot)
		if err != nil {
			return nil, fmt.Errorf("compare source package root with runner working directory: %w", err)
		}
		if relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			readRoots = append(readRoots, sourcePackageRoot)
		}
	}

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

	outcomes := make([]StepInvocationOutcome, 0, len(batch.Steps))
	for _, step := range batch.Steps {
		outcome, err := i.invokeOne(ctx, descriptorDir, step.Descriptor)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func (i ProcessStepInvoker) invokeOne(ctx context.Context, descriptorDir string, descriptor StepInvocationDescriptor) (StepInvocationOutcome, error) {
	descriptorBytes, err := marshalCanonicalJSON(descriptor)
	if err != nil {
		return StepInvocationOutcome{}, fmt.Errorf("marshal descriptor for %s: %w", descriptor.NodeID, err)
	}

	descriptorPath := filepath.Join(descriptorDir, descriptor.RunID+"-"+descriptor.NodeID+"-"+fmt.Sprint(descriptor.Attempt)+".json")
	if err := os.WriteFile(descriptorPath, descriptorBytes, 0o644); err != nil {
		return StepInvocationOutcome{}, fmt.Errorf("write descriptor %q: %w", descriptorPath, err)
	}

	var argv []string
	if len(i.CommandTemplate) == 0 {
		inputs := DefaultRunnerCommandInputs{
			WorkingDir:    i.WorkingDir,
			DescriptorDir: descriptorDir,
			DatastoreRoot: descriptor.Datastore.Path,
		}
		if descriptor.SourcePackage.SourceArchive.ContentType == SourceDirectoryContentType {
			if descriptor.Datastore.Kind != "local" {
				return StepInvocationOutcome{}, fmt.Errorf("build runner command for %s: source directory packages require a local datastore", descriptor.NodeID)
			}
			store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: descriptor.Datastore.Path})
			if err != nil {
				return StepInvocationOutcome{}, fmt.Errorf("open local datastore for source package: %w", err)
			}
			key, err := datastore.ParseKey(descriptor.SourcePackage.SourceArchive.Key)
			if err != nil {
				return StepInvocationOutcome{}, err
			}
			object, err := store.Get(ctx, key)
			if err != nil {
				return StepInvocationOutcome{}, fmt.Errorf("read source package pointer %s: %w", key, err)
			}
			if err := verifyDigest(descriptor.SourcePackage.SourceArchive.Hash, object.Body); err != nil {
				return StepInvocationOutcome{}, fmt.Errorf("source package pointer %s: %w", key, err)
			}
			var pointer sourceFetchPointer
			if err := json.Unmarshal(object.Body, &pointer); err != nil {
				return StepInvocationOutcome{}, fmt.Errorf("decode source package pointer %s: %w", key, err)
			}
			if pointer.SourceFetch == "" {
				return StepInvocationOutcome{}, fmt.Errorf("source package pointer %s is missing sourceFetch", key)
			}
			inputs.SourcePackageRoot = pointer.SourceFetch
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
		outputHash, hashErr := hashLocalOutput(ctx, descriptor)
		if hashErr != nil {
			return StepInvocationOutcome{}, hashErr
		}
		return StepInvocationOutcome{
			NodeID:             descriptor.NodeID,
			Attempt:            descriptor.Attempt,
			Status:             StatusSucceeded,
			ExitCode:           0,
			Diagnostic:         diagnostic,
			ExpectedOutputHash: outputHash,
		}, nil
	}

	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return StepInvocationOutcome{}, fmt.Errorf("invoke runner for %s: %w", descriptor.NodeID, err)
	}

	return StepInvocationOutcome{
		NodeID:     descriptor.NodeID,
		Attempt:    descriptor.Attempt,
		Status:     StatusFailed,
		ExitCode:   exitError.ExitCode(),
		Diagnostic: diagnostic,
	}, nil
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

func hashLocalOutput(ctx context.Context, descriptor StepInvocationDescriptor) (string, error) {
	if descriptor.Datastore.Kind != "local" {
		return "", fmt.Errorf("hash runner output for %s: only local datastores are supported", descriptor.NodeID)
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: descriptor.Datastore.Path})
	if err != nil {
		return "", fmt.Errorf("open local datastore for runner output: %w", err)
	}
	key, err := datastore.ParseKey(descriptor.Output.Artifact.Key)
	if err != nil {
		return "", err
	}
	object, err := store.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("read runner output %s: %w", key, err)
	}
	return canonical.DigestBytes(object.Body), nil
}
