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

	"github.com/Sly1029/massive/internal/runjournal"
)

func TestSignalsCancelDetachedExitHookAndRecordItsOutcome(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	root := t.TempDir()
	binary := filepath.Join(root, "massive")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	entry := filepath.Join(root, "workflow.py")
	source := `import os
import time
from pathlib import Path
from massive import GraphBuilder, RunOutcome, StepContext, container, execution

def check(ctx: StepContext[int]) -> int:
    if os.environ["STALL_RUN"] == "1":
        Path(os.environ["RUN_READY"]).write_text(str(os.getpid()))
        time.sleep(120)
    return ctx.inputs

def notify(ctx: StepContext[RunOutcome]) -> None:
    Path(os.environ["HOOK_READY"]).write_text(str(os.getpid()))
    time.sleep(120)

graph = GraphBuilder(name="signal-hook", input_type=int, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "1" * 64)))
graph.edge_from(graph.start).to(graph.add(check)).to_end(graph.end)
graph.on_exit(notify)
`
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		signal    syscall.Signal
		cancelRun bool
	}{
		{"interrupt after success", syscall.SIGINT, false},
		{"terminate after success", syscall.SIGTERM, false},
		{"second interrupt", syscall.SIGINT, true},
		{"terminate after interrupt", syscall.SIGTERM, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := t.TempDir()
			store := filepath.Join(work, "store")
			runReady, hookReady := filepath.Join(work, "run-ready"), filepath.Join(work, "hook-ready")
			stall, status, exitCode := "0", "succeeded", 0
			if test.cancelRun {
				stall, status, exitCode = "1", "cancelled", 1
			}
			command := exec.Command(binary, "run", entry, "--input=2", "--store", store,
				"--project", "test/hook-signals", "--run-id", "signals", "--json")
			command.Args = append(command.Args, "--pass-env", "STALL_RUN", "--pass-env", "RUN_READY", "--pass-env", "HOOK_READY")
			command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python, "STALL_RUN="+stall,
				"RUN_READY="+runReady, "HOOK_READY="+hookReady)
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			t.Cleanup(func() {
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				_ = filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
					if err == nil && entry.IsDir() {
						_ = os.Chmod(path, 0o755)
					}
					return nil
				})
			})
			// File handshakes prove the real isolated subprocess has started.
			waitForPID := func(path string) int {
				t.Helper()
				for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
						pid, err := strconv.Atoi(string(body))
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
						return pid
					}
					select {
					case err := <-exited:
						t.Fatalf("CLI exited before handshake: %v", err)
					default:
					}
				}
				t.Fatal("subprocess did not start")
				return 0
			}
			if test.cancelRun {
				waitForPID(runReady)
				if err := syscall.Kill(-command.Process.Pid, syscall.SIGINT); err != nil {
					t.Fatal(err)
				}
			}
			hookPID := waitForPID(hookReady)
			if err := syscall.Kill(-command.Process.Pid, test.signal); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-exited:
				code := 0
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					code = exit.ExitCode()
				} else if err != nil {
					t.Fatal(err)
				}
				if code != exitCode {
					t.Fatalf("exit = %d, want %d\n%s", code, exitCode, stderr.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("CLI did not return after hook signal")
			}
			if err := syscall.Kill(hookPID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("hook process %d survived: %v", hookPID, err)
			}
			var output runOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || output.Status != status || output.ExitHook == nil || output.ExitHook.Status != "failed" {
				t.Fatalf("run outcome changed: %s (%v)", stdout.String(), err)
			}
			if !test.cancelRun && string(output.Result) != "2" {
				t.Fatalf("successful result was not delivered: %s", stdout.String())
			}
			inspected, err := exec.Command(binary, "inspect", "signals", "--project", "test/hook-signals", "--store", store, "--json").Output()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := runjournal.Parse(inspected)
			if err != nil || journal.Status != status || journal.ExitHook == nil || len(journal.ExitHook.Attempts) != 1 || !strings.Contains(journal.ExitHook.Attempts[0].Diagnostic, "exit hook cancelled") {
				t.Fatalf("hook cancellation not recorded: %s (%v)", inspected, err)
			}
		})
	}
}
