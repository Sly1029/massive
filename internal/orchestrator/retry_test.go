package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func TestExecutionPolicyDelayBackoffIsCapped(t *testing.T) {
	policy := policyForContract(&planpb.ExecutionContract{Retry: &planpb.RetryPolicy{
		MaxAttempts: pointer(uint32(6)), DelaySeconds: pointer(uint32(10)),
		BackoffFactor: pointer(uint32(3)), MaxDelaySeconds: pointer(uint32(60)),
	}})
	for attempt, want := range map[int]time.Duration{2: 10 * time.Second, 3: 30 * time.Second, 4: 60 * time.Second, 5: 60 * time.Second} {
		if got := policy.delayBefore(attempt); got != want {
			t.Fatalf("delayBefore(%d) = %s, want %s", attempt, got, want)
		}
	}
	if single := policyForContract(&planpb.ExecutionContract{}); single.maxAttempts != 1 || single.timeout != 0 {
		t.Fatalf("contract without retry = %#v, want one attempt and no deadline", single)
	}
}

func TestRetryableOutcomeHonorsRunnerExitContract(t *testing.T) {
	for _, testCase := range []struct {
		outcome StepInvocationOutcome
		want    bool
	}{
		{StepInvocationOutcome{Status: StatusFailed, ExitCode: runnerExitStepExecution}, true},
		{StepInvocationOutcome{Status: StatusFailed, ExitCode: -1, TimedOutAfter: time.Second}, true},
		{StepInvocationOutcome{Status: StatusFailed, ExitCode: 137}, true},
		{StepInvocationOutcome{Status: StatusFailed, ExitCode: runnerExitDescriptorResolution}, false},
		{StepInvocationOutcome{Status: StatusFailed, ExitCode: runnerExitSchemaValidation}, false},
		{StepInvocationOutcome{Status: StatusFailed, ExitCode: runnerExitNonRetryable}, false},
		{StepInvocationOutcome{Status: StatusCancelled}, false},
	} {
		if got := retryableOutcome(testCase.outcome); got != testCase.want {
			t.Fatalf("retryableOutcome(%#v) = %t, want %t", testCase.outcome, got, testCase.want)
		}
	}
}

func TestRunRetriesFailedStaticStepIntoNewAttemptSlot(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, manifests := compileRetryFixture(t, "linear-chain", sourceRoot, 3, 0)
	runner := newFlakyRunner(t, map[string][]int{"increment": {runnerExitStepExecution}})

	result, err := Run(context.Background(), RunConfig{
		Plan: compiled.Plan, DatastoreRoot: storeRoot, ProjectID: "examples/retries", RunID: "static-retry",
		SourcePackageRoot: sourceRoot, SourceManifests: manifests, StepInvoker: runner.invoker(),
	}, []byte("20"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", result.Status)
	}
	manifest := readRunManifest(t, storeRoot, result.ProjectKey, result.RunID)
	increment := manifest.Steps[1]
	if increment.NodeID != "increment" || increment.Status != StatusSucceeded || len(increment.Attempts) != 2 {
		t.Fatalf("increment journal = %#v, want two attempts", increment)
	}
	first, second := increment.Attempts[0], increment.Attempts[1]
	if first.Attempt != 1 || first.Status != StatusFailed || !strings.Contains(first.Diagnostic, "step-execution-failure (exit 66)") || first.Output != nil {
		t.Fatalf("first attempt = %#v, want durable retryable failure", first)
	}
	if second.Attempt != 2 || second.Status != StatusSucceeded || second.Output == nil || !strings.Contains(second.Output.Manifest.Key, "/increment/2/output-manifest.json") {
		t.Fatalf("second attempt = %#v, want output in the attempt-2 slot", second)
	}
	for _, step := range []int{0, 2} {
		if len(manifest.Steps[step].Attempts) != 1 {
			t.Fatalf("step %s attempts = %d, want 1", manifest.Steps[step].NodeID, len(manifest.Steps[step].Attempts))
		}
	}
	descriptors := runner.descriptors(t)
	if len(descriptors) != 4 {
		t.Fatalf("runner received %d descriptors, want 4 (three steps plus one retry)", len(descriptors))
	}
	for _, descriptor := range descriptors {
		if descriptor.MaxAttempts != 3 {
			t.Fatalf("descriptor %s maxAttempts = %d, want 3", descriptor.NodeID, descriptor.MaxAttempts)
		}
		assertDescriptorFileValidAgainstFrozenSchema(t, descriptor.path)
	}
}

func TestRunDoesNotRetryNonRetryableOrExhaustedStaticSteps(t *testing.T) {
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	for _, testCase := range []struct {
		name      string
		exitCodes []int
		attempts  int
		want      string
		suffix    string
	}{
		{"non-retryable", []int{runnerExitNonRetryable}, 1, "non-retryable-step-failure (exit 67)", "(attempt 1 of 3)"},
		{"schema", []int{runnerExitSchemaValidation}, 1, "schema-validation-failure (exit 65)", "(attempt 1 of 3)"},
		{"exhausted", []int{66, 66, 66}, 3, "step-execution-failure (exit 66)", "(attempt 3 of 3)"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			storeRoot := newStoreRoot(t)
			compiled, manifests := compileRetryFixture(t, "linear-chain", sourceRoot, 3, 0)
			runner := newFlakyRunner(t, map[string][]int{"double": testCase.exitCodes})

			result, err := Run(context.Background(), RunConfig{
				Plan: compiled.Plan, DatastoreRoot: storeRoot, ProjectID: "examples/retries", RunID: "static-" + testCase.name,
				SourcePackageRoot: sourceRoot, SourceManifests: manifests, StepInvoker: runner.invoker(),
			}, []byte("20"))
			if err == nil || !strings.Contains(err.Error(), testCase.want) || !strings.HasSuffix(err.Error(), testCase.suffix) {
				t.Fatalf("run error = %v, want %q ending %q", err, testCase.want, testCase.suffix)
			}
			manifest := readRunManifest(t, storeRoot, result.ProjectKey, result.RunID)
			double := manifest.Steps[0]
			if double.Status != StatusFailed || len(double.Attempts) != testCase.attempts {
				t.Fatalf("double journal = %#v, want %d failed attempts", double, testCase.attempts)
			}
			for index, attempt := range double.Attempts {
				if attempt.Attempt != index+1 || attempt.Status != StatusFailed {
					t.Fatalf("attempt %d = %#v, want ordered failure", index, attempt)
				}
			}
		})
	}
}

func TestRunRetriesOnlyFailedMapItems(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := finiteMapSourceRoot(t)
	compiled, manifests := compileRetryFixture(t, "finite-map", sourceRoot, 2, 0)
	runner := newFlakyRunner(t, map[string][]int{"map-items/1": {runnerExitStepExecution}})

	result, err := Run(context.Background(), RunConfig{
		Plan: compiled.Plan, DatastoreRoot: storeRoot, ProjectID: "examples/retries", RunID: "map-retry",
		SourcePackageRoot: sourceRoot, SourceManifests: manifests, StepInvoker: runner.invoker(),
	}, []byte(`[2,1,2]`))
	if err != nil {
		t.Fatal(err)
	}
	assertStoredJSON(t, storeRoot, result.ResultKey, `["item:2","item:1","item:2"]`)
	manifest := readRunManifest(t, storeRoot, result.ProjectKey, result.RunID)
	for index, item := range *manifest.Steps[0].Items {
		wantAttempts := 1
		if index == 1 {
			wantAttempts = 2
		}
		if item.Status != StatusSucceeded || len(item.Attempts) != wantAttempts {
			t.Fatalf("item %d = %#v, want %d attempts", index, item, wantAttempts)
		}
		last := item.Attempts[len(item.Attempts)-1]
		if !strings.Contains(last.Output.Manifest.Key, "/items/"+strconv.Itoa(index)+"/"+strconv.Itoa(wantAttempts)+"/output-manifest.json") {
			t.Fatalf("item %d output key = %q", index, last.Output.Manifest.Key)
		}
	}
	if item := (*manifest.Steps[0].Items)[1]; item.Attempts[0].Status != StatusFailed {
		t.Fatalf("failed item attempt was overwritten: %#v", item.Attempts[0])
	}
}

func TestRunStopsMapRetriesAfterTerminalSiblingFailure(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := finiteMapSourceRoot(t)
	compiled, manifests := compileRetryFixture(t, "finite-map", sourceRoot, 3, 0)
	runner := newFlakyRunner(t, map[string][]int{
		"map-items/0": {runnerExitStepExecution},
		"map-items/2": {runnerExitNonRetryable},
	})

	result, err := Run(context.Background(), RunConfig{
		Plan: compiled.Plan, DatastoreRoot: storeRoot, ProjectID: "examples/retries", RunID: "map-terminal",
		SourcePackageRoot: sourceRoot, SourceManifests: manifests, StepInvoker: runner.invoker(),
	}, []byte(`[2,1,2]`))
	if err == nil || !strings.Contains(err.Error(), "non-retryable-step-failure (exit 67)") || !strings.Contains(err.Error(), "(attempt 1 of 3)") {
		t.Fatalf("run error = %v, want the terminal item failure", err)
	}
	manifest := readRunManifest(t, storeRoot, result.ProjectKey, result.RunID)
	items := *manifest.Steps[0].Items
	for _, index := range []int{0, 2} {
		if items[index].Status != StatusFailed || len(items[index].Attempts) != 1 || items[index].Attempts[0].Status != StatusFailed {
			t.Fatalf("item %d = %#v, want one failed attempt and no retry", index, items[index])
		}
	}
	if items[1].Status != StatusSucceeded {
		t.Fatalf("item 1 = %#v, want succeeded", items[1])
	}
}

func TestProcessStepInvokerTimesOutAttempt(t *testing.T) {
	script := filepath.Join(t.TempDir(), "runner.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	outcomes, err := (ProcessStepInvoker{CommandTemplate: []string{script}, ProcessLimit: 1}).InvokeSteps(context.Background(), StepInvocationBatch{
		Steps: []StepInvocation{{Descriptor: processMapDescriptor("timeout", 0), Timeout: 200 * time.Millisecond}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("timed-out runner was not stopped (%s)", elapsed)
	}
	if len(outcomes) != 1 || outcomes[0].Status != StatusFailed || outcomes[0].TimedOutAfter != 200*time.Millisecond || !retryableOutcome(outcomes[0]) {
		t.Fatalf("outcomes = %#v, want one retryable timed-out failure", outcomes)
	}
	if summary := runnerFailureSummary(outcomes[0], ""); summary != "step-timeout (timed out after 200ms)" {
		t.Fatalf("timeout summary = %q", summary)
	}
}

func TestIsolatedStepRejectsAttemptBeyondPolicy(t *testing.T) {
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, _ := compileRetryFixture(t, "linear-chain", sourceRoot, 2, 0)
	_, err := RunIsolatedStep(context.Background(), IsolatedStepConfig{
		Plan: compiled.Plan, NodeID: "double", Datastore: LocalDatastoreDescriptor{Kind: "local", Path: newStoreRoot(t)},
		ProjectID: "examples/retries", RunID: "isolated-overflow", Attempt: 3,
	}, []byte("20"))
	if err == nil || !strings.Contains(err.Error(), "attempt 3 exceeds its retry policy of 2 attempts") {
		t.Fatalf("isolated error = %v, want retry policy bound", err)
	}
	var failure *InvocationFailure
	if errors.As(err, &failure) {
		t.Fatalf("policy rejection reported as runner failure: %#v", failure)
	}
}

func TestInvocationFailureRetryableUsesAttemptBudget(t *testing.T) {
	for _, testCase := range []struct {
		failure InvocationFailure
		want    bool
	}{
		{InvocationFailure{Attempt: 1, MaxAttempts: 2, ExitCode: runnerExitStepExecution}, true},
		{InvocationFailure{Attempt: 2, MaxAttempts: 2, ExitCode: runnerExitStepExecution}, false},
		{InvocationFailure{Attempt: 1, MaxAttempts: 2, ExitCode: runnerExitNonRetryable}, false},
		{InvocationFailure{Attempt: 1, MaxAttempts: 2, ExitCode: -1, TimedOutAfter: time.Second}, true},
	} {
		if got := testCase.failure.Retryable(); got != testCase.want {
			t.Fatalf("Retryable(%#v) = %t, want %t", testCase.failure, got, testCase.want)
		}
	}
}

// flakyRunner is a real runner process. Listed attempts ("node" or
// "node/itemIndex" mapped to per-attempt exit codes) exit before author code
// starts; every other attempt execs the TypeScript runner and publishes output.
type flakyRunner struct {
	script   string
	captures string
}

func newFlakyRunner(t *testing.T, exitCodes map[string][]int) flakyRunner {
	t.Helper()
	directory := t.TempDir()
	runner := flakyRunner{script: filepath.Join(directory, "runner.sh"), captures: filepath.Join(directory, "descriptors")}
	if err := os.Mkdir(runner.captures, 0o755); err != nil {
		t.Fatal(err)
	}
	var cases strings.Builder
	for key, codes := range exitCodes {
		for index, code := range codes {
			fmt.Fprintf(&cases, "  '%s:%d') echo 'flaky attempt %d' >&2; exit %d ;;\n", key, index+1, index+1, code)
		}
	}
	script := `#!/bin/sh
set -eu
descriptor="$1"
node=$(grep -o '"nodeId": *"[^"]*"' "$descriptor" | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/')
attempt=$(grep -o '"attempt": *[0-9]*' "$descriptor" | head -n 1 | grep -o '[0-9]*$')
index=$(grep -o '"index": *[0-9]*' "$descriptor" | head -n 1 | grep -o '[0-9]*$' || true)
key="$node"
if [ -n "$index" ]; then key="$node/$index"; fi
cp "$descriptor" "` + runner.captures + `/$(printf '%s' "$key" | tr / _)-$attempt.json"
case "$key:$attempt" in
` + cases.String() + `esac
exec "` + filepath.Join(repoRootForTest(t), "scripts", "massive-typescript-runner") + `" "$descriptor"
`
	if err := os.WriteFile(runner.script, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runner
}

func (r flakyRunner) invoker() *ProcessStepInvoker {
	return &ProcessStepInvoker{CommandTemplate: []string{r.script, descriptorPathToken}, ProcessLimit: 2}
}

type capturedDescriptor struct {
	path        string
	NodeID      string `json:"nodeId"`
	MaxAttempts int    `json:"maxAttempts"`
}

// descriptors returns every descriptor the runner process received, including
// attempts that exited before author code ran.
func (r flakyRunner) descriptors(t *testing.T) []capturedDescriptor {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(r.captures, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	descriptors := make([]capturedDescriptor, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		descriptor := capturedDescriptor{path: path}
		if err := json.Unmarshal(data, &descriptor); err != nil {
			t.Fatal(err)
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors
}

func finiteMapSourceRoot(t *testing.T) string {
	t.Helper()
	sourceRoot := t.TempDir()
	source := "export function format(args: { readonly input: number }): string {\n  return `item:${args.input}`;\n}\n"
	if err := os.WriteFile(filepath.Join(sourceRoot, "workflow.ts"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return sourceRoot
}

// compileRetryFixture compiles a conformance fixture whose every contract
// carries a zero-delay retry policy and optional per-attempt timeout.
func compileRetryFixture(t *testing.T, name string, sourceDir string, maxAttempts int, timeoutSeconds int) (*plan.CompileResult, map[string]SourcePackageManifest) {
	t.Helper()

	specData := patchSpecSource(t, readRepoFile(t, "conformance", "fixtures", "specs", name, "workflow-spec.json"), sourceDir)
	var root map[string]any
	if err := json.Unmarshal(specData, &root); err != nil {
		t.Fatal(err)
	}
	for _, contract := range root["contracts"].(map[string]any) {
		contract := contract.(map[string]any)
		contract["retry"] = map[string]any{"maxAttempts": maxAttempts, "delaySeconds": 0, "backoffFactor": 1, "maxDelaySeconds": 0}
		if timeoutSeconds > 0 {
			contract["timeoutSeconds"] = timeoutSeconds
		}
	}
	specData, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	specData = resignWorkflowSpec(t, specData)
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}
	return compiled, manifestsFromSpec(workflowSpec)
}
