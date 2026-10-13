package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/internal/executionphase"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/valueparam"
)

func TestArgoFatalRuntimePublishesSafeCauseGuardsDispatchAndInvokesHook(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages/python/.venv/bin/python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable")
	}
	t.Setenv("MASSIVE_PYTHON", python)
	root := t.TempDir()
	source := filepath.Join(root, "workflow.py")
	outcomePath := filepath.Join(root, "outcome.json")
	authorPath := filepath.Join(root, "started")
	if err := os.WriteFile(source, []byte(fmt.Sprintf(`from pathlib import Path
from massive import FatalRunError, GraphBuilder, MapItemOutcome, RunOutcome, StepContext, container, execution, retry

def refuse(ctx: StepContext[int]) -> int:
    Path(%q).write_text(str(ctx.inputs))
    if ctx.inputs == 0:
        raise FatalRunError("retention.refused", "Retention gate refused processing") from RuntimeError("SECRET exception chain")
    return ctx.inputs

def notify(ctx: StepContext[RunOutcome]) -> None:
    Path(%q).write_text(ctx.inputs.model_dump_json())

def collect(ctx: StepContext[list[MapItemOutcome[int]]]) -> int: return len(ctx.inputs)

defaults = execution(environment=container("example.invalid/runner@sha256:" + "0"*64), retry=retry(3))
graph = GraphBuilder(name="fatal-step", input_type=int, output_type=int, defaults=defaults)
graph.edge_from(graph.start).to(graph.add(refuse)).to_end(graph.end)
graph.on_exit(notify)
mapped = GraphBuilder(name="fatal-map", input_type=list[int], output_type=int, defaults=defaults)
outcomes = mapped.map(mapped.start, refuse, id="refuse", item_failures="collect", concurrency=2)
mapped.edge_from(outcomes).to(mapped.add(collect)).to_end(mapped.end)
mapped.on_exit(notify)
`, authorPath, outcomePath)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, collecting := range []bool{false, true} {
		t.Run(fmt.Sprint(collecting), func(t *testing.T) {
			entry := source + "#graph"
			if collecting {
				entry = source + "#mapped"
			}
			bundle := filepath.Join(t.TempDir(), "bundle")
			if err := (&BuildCommand{Entry: entry, Output: bundle, Namespace: "workflows", ServiceAccount: "massive-runner", ArtifactStore: "massive-artifacts", RuntimeTransport: "embedded-v0", Profile: "argo", Target: "argo"}).Run(t.Context(), &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			config := localDatastoreConfig(t)
			planPath := filepath.Join(bundle, "massive-plan.json")
			sources := RuntimeSources{BundleDir: filepath.Join(bundle, "runtime-assets")}
			output, termination := filepath.Join(root, "result.json"), filepath.Join(root, "termination.json")
			if collecting {
				err = (&RuntimeMapItemCommand{Plan: planPath, RuntimeSources: sources, Node: "refuse", Item: `{"index":0,"value":0}`, Output: output, Project: "fatal-runtime", RunID: "run", DatastoreConfig: config, TerminationLog: termination}).Run(t.Context())
				if string(readFile(t, output)) != valueparam.FatalItemEnvelope {
					t.Fatal("fatal collected as outcome")
				}
			} else {
				err = (&RuntimeStepCommand{Plan: planPath, RuntimeSources: sources, Node: "refuse", WorkflowInput: "0", Output: output, Project: "fatal-runtime", RunID: "run", DatastoreConfig: config, TerminationLog: termination}).Run(t.Context())
			}
			var fatal *orchestrator.FatalRunError
			if !errors.As(err, &fatal) || exitCodeFor(err) != 69 {
				t.Fatalf("fatal runtime exit: %v", err)
			}
			codec, _, err := openRuntimeDatastore(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			key, err := orchestrator.FatalRunKey("fatal-runtime", "run")
			if err != nil {
				t.Fatal(err)
			}
			cause, err := orchestrator.ReadFatalRun(t.Context(), codec.Store, key)
			if err != nil || cause == nil || *cause != fatal.Failure {
				t.Fatalf("durable cause: %+v %v", cause, err)
			}
			evidence, err := executionphase.Parse(string(readFile(t, termination)))
			if err != nil || !evidence.Fatal {
				t.Fatalf("fatal termination evidence: %+v %v", evidence, err)
			}
			// A later pod must reject the marker before entering author code.
			err = (&RuntimeStepCommand{Plan: planPath, RuntimeSources: sources, Node: "refuse", WorkflowInput: "1", Output: output, Project: "fatal-runtime", RunID: "run", DatastoreConfig: config}).Run(t.Context())
			if !errors.As(err, &fatal) || string(readFile(t, authorPath)) != "0" {
				t.Fatalf("dispatch continued: %v", err)
			}
			err = (&RuntimeExitHookCommand{Plan: planPath, RuntimeSources: sources, Node: "notify", Status: "Failed", Failures: `"[]"`, StartedAt: "2026-01-01T00:00:00Z", Output: output, Project: "fatal-runtime", RunID: "run", DatastoreConfig: config}).Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var outcome orchestrator.RunOutcome
			if err := json.Unmarshal(readFile(t, outcomePath), &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.FailureKind == nil || *outcome.FailureKind != "fatal" || outcome.Category == nil || *outcome.Category != cause.Category || outcome.Message == nil || *outcome.Message != cause.Message || outcome.FailedNode == nil || *outcome.FailedNode != "refuse" {
				t.Fatalf("fatal hook input: %+v", outcome)
			}
		})
	}
}

func TestFatalWatcherProbeUsesTheRealReadinessFile(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	command := &RuntimeWatchFatalCommand{Probe: true, ReadyFile: ready}
	if err := command.Run(t.Context()); !os.IsNotExist(err) {
		t.Fatalf("premature readiness: %v", err)
	}
	if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
}
