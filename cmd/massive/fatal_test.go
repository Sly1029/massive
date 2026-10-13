package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/artifact"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/executionphase"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/runjournal"
	"github.com/Sly1029/massive/internal/valueparam"
)

func TestRuntimeSettlementPreservesPublishedSuccessAfterFatalCancellation(t *testing.T) {
	_, store := localDatastore(t)
	project, runID := "fatal-settlement", "run"
	key, err := orchestrator.FatalRunKey(project, runID)
	if err != nil {
		t.Fatal(err)
	}
	schema := []byte(`{"type":"integer"}`)
	schemaHash := canonical.DigestBytes(schema)
	if _, err := store.Put(t.Context(), datastore.MustKey("blobs/sha256/"+strings.TrimPrefix(schemaHash, "sha256:")), schema, datastore.PutOptions{ContentType: artifact.JSONContentType}); err != nil {
		t.Fatal(err)
	}
	producer := artifact.Producer{ProjectKey: orchestrator.NormalizeProjectKey(project), RunID: runID, NodeID: "completed", Attempt: 1, PlanHash: "sha256:" + strings.Repeat("a", 64)}
	destination := artifact.Destination{ManifestKey: datastore.MustKey("projects/" + producer.ProjectKey + "/runs/" + runID + "/steps/completed/1/output-manifest.json"), Schema: schemaHash}
	if _, err := artifact.PublishJSON(t.Context(), store, destination, producer, []byte("42")); err != nil {
		t.Fatal(err)
	}
	_, result, err := artifact.ResolveJSON(t.Context(), store, destination, producer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := &orchestrator.FatalRunError{Failure: runjournal.FatalFailure{Message: "Sibling failed", NodeID: "sibling"}}
	if err := orchestrator.PublishFatalRun(t.Context(), store, key, cause.Failure); err != nil {
		t.Fatal(err)
	}
	cancel(cause)
	got, err := settleRuntimeInvocation(ctx, store, key, false, result, nil)
	if err != nil || string(got) != "42" {
		t.Fatalf("published success discarded after sibling fatal: %s %v", got, err)
	}
	// Failed invocations still inherit the sibling's safe cause and exit 69.
	got, err = settleRuntimeInvocation(ctx, store, key, false, nil, context.Canceled)
	var fatal *orchestrator.FatalRunError
	if got != nil || !errors.As(err, &fatal) || fatal.Failure != cause.Failure || exitCodeFor(err) != 69 {
		t.Fatalf("failed invocation lost fatal cause: %s %v", got, err)
	}
}

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
import time
from massive import FatalRunError, GraphBuilder, MapItemOutcome, RunOutcome, StepContext, container, execution, retry

def refuse(ctx: StepContext[int]) -> int:
    Path(%q).write_text(str(ctx.inputs))
    if ctx.inputs == 0:
        raise FatalRunError("Input is invalid", "input.invalid") from RuntimeError("SECRET exception chain")
    if ctx.inputs == 1:
        Path(%q).touch()
        time.sleep(60)
        Path(%q).touch()
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
`, authorPath, authorPath+"-sibling", authorPath+"-completed", outcomePath)), 0600); err != nil {
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
			_ = os.Remove(authorPath + "-sibling")
			ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
			defer stop()
			siblingResult := make(chan error, 1)
			go func() {
				if collecting {
					siblingResult <- (&RuntimeMapItemCommand{Plan: planPath, RuntimeSources: sources, Node: "refuse", Item: `{"index":1,"value":1}`, Output: output + "-sibling", Project: "fatal-runtime", RunID: "run", DatastoreConfig: config}).Run(ctx)
				} else {
					siblingResult <- (&RuntimeStepCommand{Plan: planPath, RuntimeSources: sources, Node: "refuse", WorkflowInput: "1", Output: output + "-sibling", Project: "fatal-runtime", RunID: "run", DatastoreConfig: config}).Run(ctx)
				}
			}()
			for {
				if _, err := os.Stat(authorPath + "-sibling"); err == nil {
					break
				}
				select {
				case err := <-siblingResult:
					t.Fatalf("sibling exited before author handshake: %v", err)
				case <-ctx.Done():
					t.Fatal("sibling did not reach author code")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if collecting {
				err = (&RuntimeMapItemCommand{Plan: planPath, RuntimeSources: sources, Node: "refuse", Item: `{"index":0,"value":0}`, Output: output, Project: "fatal-runtime", RunID: "run", DatastoreConfig: config, TerminationLog: termination}).Run(t.Context())
				if string(readFile(t, output)) != valueparam.FatalItemEnvelope {
					t.Fatal("fatal collected as outcome")
				}
			} else {
				err = (&RuntimeStepCommand{Plan: planPath, RuntimeSources: sources, Node: "refuse", WorkflowInput: "0", Output: output, Project: "fatal-runtime", RunID: "run", DatastoreConfig: config}).Run(t.Context())
			}
			var fatal *orchestrator.FatalRunError
			if !errors.As(err, &fatal) || exitCodeFor(err) != 69 {
				t.Fatalf("fatal runtime exit: %v", err)
			}
			select {
			case siblingErr := <-siblingResult:
				if !errors.As(siblingErr, &fatal) || exitCodeFor(siblingErr) != 69 {
					t.Fatalf("sibling cancellation: %v", siblingErr)
				}
			case <-ctx.Done():
				t.Fatal("fatal marker did not cancel started sibling")
			}
			if _, err := os.Stat(authorPath + "-completed"); !os.IsNotExist(err) {
				t.Fatal("sibling completed")
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
			if collecting {
				evidence, err := executionphase.Parse(string(readFile(t, termination)))
				if err != nil || !evidence.Fatal {
					t.Fatalf("fatal termination evidence: %+v %v", evidence, err)
				}
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
			if outcome.Fatal == nil || outcome.Fatal.Type != cause.Type || outcome.Fatal.Message != cause.Message || outcome.FailedNode == nil || *outcome.FailedNode != "refuse" {
				t.Fatalf("fatal hook input: %+v", outcome)
			}
		})
	}
}
