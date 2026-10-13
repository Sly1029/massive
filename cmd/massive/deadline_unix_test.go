//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/runjournal"
)

// A run deadline stops in-flight map items through the cancellation path and
// fails the run with a durable diagnostic naming the deadline.
func TestRunDeadlineFailsTheRunAndStopsItsWorkers(t *testing.T) {
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
from massive import GraphBuilder, StepContext, container, execution

def items(ctx: StepContext[str]) -> list[str]:
    return [str(Path(ctx.inputs) / str(index)) for index in range(2)]

def wait(ctx: StepContext[str]) -> int:
    Path(ctx.inputs).write_text(str(os.getpid()))
    time.sleep(120)
    return 1

graph = GraphBuilder(name="deadline", input_type=str, output_type=list[int],
    defaults=execution(environment=container("example.invalid/runner@sha256:"+"1"*64)),
    deadline=timedelta(seconds=10))
source = graph.add(items)
graph.edge_from(graph.start).to(source)
graph.edge_from(graph.map(source, wait, id="wait", concurrency=2)).to_end(graph.end)
`
	entry := filepath.Join(root, "workflow.py")
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	if err := os.Mkdir(ready, 0o755); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binary, "run", entry, "--input", string(input), "--store", store,
		"--project", "test/deadline", "--run-id", "deadline", "--json")
	command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now()
	err = command.Run()
	elapsed := time.Since(started)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v, want status 1\nstderr: %s", err, stderr.String())
	}
	// The deadline, not the 120-second steps, ended the run.
	if elapsed > 60*time.Second {
		t.Fatalf("run took %s", elapsed)
	}
	var result runOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Status != "failed" {
		t.Fatalf("run output = %q (%v)", stdout.String(), err)
	}

	workers := 0
	for index := range 2 {
		body, err := os.ReadFile(filepath.Join(ready, strconv.Itoa(index)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		pid, err := strconv.Atoi(string(body))
		if err != nil {
			t.Fatal(err)
		}
		workers++
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("map item process %d survived the deadline: %v", pid, err)
		}
	}
	if workers == 0 {
		t.Fatalf("no map item started within the deadline\nstderr: %s", stderr.String())
	}

	inspected, err := exec.Command(binary, "inspect", "deadline", "--project", "test/deadline",
		"--store", store, "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var manifest runjournal.Manifest
	if err := json.Unmarshal(inspected, &manifest); err != nil {
		t.Fatalf("inspect output %s: %v", inspected, err)
	}
	if manifest.Status != "failed" || manifest.Diagnostic != "run exceeded its 10-second deadline at node wait" {
		t.Fatalf("journal status %q diagnostic %q", manifest.Status, manifest.Diagnostic)
	}
	for _, step := range manifest.Steps {
		if step.NodeID == "wait" && (step.Status != "cancelled" || step.Items == nil) {
			t.Fatalf("map step = %+v", step)
		}
		if step.NodeID == "wait" {
			for _, item := range *step.Items {
				if item.Status != "cancelled" && item.Status != "not-started" {
					t.Fatalf("map item = %+v", item)
				}
			}
		}
	}
}
