package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/datastore"
)

func TestCancelledMapKeepsCompletedArtifactsAndTerminalJournal(t *testing.T) {
	useCancellationPython(t)
	root := t.TempDir()
	entry := filepath.Join(root, "workflow.py")
	source := `from pathlib import Path
import time
from pydantic import BaseModel, Field
from massive import GraphBuilder, StepContext, container, execution

class Request(BaseModel):
    ready: str
class Item(Request):
    index: int

def prepare(ctx: StepContext[None, Request]) -> list[Item]:
    return [Item(ready=ctx.inputs.ready, index=index) for index in range(5)]

def work(ctx: StepContext[None, Item]) -> int:
    if ctx.inputs.index == 1:
        Path(ctx.inputs.ready).write_text("started")
        time.sleep(60)
    return ctx.inputs.index

def collect(ctx: StepContext[None, list[int]]) -> int:
    return sum(ctx.inputs)

graph = GraphBuilder(name="cancellation", input_type=Request, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:"+"1"*64, platform="linux/amd64")))
prepared = graph.add(graph.step()(prepare))
items = graph.map(prepared, graph.step()(work), id="workers", concurrency=1)
collected = graph.add(graph.step()(collect))
graph.edge_from(graph.start).to(prepared)
graph.edge_from(items).to(collected)
graph.edge_from(collected).to(graph.end)
`
	if err := os.WriteFile(entry, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	frontend, err := Emit(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	input, err := json.Marshal(map[string]string{"ready": ready})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := cancelWhenReady(ctx, cancel, ready)
	storeRoot := writableStoreForTest(t)
	result, err := RunLocal(ctx, LocalRunRequest{Frontend: frontend, Input: input, Store: storeRoot, Project: "test/cancellation", RunID: "cancelled-map"})
	if observedErr := <-observed; observedErr != nil {
		t.Fatal(observedErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want cancellation", err)
	}
	if result == nil || result.Run == nil || result.Run.Status != "cancelled" {
		t.Fatalf("cancelled result = %#v", result)
	}
	journal, err := Inspect(t.Context(), storeRoot, "test/cancellation", "cancelled-map")
	if err != nil {
		t.Fatal(err)
	}
	if journal.Status != "cancelled" || journal.SchemaVersion != 4 || journal.Encoding != "json-v4" {
		t.Fatalf("journal = %#v", journal)
	}
	statuses := map[string]string{}
	for _, step := range journal.Steps {
		statuses[step.NodeID] = step.Status
	}
	if statuses["prepare"] != "succeeded" || statuses["workers"] != "cancelled" || statuses["collect"] != "not-started" {
		t.Fatalf("steps = %v", statuses)
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range journal.Steps {
		if step.NodeID != "workers" {
			continue
		}
		if step.Items == nil || len(*step.Items) != 5 {
			t.Fatal("map cardinality lost")
		}
		for index, item := range *step.Items {
			want := "not-started"
			if index == 0 {
				want = "succeeded"
			}
			if index == 1 {
				want = "cancelled"
			}
			if item.Status != want {
				t.Fatalf("item %d = %s, want %s", index, item.Status, want)
			}
			if index > 1 && len(item.Attempts) != 0 {
				t.Fatalf("undispatched item %d has an attempt", index)
			}
		}
		if step.Attempts[0].Diagnostic != "map execution cancelled" {
			t.Fatalf("cancelled map diagnostic = %q", step.Attempts[0].Diagnostic)
		}
		if step.Attempts[0].Output != nil {
			t.Fatal("cancelled map advertises a collection")
		}
		collectionKey := "projects/" + journal.ProjectKey + "/runs/" + journal.RunID + "/steps/workers/1/output-manifest.json"
		if _, err := store.Get(t.Context(), datastore.MustKey(collectionKey)); !errors.Is(err, datastore.ErrNotFound) {
			t.Fatal("cancelled map published a collection manifest")
		}
		first := (*step.Items)[0]
		if len(first.Attempts) != 1 || first.Attempts[0].Output == nil {
			t.Fatal("completed item lost its output")
		}
		body, err := store.Get(t.Context(), datastore.MustKey(first.Attempts[0].Output.Body.Key))
		if err != nil || string(body.Body) != "0" {
			t.Fatalf("completed item body = %#v, %v", body, err)
		}
	}
}

func useCancellationPython(t *testing.T) {
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
	t.Setenv("MASSIVE_PYTHON", python)
}

// A filesystem handshake proves the real runner entered author code before
// cancellation; timing alone would only test process startup nondeterministically.
func cancelWhenReady(ctx context.Context, cancel context.CancelFunc, ready string) <-chan error {
	observed := make(chan error, 1)
	go func() {
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.C:
				cancel()
				observed <- errors.New("task never reached readiness marker")
				return
			case <-ctx.Done():
				observed <- ctx.Err()
				return
			case <-ticker.C:
				if _, err := os.Stat(ready); err == nil {
					cancel()
					observed <- nil
					return
				}
			}
		}
	}()
	return observed
}

func TestCancelledStepAndDecisionBranch(t *testing.T) {
	useCancellationPython(t)
	for _, decision := range []bool{false, true} {
		name := "linear"
		if decision {
			name = "decision"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			entry := filepath.Join(root, "workflow.py")
			source := `from pathlib import Path
import time
from typing import Annotated, Literal
from pydantic import BaseModel, Field
from massive import GraphBuilder, StepContext, container, execution
class Request(BaseModel):
    ready: str
class Active(Request):
    kind: Literal["active"] = "active"
class Inactive(Request):
    kind: Literal["inactive"] = "inactive"
def classify(ctx: StepContext[None, Request]) -> Annotated[Active | Inactive, Field(discriminator="kind")]:
    return Active(ready=ctx.inputs.ready)
def work(ctx: StepContext[None, Active]) -> int:
    Path(ctx.inputs.ready).write_text("started")
    time.sleep(60)
    return 1
def other(ctx: StepContext[None, Inactive]) -> int:
    Path(ctx.inputs.ready + ".inactive").write_text("must not run")
    return 2
def linear(ctx: StepContext[None, Request]) -> int:
    Path(ctx.inputs.ready).write_text("started")
    time.sleep(60)
    return 1
def collect(ctx: StepContext[None, int]) -> int:
    return ctx.inputs
graph = GraphBuilder(name="cancellation", input_type=Request, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:"+"1"*64, platform="linux/amd64")))
collected = graph.add(graph.step()(collect))
`
			if decision {
				source += `classified = graph.add(graph.step()(classify))
active = graph.add(graph.step()(work))
inactive = graph.add(graph.step()(other))
route = graph.decision(classified, on="kind", id="route")
active_input = route.case(Active)
inactive_input = route.case(Inactive)
selected = route.select(int, active=active, inactive=inactive)
graph.edge_from(graph.start).to(classified)
graph.edge_from(active_input).to(active)
graph.edge_from(inactive_input).to(inactive)
graph.edge_from(selected).to(collected)
`
			} else {
				source += `blocked = graph.add(graph.step()(linear))
graph.edge_from(graph.start).to(blocked)
graph.edge_from(blocked).to(collected)
`
			}
			source += "graph.edge_from(collected).to(graph.end)\n"
			if err := os.WriteFile(entry, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			frontend, err := Emit(t.Context(), entry)
			if err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(root, "ready")
			input, err := json.Marshal(map[string]string{"ready": ready})
			if err != nil {
				t.Fatal(err)
			}
			store := writableStoreForTest(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observed := cancelWhenReady(ctx, cancel, ready)
			result, err := RunLocal(ctx, LocalRunRequest{Frontend: frontend, Input: input, Store: store, Project: "test/cancellation", RunID: name})
			if observedErr := <-observed; observedErr != nil {
				t.Fatal(observedErr)
			}
			if !errors.Is(err, context.Canceled) || result == nil || result.Run == nil || result.Run.Status != "cancelled" {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			journal, err := Inspect(t.Context(), store, "test/cancellation", name)
			if err != nil {
				t.Fatal(err)
			}
			blocked := "linear"
			if decision {
				blocked = "work"
			}
			statuses := map[string]string{}
			for _, step := range journal.Steps {
				statuses[step.NodeID] = step.Status
				if step.NodeID == blocked && (len(step.Attempts) != 1 || step.Attempts[0].Status != "cancelled" || step.Attempts[0].Output != nil) {
					t.Fatalf("cancelled attempt=%#v", step)
				}
			}
			if statuses[blocked] != "cancelled" || statuses["collect"] != "not-started" || journal.Result != nil {
				t.Fatalf("terminal journal=%#v", journal)
			}
			if decision {
				if len(journal.Decisions) != 1 || journal.Decisions[0].SelectedCase != "active" {
					t.Fatalf("decision lost: %#v", journal.Decisions)
				}
				if statuses["classify"] != "succeeded" || (statuses["other"] != "skipped" && statuses["other"] != "not-started") {
					t.Fatalf("branch statuses=%v", statuses)
				}
				if _, err := os.Stat(ready + ".inactive"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("inactive branch ran: %v", err)
				}
			}
		})
	}
}

func TestCancelledBeforeDispatchDoesNotCreateRun(t *testing.T) {
	useCancellationPython(t)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	frontend, err := Emit(t.Context(), filepath.Join(root, "conformance", "workflows", "python-linear", "workflow.py"))
	if err != nil {
		t.Fatal(err)
	}
	store := writableStoreForTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = RunLocal(ctx, LocalRunRequest{Frontend: frontend, Input: []byte(`{"value":1}`), Store: store, Project: "test/cancellation", RunID: "before-dispatch"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled run: %v", err)
	}
	if _, err := Inspect(t.Context(), store, "test/cancellation", "before-dispatch"); err == nil {
		t.Fatal("pre-cancelled run published a journal")
	}
}

func TestFailedRunTerminalJournalExcludesRunnerOutput(t *testing.T) {
	useCancellationPython(t)
	entry := filepath.Join(t.TempDir(), "workflow.py")
	source := `from massive import GraphBuilder, StepContext, container, execution
def fail(ctx: StepContext[None, int]) -> int:
    raise RuntimeError("private-runner-output-sentinel")
def next_step(ctx: StepContext[None, int]) -> int:
    return ctx.inputs
graph = GraphBuilder(name="failure", input_type=int, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:"+"1"*64, platform="linux/amd64")))
failed = graph.add(graph.step()(fail))
next_node = graph.add(graph.step()(next_step))
graph.edge_from(graph.start).to(failed)
graph.edge_from(failed).to(next_node)
graph.edge_from(next_node).to(graph.end)
`
	if err := os.WriteFile(entry, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	frontend, err := Emit(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	store := writableStoreForTest(t)
	result, err := RunLocal(t.Context(), LocalRunRequest{Frontend: frontend, Input: []byte("1"), Store: store, Project: "test/failure", RunID: "failed"})
	if err == nil || !strings.Contains(err.Error(), "private-runner-output-sentinel") {
		t.Fatalf("caller lost runner diagnostic: %v", err)
	}
	if result == nil || result.Run == nil || result.Run.Status != "failed" {
		t.Fatalf("result=%#v", result)
	}
	journal, err := Inspect(t.Context(), store, "test/failure", "failed")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private-runner-output-sentinel") {
		t.Fatal("shared journal contains runner output")
	}
	statuses := map[string]string{}
	for _, step := range journal.Steps {
		statuses[step.NodeID] = step.Status
	}
	if statuses["fail"] != "failed" || statuses["next_step"] != "not-started" {
		t.Fatalf("terminal steps=%v", statuses)
	}
}
