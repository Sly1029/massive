package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/controlplane"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/runjournal"
)

func TestDeadlineDuringSuccessfulSettlementHasNoFailedNode(t *testing.T) {
	for _, successor := range []string{"end", "step"} {
		t.Run(successor, func(t *testing.T) {
			source := `import os
from pathlib import Path
from massive import GraphBuilder, RunOutcome, StepContext, container, execution

def first(ctx: StepContext[int]) -> int:
    return ctx.inputs

def next_step(ctx: StepContext[int]) -> int:
    raise AssertionError("the deadline must prevent dispatch")

def notify(ctx: StepContext[RunOutcome]) -> None:
    Path(os.environ["OUTCOME_FILE"]).write_text(ctx.inputs.model_dump_json())

graph = GraphBuilder(name="settlement-deadline", input_type=int, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "1" * 64)))
path = graph.edge_from(graph.start).to(graph.add(first))
`
			if successor == "step" {
				source += "path = path.to(graph.add(next_step))\n"
			}
			source += "path.to_end(graph.end)\ngraph.on_exit(notify)\n"
			config, outcomeFile := compiledExitHookFixture(t, source)
			ctx, stop := context.WithCancelCause(t.Context())
			defer stop(nil)
			config.Hooks = orchestrator.RunHooks{AfterStepInvocation: func(_ context.Context, descriptor orchestrator.StepInvocationDescriptor) error {
				if descriptor.NodeID == "first" {
					// The real subprocess has succeeded. Expire the run while its
					// output is being reconciled under the detached settlement context.
					timer := time.AfterFunc(10*time.Millisecond, func() { stop(&orchestrator.RunDeadlineError{Seconds: 1}) })
					defer timer.Stop()
					<-ctx.Done()
				}
				return nil
			}}
			result, runErr := orchestrator.Run(ctx, config, []byte("20"))
			var deadline *orchestrator.RunDeadlineError
			if !errors.As(runErr, &deadline) || result == nil || result.Status != "failed" || result.ExitHook == nil || result.ExitHook.Status != "succeeded" {
				t.Fatalf("deadline/hook result = %#v (%v)", result, runErr)
			}
			body, err := os.ReadFile(outcomeFile)
			if err != nil {
				t.Fatal(err)
			}
			var outcome orchestrator.RunOutcome
			if err := json.Unmarshal(body, &outcome); err != nil || outcome.Status != "failed" || outcome.FailedNode != nil {
				t.Fatalf("deadline attributed to successful or undispatched work: %s (%v)", body, err)
			}
			body, err = os.ReadFile(filepath.Join(config.DatastoreRoot, filepath.FromSlash(result.ManifestKey)))
			if err != nil {
				t.Fatal(err)
			}
			journal, err := runjournal.Parse(body)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range journal.Steps {
				if step.NodeID == "first" && (step.Status != "succeeded" || len(step.Attempts) != 1) {
					t.Fatalf("completed work was lost: %+v", step)
				}
				if step.NodeID == "next_step" && (step.Status != "not-started" || len(step.Attempts) != 0) {
					t.Fatalf("successor was dispatched: %+v", step)
				}
			}
		})
	}
}

func TestExitHookTimeoutBoundsOutputVerification(t *testing.T) {
	config, _ := compiledExitHookFixture(t, `from datetime import timedelta
from massive import GraphBuilder, RunOutcome, StepContext, container, execution

def first(ctx: StepContext[int]) -> int:
    return ctx.inputs

def notify(ctx: StepContext[RunOutcome]) -> None:
    pass

graph = GraphBuilder(name="hook-verification-timeout", input_type=int, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "1" * 64)))
graph.edge_from(graph.start).to(graph.add(first)).to_end(graph.end)
graph.on_exit(notify, timeout=timedelta(seconds=3))
`)
	verified := false
	config.Hooks.AfterStepInvocation = func(ctx context.Context, descriptor orchestrator.StepInvocationDescriptor) error {
		if descriptor.NodeID != "notify" {
			return nil
		}
		// Observe a real successful hook subprocess, then stall settlement until
		// the hook's own lifetime ends. Verification must retain that lifetime.
		select {
		case <-ctx.Done():
			verified = true
			return ctx.Err()
		case <-time.After(8 * time.Second):
			t.Error("hook verification outlived its timeout")
			return errors.New("hook verification outlived its timeout")
		}
	}
	result, err := orchestrator.Run(t.Context(), config, []byte("20"))
	if err != nil || !verified || result.Status != "succeeded" || result.ExitHook == nil || result.ExitHook.Status != "failed" {
		t.Fatalf("hook verification timeout lost the settled outcome: %#v (%v)", result, err)
	}
	body, err := os.ReadFile(filepath.Join(config.DatastoreRoot, filepath.FromSlash(result.ManifestKey)))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := runjournal.Parse(body)
	if err != nil || journal.ExitHook == nil || len(journal.ExitHook.Attempts) != 1 || !strings.Contains(journal.ExitHook.Attempts[0].Diagnostic, "3-second timeout") {
		t.Fatalf("hook verification timeout not recorded: %s (%v)", body, err)
	}
}

// compiledExitHookFixture emits and compiles real source for the local runner
// and supplies its verified source manifest and a filesystem datastore.
func compiledExitHookFixture(t *testing.T, source string) (orchestrator.RunConfig, string) {
	t.Helper()
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	t.Setenv("MASSIVE_PYTHON", python)
	root := t.TempDir()
	store := filepath.Join(root, "store")
	t.Cleanup(func() {
		_ = filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	entry := filepath.Join(root, "workflow.py")
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	frontend, err := controlplane.Emit(t.Context(), entry, environment.Execution)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(frontend.Spec, frontend.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	manifests := map[string]orchestrator.SourcePackageManifest{}
	for id, pkg := range frontend.Spec.SourcePackages {
		manifest := orchestrator.SourcePackageManifest{Root: root}
		for _, file := range pkg.Files {
			manifest.Files = append(manifest.Files, orchestrator.SourcePackageFile{Path: file.Path, Hash: file.Hash})
		}
		manifests[id] = manifest
	}
	outcomeFile := filepath.Join(root, "outcome.json")
	t.Setenv("OUTCOME_FILE", outcomeFile)
	return orchestrator.RunConfig{
		Plan: compiled.Plan, DatastoreRoot: store, ProjectID: "test/settlement-hook", RunID: "deadline",
		SourcePackageRoot: root, SourceManifests: manifests, RunnerCommand: orchestrator.PythonRunnerCommand(python),
	}, outcomeFile
}
