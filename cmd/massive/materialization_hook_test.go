package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestExitHookRunsAfterSourceMaterializationDeadline(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	t.Setenv("MASSIVE_PYTHON", python)
	root := t.TempDir()
	store := filepath.Join(root, "store")
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(source, "workflow.py")
	body := `import os
from datetime import timedelta
from pathlib import Path
from massive import GraphBuilder, RunOutcome, StepContext, container, execution

def check(ctx: StepContext[int]) -> int:
    raise AssertionError("the run expired before author code could start")

def notify(ctx: StepContext[RunOutcome]) -> None:
    Path(os.environ["OUTCOME_FILE"]).write_text(ctx.inputs.model_dump_json())

graph = GraphBuilder(name="materialization-hook", input_type=int, output_type=int,
    deadline=timedelta(seconds=1),
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "1" * 64)))
graph.edge_from(graph.start).to(graph.add(check)).to_end(graph.end)
graph.on_exit(notify, timeout=timedelta(seconds=10))
`
	if err := os.WriteFile(entry, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "pyproject.toml"), []byte("[tool.massive.source]\ninclude = [\"workflow.py\", \"data/*.txt\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 4096 {
		if err := os.WriteFile(filepath.Join(source, "data", fmt.Sprintf("%04d.txt", i)), []byte(strings.Repeat("source fixture\n", 600)), 0o600); err != nil {
			t.Fatal(err)
		}
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
		manifest := orchestrator.SourcePackageManifest{Root: source}
		for _, file := range pkg.Files {
			manifest.Files = append(manifest.Files, orchestrator.SourcePackageFile{Path: file.Path, Hash: file.Hash})
		}
		manifests[id] = manifest
	}
	outcomeFile := filepath.Join(root, "outcome.json")
	t.Setenv("OUTCOME_FILE", outcomeFile)
	t.Cleanup(func() {
		_ = filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	// Shorten the inherited lifetime to make materialization cancellation
	// deterministic without depending on the filesystem's speed.
	ctx, stop := context.WithTimeoutCause(t.Context(), 100*time.Millisecond, &orchestrator.RunDeadlineError{Seconds: 1})
	defer stop()
	result, runErr := orchestrator.Run(ctx, orchestrator.RunConfig{
		PassEnv: []string{"OUTCOME_FILE"},
		Plan:    compiled.Plan, DatastoreRoot: store, ProjectID: "test/materialization-hook", RunID: "deadline",
		SourcePackageRoot: source, SourceManifests: manifests, RunnerCommand: orchestrator.PythonRunnerCommand(python),
	}, []byte("20"))
	var deadline *orchestrator.RunDeadlineError
	if !errors.As(runErr, &deadline) || result == nil || result.Status != "failed" || result.ExitHook == nil || result.ExitHook.Status != "succeeded" {
		t.Fatalf("materialization deadline lost its outcome or hook: result=%#v error=%v", result, runErr)
	}
	outcomeJSON, err := os.ReadFile(outcomeFile)
	if err != nil {
		t.Fatal(err)
	}
	var outcome orchestrator.RunOutcome
	if err := json.Unmarshal(outcomeJSON, &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "failed" || outcome.FailedNode != nil {
		t.Fatalf("hook outcome = %s", outcomeJSON)
	}
	journalJSON, err := os.ReadFile(filepath.Join(store, filepath.FromSlash(result.ManifestKey)))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := runjournal.Parse(journalJSON)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Status != "failed" || journal.ExitHook == nil || journal.ExitHook.Status != "succeeded" || len(journal.ExitHook.Attempts) != 1 {
		t.Fatalf("journal = %s", journalJSON)
	}
	for _, step := range journal.Steps {
		if len(step.Attempts) != 0 {
			t.Fatalf("author code dispatched before materialization: %+v", step)
		}
	}
}
