package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/runjournal"
)

// Exit hooks run once after every local run settles, receive its outcome,
// and never change its status, even when the hook itself fails.
func TestExitHookReceivesEveryOutcomeWithoutChangingIt(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Fatal("install the Python SDK environment with uv sync --project packages/python")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "massive")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	store := filepath.Join(root, "store")
	t.Cleanup(func() {
		_ = filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	source := `import os
import time
from datetime import timedelta
from pathlib import Path
from massive import GraphBuilder, MapItemOutcome, RunOutcome, StepContext, container, execution

DEFAULTS = execution(environment=container("example.invalid/runner@sha256:" + "1" * 64))

def check(ctx: StepContext[int]) -> int:
    if ctx.inputs < 0:
        raise ValueError("negative input")
    return ctx.inputs

def stall(ctx: StepContext[int]) -> int:
    time.sleep(120)
    return ctx.inputs

def record(ctx: StepContext[RunOutcome]) -> None:
    Path(os.environ["OUTCOME_FILE"]).write_text(ctx.inputs.model_dump_json())

def broken(ctx: StepContext[RunOutcome]) -> None:
    record(ctx)
    raise RuntimeError("notification service unavailable")

def stalled_notification(ctx: StepContext[RunOutcome]) -> None:
    record(ctx)
    time.sleep(120)

def unwritable(ctx: StepContext[RunOutcome]) -> None:
    record(ctx)
    # The terminal journal has already been published. Keep its directory
    # readable, but prohibit the subsequent atomic hook-record rewrite.
    (run_dir,) = Path(os.environ["STORE_ROOT"]).glob(f"projects/*/runs/{ctx.inputs.run_id}")
    assert '"status":"' + ctx.inputs.status + '"' in (run_dir / "run-manifest.json").read_text()
    (run_dir / "steps").mkdir(exist_ok=True)
    run_dir.chmod(0o555)

def items(ctx: StepContext[int]) -> list[int]:
    return [ctx.inputs, -1]

collected = GraphBuilder(name="collected", input_type=int, output_type=list[MapItemOutcome[int]], defaults=DEFAULTS)
source = collected.add(items)
collected.edge_from(collected.start).to(source)
collected.edge_from(collected.map(source, check, id="checked-items", item_failures="collect")).to_end(collected.end)
collected.on_exit(record)

checked = GraphBuilder(name="checked", input_type=int, output_type=int, defaults=DEFAULTS)
checked.edge_from(checked.start).to(checked.add(check)).to_end(checked.end)
checked.on_exit(record)

broken_hook = GraphBuilder(name="broken-hook", input_type=int, output_type=int, defaults=DEFAULTS)
broken_hook.edge_from(broken_hook.start).to(broken_hook.add(check)).to_end(broken_hook.end)
broken_hook.on_exit(broken)

timed_hook = GraphBuilder(name="timed-hook", input_type=int, output_type=int, defaults=DEFAULTS)
timed_hook.edge_from(timed_hook.start).to(timed_hook.add(check)).to_end(timed_hook.end)
timed_hook.on_exit(stalled_notification, timeout=timedelta(seconds=2))

stalled = GraphBuilder(name="stalled", input_type=int, output_type=int, defaults=DEFAULTS,
    deadline=timedelta(seconds=5))
stalled.edge_from(stalled.start).to(stalled.add(stall)).to_end(stalled.end)
stalled.on_exit(record)

unwritable_hook = GraphBuilder(name="unwritable-hook", input_type=int, output_type=int, defaults=DEFAULTS)
unwritable_hook.edge_from(unwritable_hook.start).to(unwritable_hook.add(check)).to_end(unwritable_hook.end)
unwritable_hook.on_exit(unwritable)
`
	entry := filepath.Join(root, "workflow.py")
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, graph, input string
		exitCode           int
		run, hook          string
		failedNode         *string
		persistenceFailure bool
	}{
		{"collected failures", "collected", "2", 0, "succeeded", "succeeded", nil, false},
		{"succeeded", "checked", "2", 0, "succeeded", "succeeded", nil, false},
		{"failed step", "checked", "-1", 1, "failed", "succeeded", pointer("check"), false},
		{"failed hook", "broken_hook", "3", 0, "succeeded", "failed", nil, false},
		{"timed out hook", "timed_hook", "3", 0, "succeeded", "failed", nil, false},
		{"deadline", "stalled", "4", 1, "failed", "succeeded", pointer("stall"), false},
		{"hook persistence after success", "unwritable_hook", "5", 0, "succeeded", "succeeded", nil, true},
		{"hook persistence after failure", "unwritable_hook", "-2", 1, "failed", "succeeded", pointer("check"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.persistenceFailure && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("requires filesystem permission enforcement")
			}
			outcomeFile := filepath.Join(t.TempDir(), "outcome.json")
			runID := "hook-" + test.graph + test.input
			commandContext, stop := context.WithTimeout(t.Context(), 30*time.Second)
			defer stop()
			command := exec.CommandContext(commandContext, binary, "run", entry+"#"+test.graph, "--input="+test.input, "--store", store,
				"--project", "test/exit-hooks", "--run-id", runID, "--json")
			command.Args = append(command.Args, "--pass-env", "OUTCOME_FILE", "--pass-env", "STORE_ROOT")
			command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python, "OUTCOME_FILE="+outcomeFile, "STORE_ROOT="+store)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			started := time.Now()
			err := command.Run()
			if test.graph == "timed_hook" && time.Since(started) > 20*time.Second {
				t.Fatal("stalled exit hook did not return within its bounded lifetime")
			}
			var exit *exec.ExitError
			if code := 0; errors.As(err, &exit) {
				code = exit.ExitCode()
				if code != test.exitCode {
					t.Fatalf("exit %d, want %d\nstderr: %s", code, test.exitCode, stderr.String())
				}
			} else if err != nil || test.exitCode != 0 {
				t.Fatalf("exit = %v, want %d\nstderr: %s", err, test.exitCode, stderr.String())
			}
			var output runOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("run output %q: %v", stdout.String(), err)
			}
			if output.Status != test.run || output.ExitHook == nil || output.ExitHook.Status != test.hook {
				t.Fatalf("run output = %s", stdout.String())
			}

			if test.persistenceFailure != strings.Contains(output.ExitHook.PersistenceDiagnostic, "exit hook record could not be published") {
				t.Fatalf("hook persistence diagnostic = %q", output.ExitHook.PersistenceDiagnostic)
			}

			// The hook ran and saw the run's own outcome.
			body, err := os.ReadFile(outcomeFile)
			if err != nil {
				t.Fatalf("exit hook did not run: %v\nstderr: %s", err, stderr.String())
			}
			var outcome orchestrator.RunOutcome
			if err := json.Unmarshal(body, &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.RunID != runID || outcome.Status != test.run || (outcome.FailedNode == nil) != (test.failedNode == nil) ||
				test.failedNode != nil && *outcome.FailedNode != *test.failedNode {
				t.Fatalf("outcome = %s", body)
			}
			started, startErr := time.Parse(time.RFC3339, outcome.StartedAt)
			finished, finishErr := time.Parse(time.RFC3339, outcome.FinishedAt)
			if startErr != nil || finishErr != nil || finished.Before(started) {
				t.Fatalf("outcome times = %s", body)
			}

			inspected, err := exec.Command(binary, "inspect", runID, "--project", "test/exit-hooks",
				"--store", store, "--json").Output()
			if err != nil {
				t.Fatal(err)
			}
			var manifest runjournal.Manifest
			if err := json.Unmarshal(inspected, &manifest); err != nil {
				t.Fatal(err)
			}
			if test.persistenceFailure {
				if manifest.Status != test.run || manifest.ExitHook != nil {
					t.Fatalf("durable run changed after hook persistence failed: %s", inspected)
				}
				return
			}
			if test.graph == "collected" {
				found := false
				for _, step := range manifest.Steps {
					if step.ItemFailures == "collect" {
						found = true
						if step.Status != "succeeded" || step.Items == nil || len(*step.Items) != 2 || (*step.Items)[1].Status != "failed" {
							t.Fatalf("collected map lost its failed item: %s", inspected)
						}
					}
				}
				if !found {
					t.Fatalf("collected map missing from journal: %s", inspected)
				}
			}
			hook := manifest.ExitHook
			if manifest.Status != test.run || hook == nil || hook.Status != test.hook || len(hook.Attempts) != 1 {
				t.Fatalf("journal = %s", inspected)
			}
			if test.graph == "timed_hook" && !strings.Contains(hook.Attempts[0].Diagnostic, "2-second timeout") {
				t.Fatalf("hook timeout was not recorded: %s", inspected)
			}
			if (test.hook == "failed") != (hook.Attempts[0].Diagnostic != "") {
				t.Fatalf("exit hook attempt = %+v", hook.Attempts[0])
			}
		})
	}
}

func pointer(value string) *string { return &value }

// The Argo exit handler's command maps controller variables onto the same
// outcome and runs the hook in isolation, as a pod does.
func TestRuntimeExitHookMapsArgoVariablesOntoTheOutcome(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Fatal("install the Python SDK environment with uv sync --project packages/python")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "massive")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	source := `import os
from pathlib import Path
from massive import GraphBuilder, MapItemOutcome, RunOutcome, StepContext, container, execution

def check(ctx: StepContext[int]) -> int:
    return ctx.inputs

def record(ctx: StepContext[RunOutcome]) -> None:
    Path(os.environ["OUTCOME_FILE"]).write_text(ctx.inputs.model_dump_json())

graph = GraphBuilder(name="checked", input_type=int, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "1" * 64, platform="linux/amd64")))
graph.edge_from(graph.start).to(graph.add(check)).to_end(graph.end)
graph.on_exit(record)
`
	entry := filepath.Join(root, "workflow.py")
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "MASSIVE_PYTHON="+python)
	bundle := filepath.Join(root, "bundle")
	build := exec.Command(binary, "build", entry, "--output", bundle, "--namespace", "workflows",
		"--service-account", "runner", "--artifact-store", "artifacts")
	build.Env = environment
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build bundle: %v\n%s", err, output)
	}
	var configMap struct {
		BinaryData map[string][]byte `json:"binaryData"`
	}
	body, err := os.ReadFile(filepath.Join(bundle, "runtime-configmap.json"))
	if err != nil || json.Unmarshal(body, &configMap) != nil {
		t.Fatalf("runtime ConfigMap: %v", err)
	}
	mount := filepath.Join(root, "mount")
	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range configMap.BinaryData {
		if err := os.WriteFile(filepath.Join(mount, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := filepath.Join(root, "datastore.json")
	if err := os.WriteFile(descriptor, []byte(`{"kind":"local","path":"`+filepath.Join(root, "store")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(root, "store"), func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})

	outcomeFile := filepath.Join(root, "outcome.json")
	for _, marker := range []string{"absent", "unreadable"} {
		t.Run(marker, func(t *testing.T) {
			if marker == "unreadable" {
				key, err := orchestrator.FatalRunKey("argo/checked", "argo-run")
				if err != nil {
					t.Fatal(err)
				}
				// A directory at the object key produces a real datastore read error
				// without preventing the hook from publishing its own artifacts.
				if err := os.MkdirAll(filepath.Join(root, "store", key.String()), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(binary, "runtime", "exit-hook", "--plan", filepath.Join(mount, "massive-plan.json"),
				"--bundle-dir", mount, "--node=record", "--status=Failed",
				`--failures="[{\"displayName\":\"check\",\"templateName\":\"step-check\",\"phase\":\"Failed\",\"finishedAt\":\"2026-01-01T00:00:09Z\"}]"`,
				"--started-at=2026-01-01T00:00:00Z", "--output", filepath.Join(root, "result.json"),
				"--project", "argo/checked", "--run-id", "argo-run", "--datastore-config", descriptor)
			command.Env = append(environment, "OUTCOME_FILE="+outcomeFile)
			started := time.Now()
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("runtime exit-hook: %v\n%s", err, output)
			}
			if marker == "unreadable" && (!strings.Contains(string(output), "fatal marker unavailable after 3 reads; proceeding with fatal=null") || time.Since(started) < 750*time.Millisecond) {
				t.Fatalf("marker read failure did not retry and diagnose: %s", output)
			}
			body, err = os.ReadFile(outcomeFile)
			if err != nil {
				t.Fatal(err)
			}
			var outcome orchestrator.RunOutcome
			if err := json.Unmarshal(body, &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.Fatal != nil || outcome.RunID != "argo-run" || outcome.Status != "failed" || outcome.FailedNode == nil || *outcome.FailedNode != "check" || outcome.StartedAt != "2026-01-01T00:00:00Z" {
				t.Fatalf("outcome = %s", body)
			}
		})
	}
}
