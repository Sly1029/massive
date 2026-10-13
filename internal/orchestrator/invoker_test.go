package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/artifact"
)

func TestDefaultRunnerCommandSelectsLanguageAdapters(t *testing.T) {
	t.Setenv("MASSIVE_PYTHON", "")
	t.Setenv("MASSIVE_TYPESCRIPT_RUNNER", "")
	if argv, err := DefaultRunnerCommand("typescript"); err != nil || !reflect.DeepEqual(argv, []string{"massive-typescript-runner", descriptorPathToken}) {
		t.Fatalf("typescript: %v %v", argv, err)
	}
	if _, err := DefaultRunnerCommand("python"); err == nil || !strings.Contains(err.Error(), "MASSIVE_PYTHON") {
		t.Fatalf("Python without MASSIVE_PYTHON = %v, want an error", err)
	}
	t.Setenv("MASSIVE_PYTHON", "/active/python")
	if argv, err := DefaultRunnerCommand("python"); err != nil || !reflect.DeepEqual(argv, []string{"/active/python", "-I", "-m", "massive.runner", descriptorPathToken}) {
		t.Fatalf("python: %v %v", argv, err)
	}
	argv, err := DefaultRunnerCommand("typescript")
	if err != nil || argv[0] != "massive-typescript-runner" {
		t.Fatalf("Python environment overrode TypeScript: %v %v", argv, err)
	}
	if _, err := DefaultRunnerCommand("ruby"); err == nil {
		t.Fatal("unsupported language accepted")
	}
}

func TestSubstituteDescriptorPathKeepsTemplateBehavior(t *testing.T) {
	descriptorPath := filepath.Join(t.TempDir(), "descriptor.json")

	replaced := substituteDescriptorPath([]string{"runner", descriptorPathToken, "--flag"}, descriptorPath)
	expectedReplaced := []string{"runner", descriptorPath, "--flag"}
	if !reflect.DeepEqual(replaced, expectedReplaced) {
		t.Fatalf("replaced argv = %#v, want %#v", replaced, expectedReplaced)
	}

	appended := substituteDescriptorPath([]string{"runner", "--flag"}, descriptorPath)
	expectedAppended := []string{"runner", "--flag", descriptorPath}
	if !reflect.DeepEqual(appended, expectedAppended) {
		t.Fatalf("appended argv = %#v, want %#v", appended, expectedAppended)
	}
}

func TestDescriptorFilePathIncludesEveryOrderedScopeFrame(t *testing.T) {
	root := t.TempDir()
	path, err := descriptorFilePath(root, StepInvocationDescriptor{
		RunID: "run-1", NodeID: "task", Attempt: 1,
		Scope: &ExecutionScope{Frames: []MapItemScopeFrame{
			{Kind: "map-item", MapID: "outer", Index: 0},
			{Kind: "map-item", MapID: "inner", Index: 4},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "run-1", "task", "scopes", "maps", "outer", "items", "0", "maps", "inner", "items", "4", "1.json")
	if path != want {
		t.Fatalf("scoped descriptor path = %q, want %q", path, want)
	}
}

func TestProcessStepInvokerRejectsUnsafeScopeIdentityBeforeWritingDescriptors(t *testing.T) {
	for name, descriptor := range map[string]StepInvocationDescriptor{
		"scope index above JSON safe integer": {
			RunID: "run-1", NodeID: "task", Attempt: 1,
			Scope: &ExecutionScope{Frames: []MapItemScopeFrame{{Kind: "map-item", MapID: "fanout", Index: int(artifact.MaxJSONSafeInteger + 1)}}},
		},
		"attempt above JSON safe integer": {
			RunID: "run-1", NodeID: "task", Attempt: int(artifact.MaxJSONSafeInteger + 1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			descriptorDir := filepath.Join(t.TempDir(), "descriptors")
			_, err := (ProcessStepInvoker{DescriptorDir: descriptorDir, CommandTemplate: []string{"must-not-run"}}).InvokeSteps(t.Context(), StepInvocationBatch{Steps: []StepInvocation{{Descriptor: descriptor}}})
			if err == nil {
				t.Fatal("InvokeSteps accepted unsafe descriptor identity")
			}
			if _, err := os.Stat(descriptorDir); !os.IsNotExist(err) {
				t.Fatalf("unsafe descriptor created directory %q: %v", descriptorDir, err)
			}
		})
	}
}

func TestProcessStepInvokerBoundsHighCardinalityBatchWithRealProcesses(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "max"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(state, "runner.sh")
	source := "#!/bin/sh\n" +
		"state='" + state + "'\n" +
		"while ! mkdir \"$state/lock\" 2>/dev/null; do sleep 0.005; done\n" +
		"mkdir \"$state/active/$$\"\n" +
		"count=$(find \"$state/active\" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')\n" +
		"max=$(cat \"$state/max\")\n" +
		"if [ \"$count\" -gt \"$max\" ]; then printf '%s' \"$count\" > \"$state/max\"; fi\n" +
		"rmdir \"$state/lock\"\n" +
		"sleep 0.08\n" +
		"rmdir \"$state/active/$$\"\n"
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}

	steps := make([]StepInvocation, 24)
	for index := range steps {
		steps[index] = StepInvocation{Descriptor: processMapDescriptor("bounded-batch", index)}
	}
	outcomes, err := (ProcessStepInvoker{CommandTemplate: []string{script}, ProcessLimit: 3}).InvokeSteps(
		context.Background(),
		StepInvocationBatch{Steps: steps, MaxConcurrency: 1_000_000},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != len(steps) {
		t.Fatalf("outcomes = %d, want %d", len(outcomes), len(steps))
	}
	maximum, err := strconv.Atoi(strings.TrimSpace(string(mustReadFile(t, filepath.Join(state, "max")))))
	if err != nil {
		t.Fatal(err)
	}
	if maximum != 3 {
		t.Fatalf("maximum live child processes = %d, want exactly 3", maximum)
	}
}

func TestProcessStepInvokerStopsDispatchAfterContextCancellation(t *testing.T) {
	state := t.TempDir()
	startedPath := filepath.Join(state, "started")
	script := filepath.Join(state, "runner.sh")
	source := "#!/bin/sh\nprintf x >> '" + startedPath + "'\nexec sleep 30\n"
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}

	steps := make([]StepInvocation, 20)
	for index := range steps {
		steps[index] = StepInvocation{Descriptor: processMapDescriptor("cancelled-batch", index)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Cancel after all worker slots have actually started, not after an assumed
	// process startup latency. The deadline only bounds a broken fixture.
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				started, err := os.ReadFile(startedPath)
				if err == nil && len(started) == 3 {
					cancel()
					return
				}
			}
		}
	}()
	outcomes, err := (ProcessStepInvoker{CommandTemplate: []string{script}}).InvokeSteps(
		ctx,
		StepInvocationBatch{Steps: steps, MaxConcurrency: 3},
	)
	<-watching
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation after workers started", err)
	}
	started := len(mustReadFile(t, startedPath))
	if started != 3 {
		t.Fatalf("started processes = %d, want exactly the 3 worker slots", started)
	}
	if len(outcomes) != started {
		t.Fatalf("outcomes = %d, started = %d", len(outcomes), started)
	}
	for _, outcome := range outcomes {
		if outcome.Status != StatusCancelled || outcome.Diagnostic != "" {
			t.Fatalf("cancelled outcome = %#v, want sanitized cancelled status", outcome)
		}
	}
}

func TestProcessStepInvokerStopsDispatchAfterInfrastructureFailure(t *testing.T) {
	steps := make([]StepInvocation, 20)
	for index := range steps {
		steps[index] = StepInvocation{Descriptor: processMapDescriptor("missing-runner", index)}
	}
	outcomes, err := (ProcessStepInvoker{CommandTemplate: []string{"massive-runner-that-does-not-exist"}}).InvokeSteps(
		context.Background(),
		StepInvocationBatch{Steps: steps, MaxConcurrency: 3},
	)
	if err == nil {
		t.Fatal("missing runner returned no infrastructure error")
	}
	if len(outcomes) == 0 || len(outcomes) > 3 {
		t.Fatalf("outcomes = %d, want only the started worker set", len(outcomes))
	}
}

func processMapDescriptor(runID string, index int) StepInvocationDescriptor {
	projectKey := "sha256-" + strings.Repeat("a", 64)
	planHash := "sha256:" + strings.Repeat("b", 64)
	schemaRef := "sha256:" + strings.Repeat("c", 64)
	scope := &ExecutionScope{Frames: []MapItemScopeFrame{{Kind: "map-item", MapID: "map-items", Index: index}}}
	return StepInvocationDescriptor{
		PlanHash: planHash, ProjectKey: projectKey, RunID: runID, NodeID: "map-items", Attempt: 1, Scope: scope,
		Output: DataArtifactManifestDestination{
			ManifestKey: runOutputManifestKey(projectKey, runID, "map-items", scope, 1).String(),
			Schema:      schemaRef,
		},
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()

	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func TestReadRunnerMessageValidatesBoundedStructuredResults(t *testing.T) {
	for name, body := range map[string]string{
		"valid":      `{"exitCode":66,"message":"real failure"}`,
		"astral":     `{"exitCode":66,"message":"` + strings.Repeat(`\ud83d\ude80`, 1024) + `"}`,
		"missing":    `{"exitCode":66}`,
		"null":       `{"exitCode":66,"message":null}`,
		"mismatched": `{"exitCode":67,"message":"wrong exit"}`,
		"oversized":  `{"exitCode":66,"message":"` + strings.Repeat("x", 9000) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "descriptor.json")
			if err := os.WriteFile(path+".failure.json", []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := readRunnerFailure(path, 66)
			message := result.Message
			if name == "astral" {
				if err != nil || message != strings.Repeat("🚀", 1024) {
					t.Fatalf("astral message length = %d, error = %v", len(message), err)
				}
			} else if name == "valid" || name == "no type" {
				if err != nil || message != "real failure" {
					t.Fatalf("message = %q, error = %v", message, err)
				}
			} else if err == nil {
				t.Fatalf("invalid result returned message %q", message)
			}
		})
	}
}

func TestFatalRunnerRecordIsRequiredAndValidated(t *testing.T) {
	for name, body := range map[string]string{
		"valid":           `{"exitCode":69,"type":"input.invalid","message":"Safe cause"}`,
		"missing":         "",
		"no type":         `{"exitCode":69,"message":"Safe cause"}`,
		"invalid type":    `{"exitCode":69,"type":7,"message":"Safe cause"}`,
		"long type":       `{"exitCode":69,"type":"` + strings.Repeat("a", 65) + `","message":"Safe cause"}`,
		"long message":    `{"exitCode":69,"type":"input.invalid","message":"` + strings.Repeat("a", 1025) + `"}`,
		"wrong exit":      `{"exitCode":67,"message":"Safe cause"}`,
		"extra exception": `{"exitCode":69,"type":"input.invalid","message":"Safe cause","exception":"SECRET"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "descriptor.json")
			if body != "" {
				if err := os.WriteFile(path+".failure.json", []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := readRunnerFailure(path, FatalRunExitCode)
			if name == "valid" || name == "no type" {
				if err != nil || (name == "valid" && result.Type != "input.invalid") || result.Message != "Safe cause" {
					t.Fatalf("fatal record: %+v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("invalid fatal record accepted")
			}
		})
	}
}
