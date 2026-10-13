package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/runevent"
	"github.com/Sly1029/massive/internal/runjournal"
)

func progressCLI(t *testing.T) (string, string, string) {
	t.Helper()
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(repository, "packages", "python", ".venv", "Scripts", "python.exe")
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatal("install the Python SDK environment with uv sync --project packages/python")
	}
	binary := filepath.Join(t.TempDir(), "massive")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	return binary, python, repository
}

func progressStore(t *testing.T) string {
	t.Helper()
	store := filepath.Join(t.TempDir(), "store")
	t.Cleanup(func() {
		_ = filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	return store
}

func TestRunProgressEventsMatchRealPythonJournals(t *testing.T) {
	binary, python, repository := progressCLI(t)
	for _, test := range []struct{ name, workflow, input, expected string }{
		{"linear", "python-linear", `{"value":41}`, `{"value":42}`},
		{"fan-in", "python-fan-in", `{"value":3}`, `{"parts":["Doubled=6","Squared=9","|","Doubled=6","Squared=9"]}`},
		{"collected failures", "python-map-outcomes", `{"tasks":[{"name":"ok","behavior":"ok"},{"name":"flaky","behavior":"flaky"},{"name":"bad","behavior":"raise"},{"name":"refuse","behavior":"refuse"}]}`, ""},
		{"empty map", "python-map-outcomes", `{"tasks":[]}`, `{"scanned":[],"failed":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, events := progressStore(t), filepath.Join(t.TempDir(), "events.ndjson")
			workflow := filepath.Join(repository, "conformance", "workflows", test.workflow, "workflow.py") + "#graph"
			command := exec.Command(binary, "run", workflow, "--project", "test/progress", "--store", store, "--run-id", "observed", "--input="+test.input, "--json", "--progress", "plain", "--events", events)
			command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err != nil {
				t.Fatalf("run: %v\n%s", err, &stderr)
			}
			sequence := readProgressEvents(t, events)
			journal := assertProgressJournal(t, store, "test/progress", "observed", sequence)
			assertProgressJSON(t, stdout.Bytes(), "observed", "succeeded", test.expected)
			for _, line := range []string{"run observed started (", " attempt 1/", " succeeded (", "STEP                     STATUS          DURATION", "run observed succeeded; total wall time "} {
				if !strings.Contains(stderr.String(), line) {
					t.Fatalf("plain stderr missing %q:\n%s", line, &stderr)
				}
			}
			if strings.ContainsAny(stderr.String(), "\r\x1b") {
				t.Fatalf("plain mode used terminal control sequences: %q", stderr.String())
			}
			if test.name == "collected failures" {
				var last *runevent.Event
				retry := false
				for i := range sequence {
					event := &sequence[i]
					if event.Type == "map.progress" {
						last = event
					}
					if event.Type == "step.attempt.started" && event.NodeID == "scan" && event.Attempt == 2 {
						retry = true
					}
				}
				if !retry || last == nil || *last.Completed != 4 || *last.Total != 4 || *last.Failed != 2 {
					t.Fatalf("missing retries or final map counts: %+v", last)
				}
				if !strings.Contains(stderr.String(), "scan map 4/4 completed, 2 failed") || !strings.Contains(stderr.String(), "diagnostic: step-execution-failure") {
					t.Fatalf("missing failure progress: %s", &stderr)
				}
				// Failure detail belongs only to the local event/log stream.
				for _, step := range journal.Steps {
					if step.Items != nil {
						for _, item := range *step.Items {
							for _, attempt := range item.Attempts {
								if strings.Contains(attempt.Diagnostic, "cannot be parsed") {
									t.Fatal("author message leaked into journal")
								}
							}
						}
					}
				}
			}
			// The same workflow with progress disabled produces exactly the same run
			// JSON shape and value, with an independent durable run identity.
			silent := exec.Command(binary, "run", workflow, "--project", "test/progress", "--store", store, "--run-id", "silent", "--input="+test.input, "--json", "--progress", "none")
			silent.Env = command.Env
			var silentOut, silentErr bytes.Buffer
			silent.Stdout, silent.Stderr = &silentOut, &silentErr
			if err := silent.Run(); err != nil {
				t.Fatalf("silent run: %v\n%s", err, &silentErr)
			}
			if silentErr.Len() != 0 {
				t.Fatalf("none mode wrote progress: %s", &silentErr)
			}
			var observedJSON, silentJSON map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &observedJSON); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(silentOut.Bytes(), &silentJSON); err != nil {
				t.Fatal(err)
			}
			delete(observedJSON, "runId")
			delete(silentJSON, "runId")
			if !reflect.DeepEqual(observedJSON, silentJSON) {
				t.Fatalf("progress changed stdout JSON: %s vs %s", &stdout, &silentOut)
			}
		})
	}
}

func readProgressEvents(t *testing.T, path string) []runevent.Event {
	t.Helper()
	body := readFile(t, path)
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Fatalf("event stream has an incomplete line: %s", body)
	}
	var events []runevent.Event
	for _, line := range bytes.Split(bytes.TrimSuffix(body, []byte("\n")), []byte("\n")) {
		event, err := runevent.Parse(line)
		if err != nil {
			t.Fatalf("event failed JSON Schema validation: %v\n%s", err, line)
		}
		events = append(events, *event)
	}
	return events
}

type progressAttempt struct {
	node          string
	item, attempt int
}

// Compare every emitted terminal attempt/step with the actual durable journal,
// including collected failures, and check temporal/identity ordering independently.
func assertProgressJournal(t *testing.T, store, project, runID string, events []runevent.Event) *runjournal.Manifest {
	t.Helper()
	path := filepath.Join(store, "projects", orchestrator.NormalizeProjectKey(project), "runs", runID, "run-manifest.json")
	journal, err := runjournal.Parse(readFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Type != "run.started" || events[len(events)-1].Type != "run.finished" {
		t.Fatal("run events are not bracketed by started/finished")
	}
	first, last := events[0], events[len(events)-1]
	if first.PlanHash != journal.PlanHash || *first.NodeCount != len(journal.Steps) || last.Status != journal.Status {
		t.Fatal("run events disagree with journal")
	}
	steps := map[string]string{}
	starts := map[string]bool{}
	attempts := map[progressAttempt]string{}
	attemptStarts := map[progressAttempt]bool{}
	latest := map[progressAttempt]int{}
	maps := map[string]runevent.Event{}
	elapsed := 0.0
	for index, event := range events {
		if event.Sequence != index+1 || event.RunID != runID || event.ElapsedMS < elapsed {
			t.Fatalf("non-monotonic event: %+v", event)
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Time); err != nil {
			t.Fatal(err)
		}
		elapsed = event.ElapsedMS
		item := -1
		if event.ItemIndex != nil {
			item = *event.ItemIndex
		}
		identity := progressAttempt{event.NodeID, item, event.Attempt}
		switch event.Type {
		case "step.started":
			if starts[event.NodeID] {
				t.Fatal("duplicate step start")
			}
			starts[event.NodeID] = true
		case "step.attempt.started":
			if !starts[event.NodeID] {
				t.Fatalf("attempt preceded step start: %+v", event)
			}
			owner := progressAttempt{event.NodeID, item, 0}
			if event.Attempt != latest[owner]+1 || event.Attempt > event.MaxAttempts {
				t.Fatalf("non-dense attempt: %+v", event)
			}
			if event.Attempt > 1 && attempts[progressAttempt{event.NodeID, item, event.Attempt - 1}] != "failed" {
				t.Fatal("retry preceded failed attempt")
			}
			latest[owner] = event.Attempt
			attemptStarts[identity] = true
		case "step.attempt.finished":
			if !attemptStarts[identity] || attempts[identity] != "" {
				t.Fatalf("attempt finished without one start: %+v", event)
			}
			attempts[identity] = event.Status
		case "step.finished":
			if event.Status != "not-started" && event.Status != "skipped" && !starts[event.NodeID] {
				t.Fatalf("step finished without starting: %+v", event)
			}
			if _, exists := steps[event.NodeID]; exists {
				t.Fatal("duplicate step finish")
			}
			steps[event.NodeID] = event.Status
		case "map.progress":
			if previous, exists := maps[event.NodeID]; exists && (*event.Completed < *previous.Completed || *event.Failed < *previous.Failed || *event.Total != *previous.Total) {
				t.Fatal("map progress moved backwards")
			}
			if *event.Completed > *event.Total || *event.Failed > *event.Completed {
				t.Fatal("invalid map counts")
			}
			maps[event.NodeID] = event
		}
	}
	expectedAttempts := map[progressAttempt]string{}
	checkStep := func(step runjournal.Step) {
		if steps[step.NodeID] != step.Status {
			t.Fatalf("step %s event status = %q, journal = %q", step.NodeID, steps[step.NodeID], step.Status)
		}
		for _, attempt := range step.Attempts {
			expectedAttempts[progressAttempt{step.NodeID, -1, attempt.Attempt}] = attempt.Status
		}
		if step.Items != nil {
			failed := 0
			for _, item := range *step.Items {
				if item.Status == "failed" {
					failed++
				}
				for _, attempt := range item.Attempts {
					expectedAttempts[progressAttempt{step.NodeID, item.Index, attempt.Attempt}] = attempt.Status
				}
			}
			if len(step.Attempts) > 0 {
				progress, ok := maps[step.NodeID]
				if !ok || *progress.Total != len(*step.Items) || *progress.Completed != len(*step.Items) || *progress.Failed != failed {
					t.Fatalf("map %s final progress disagrees with journal: %+v", step.NodeID, progress)
				}
			}
		}
	}
	for _, step := range journal.Steps {
		checkStep(step)
	}
	if journal.ExitHook != nil {
		checkStep(*journal.ExitHook)
		if last.ExitHook == nil || last.ExitHook.NodeID != journal.ExitHook.NodeID || last.ExitHook.Status != journal.ExitHook.Status {
			t.Fatal("exit hook event disagrees with journal")
		}
	}
	if !reflect.DeepEqual(attempts, expectedAttempts) {
		t.Fatalf("attempt events disagree with journal:\n%v\n%v", attempts, expectedAttempts)
	}
	if len(attemptStarts) != len(attempts) {
		t.Fatal("unterminated event attempt")
	}
	return journal
}

func assertProgressJSON(t *testing.T, body []byte, runID, status, expectedResult string) {
	t.Helper()
	var output map[string]json.RawMessage
	if err := json.Unmarshal(body, &output); err != nil {
		t.Fatalf("stdout is not exactly one JSON result: %v\n%s", err, body)
	}
	if string(output["runId"]) != fmt.Sprintf("%q", runID) || string(output["status"]) != fmt.Sprintf("%q", status) {
		t.Fatalf("unexpected run JSON: %s", body)
	}
	expectedFields := 2
	if status == "succeeded" {
		expectedFields++
	}
	if output["exitHook"] != nil {
		expectedFields++
	}
	if len(output) != expectedFields {
		t.Fatalf("stdout contract changed: %s", body)
	}
	if expectedResult != "" {
		var expected, actual any
		if err := json.Unmarshal([]byte(expectedResult), &expected); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(output["result"], &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("result = %s, want %s", output["result"], expectedResult)
		}
	}
}

func TestRunMapProgressAndFailureTailAreLive(t *testing.T) {
	binary, python, _ := progressCLI(t)
	root, store := t.TempDir(), progressStore(t)
	workflow := filepath.Join(root, "workflow.py")
	source := `import os
import time
from pathlib import Path
from massive import GraphBuilder, StepContext, MapItemOutcome, NonRetryableError, container, execution

root = Path(os.environ["MASSIVE_PROGRESS_HANDSHAKE"])
def task(ctx: StepContext[int]) -> int:
    if ctx.inputs == 1:
        (root / "ready").touch()
        while not (root / "release").exists():
            time.sleep(0.02)
        return 42
    while not (root / "ready").exists():
        time.sleep(0.02)
    print("discarded-prefix\n" + "x" * (2 * 1024 * 1024), flush=True)
    print("failure-tail-sentinel", flush=True)
    raise NonRetryableError("failure-diagnostic-sentinel")

graph = GraphBuilder(name="live-progress", input_type=list[int], output_type=list[MapItemOutcome[int]],
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "0" * 64)))
items = graph.map(graph.start, task, id="items", item_failures="collect", concurrency=2)
graph.edge_from(items).to_end(graph.end)
`
	if err := os.WriteFile(workflow, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	eventsPath, stderrPath := filepath.Join(root, "events.ndjson"), filepath.Join(root, "stderr.log")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "run", workflow+"#graph", "--project", "test/progress", "--store", store, "--run-id", "live", "--input", "[0,1]", "--json", "--events", eventsPath)
	command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python, "MASSIVE_PROGRESS_HANDSHAKE="+root)
	var stdout bytes.Buffer
	command.Stdout, command.Stderr = &stdout, stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait for complete, schema-valid lines while the slow item is held open.
	// No running process or writer is replaced with a test double.
	deadline := time.Now().Add(20 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(eventsPath)
		for _, line := range bytes.Split(body, []byte("\n")) {
			event, err := runevent.Parse(line)
			if err == nil && event.Type == "map.progress" && *event.Completed == 1 && *event.Total == 2 && *event.Failed == 1 {
				observed = true
			}
		}
		logs, _ := os.ReadFile(stderrPath)
		if observed && bytes.Contains(logs, []byte("failure-tail-sentinel")) && bytes.Contains(logs, []byte("failure-diagnostic-sentinel")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	logs, _ := os.ReadFile(stderrPath)
	if !observed || !bytes.Contains(logs, []byte("failure-tail-sentinel")) || !bytes.Contains(logs, []byte("failure-diagnostic-sentinel")) {
		cancel()
		_ = command.Wait()
		t.Fatalf("failure/progress did not arrive before releasing sibling:\n%s", logs)
	}
	if bytes.Contains(logs, []byte("discarded-prefix")) {
		t.Fatal("failure tail retained the discarded log prefix")
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0o600); err != nil {
		cancel()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("live map run: %v\n%s", err, readFile(t, stderrPath))
	}
	events := readProgressEvents(t, eventsPath)
	assertProgressJournal(t, store, "test/progress", "live", events)
	assertProgressJSON(t, stdout.Bytes(), "live", "succeeded", "")
}

func TestRunEventPathsAndProgressModesAreValidated(t *testing.T) {
	binary, _, _ := progressCLI(t)
	root := t.TempDir()
	existing := filepath.Join(root, "existing.ndjson")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{
		{"--events", "-"}, {"--events", existing}, {"--progress", "invalid"},
	} {
		command := exec.Command(binary, append([]string{"run", "absent.py", "--json"}, flags...)...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err == nil || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("invalid flags accepted or polluted stdout: %v\n%s\n%s", err, &stdout, &stderr)
		}
	}
	if string(readFile(t, existing)) != "keep me" {
		t.Fatal("event output overwrote an existing file")
	}
}

func TestRunTerminalEventsIncludeFailureAndExitHook(t *testing.T) {
	binary, python, _ := progressCLI(t)
	root := t.TempDir()
	workflow := filepath.Join(root, "workflow.py")
	source := `from datetime import timedelta
import time
from massive import GraphBuilder, StepContext, RunOutcome, NonRetryableError, container, execution

DEFAULTS = execution(environment=container("example.invalid/runner@sha256:" + "0" * 64))
def task(ctx: StepContext[int]) -> int:
    if ctx.inputs == -1:
        print("terminal-log-sentinel", flush=True)
        raise NonRetryableError("terminal-diagnostic-sentinel")
    if ctx.inputs == -2:
        time.sleep(30)
    return ctx.inputs

def after(ctx: StepContext[int]) -> int:
    return ctx.inputs

def hook(ctx: StepContext[RunOutcome]) -> None:
    if ctx.inputs.status == "succeeded":
        raise NonRetryableError("hook-failure-sentinel")

graph = GraphBuilder(name="terminal-progress", input_type=int, output_type=int, defaults=DEFAULTS,
    deadline=timedelta(seconds=3))
graph.edge_from(graph.start).to(graph.add(task)).to(graph.add(after)).to_end(graph.end)
graph.on_exit(hook)

def sum_items(ctx: StepContext[list[int]]) -> int:
    return sum(ctx.inputs)

failed_map = GraphBuilder(name="failed-map-progress", input_type=list[int], output_type=int, defaults=DEFAULTS)
items = failed_map.map(failed_map.start, task, id="tasks", concurrency=2)
failed_map.edge_from(items).to(failed_map.add(sum_items)).to_end(failed_map.end)
failed_map.on_exit(hook)
`
	if err := os.WriteFile(workflow, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, graph, input, status, hook, failedNode string }{
		{"failed task", "graph", "-1", "failed", "succeeded", "task"},
		{"failed map", "failed_map", "[-1,1]", "failed", "succeeded", "tasks"},
		{"failed hook", "graph", "1", "succeeded", "failed", ""},
		{"deadline", "graph", "-2", "failed", "succeeded", "task"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, eventsPath := progressStore(t), filepath.Join(t.TempDir(), "events.ndjson")
			command := exec.Command(binary, "run", workflow+"#"+test.graph, "--project", "test/progress", "--store", store, "--run-id", "terminal", "--input="+test.input, "--json", "--progress", "plain", "--events", eventsPath)
			command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			if (err == nil) != (test.status == "succeeded") {
				t.Fatalf("run: %v\n%s", err, &stderr)
			}
			events := readProgressEvents(t, eventsPath)
			journal := assertProgressJournal(t, store, "test/progress", "terminal", events)
			assertProgressJSON(t, stdout.Bytes(), "terminal", test.status, "")
			last := events[len(events)-1]
			if last.ExitHook == nil || last.ExitHook.Status != test.hook {
				t.Fatalf("exit hook missing from completion: %+v", last)
			}
			if test.status == "failed" {
				if last.FailedNode == nil || *last.FailedNode != test.failedNode {
					t.Fatalf("failed node = %+v", last.FailedNode)
				}
				if journal.Steps[1].Status != "not-started" {
					t.Fatal("queued task was incorrectly dispatched")
				}
			}
			if (test.name == "failed task" || test.name == "failed map") && (!strings.Contains(stderr.String(), "terminal-log-sentinel") || !strings.Contains(stderr.String(), "terminal-diagnostic-sentinel")) {
				t.Fatalf("missing failure context: %s", &stderr)
			}
			if test.name == "failed map" {
				collectionFailure := false
				for _, event := range events {
					if event.Type == "step.attempt.finished" && event.NodeID == "tasks" && event.ItemIndex == nil {
						collectionFailure = true
						if event.Classification != "error" {
							t.Fatalf("map failure mislabeled: %+v", event)
						}
					}
				}
				if !collectionFailure {
					t.Fatal("missing map collection failure")
				}
			}
			// Validate strict consumer behavior using a real event as the base record.
			line, err := json.Marshal(events[0])
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			for _, mutation := range []string{"version", "unknown field", "missing time"} {
				altered := make(map[string]any, len(record))
				for key, value := range record {
					altered[key] = value
				}
				switch mutation {
				case "version":
					altered["schemaVersion"] = 0
				case "unknown field":
					altered["unexpected"] = true
				case "missing time":
					delete(altered, "time")
				}
				malformed, err := json.Marshal(altered)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := runevent.Parse(malformed); err == nil {
					t.Fatalf("schema accepted %s", mutation)
				}
			}
		})
	}
}
