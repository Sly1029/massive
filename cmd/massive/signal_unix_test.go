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
	"strings"
	"syscall"
	"testing"
	"time"
)

// A terminal interrupt reaches only the CLI's foreground process group; the
// CLI must stop the step's own process group and close the journal.
func TestInterruptCancelsRunningStepAndJournal(t *testing.T) {
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
from pathlib import Path
from pydantic import BaseModel
from massive import GraphBuilder, StepContext, container, execution

class Request(BaseModel):
    ready: str

def wait(ctx: StepContext[Request]) -> int:
    Path(ctx.inputs.ready).write_text(str(os.getpid()))
    time.sleep(60)
    return 1

graph = GraphBuilder(name="interrupt", input_type=Request, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:"+"1"*64)))
graph.edge_from(graph.start).to(graph.add(wait)).to(graph.end)
`
	entry := filepath.Join(root, "workflow.py")
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	input, err := json.Marshal(map[string]string{"ready": ready})
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binary, "run", entry, "--input", string(input), "--store", store,
		"--project", "test/interrupt", "--run-id", "interrupted", "--json")
	command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()

	stepPID := 0
	for deadline := time.Now().Add(30 * time.Second); stepPID == 0; time.Sleep(10 * time.Millisecond) {
		if body, err := os.ReadFile(ready); err == nil && len(body) > 0 {
			if stepPID, err = strconv.Atoi(string(body)); err != nil {
				t.Fatal(err)
			}
		} else if time.Now().After(deadline) {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			t.Fatalf("step never started\nstderr: %s", stderr.String())
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(stepPID, syscall.SIGKILL) })
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("exit = %v, want status 1\nstderr: %s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		t.Fatal("CLI did not exit after interrupt")
	}
	if err := syscall.Kill(stepPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("step process %d survived the interrupt: %v", stepPID, err)
	}
	var result runOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Status != "cancelled" {
		t.Fatalf("run output = %q (%v)", stdout.String(), err)
	}
	inspected, err := exec.Command(binary, "inspect", "interrupted", "--project", "test/interrupt",
		"--store", store, "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inspected), `"status":"cancelled"`) {
		t.Fatalf("journal = %s", inspected)
	}
}
