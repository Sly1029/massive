//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// A terminated or evicted pod must stay retryable: SIGTERM during the pod's
// dependency preflight exits as a cancellation, never as preflight exit 68.
func TestTerminationDuringPodPreflightIsRetryable(t *testing.T) {
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
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"workflow.py", "helper.py"} {
		body, err := os.ReadFile(filepath.Join(repository, "conformance", "workflows", "python-linear", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundle := filepath.Join(root, "bundle")
	build := exec.Command(binary, "build", filepath.Join(project, "workflow.py"), "--output", bundle,
		"--namespace", "workflows", "--service-account", "runner", "--artifact-store", "artifacts")
	build.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	// The pod's interpreter is still being probed when the pod is terminated.
	started := filepath.Join(root, "probe-started")
	slowPython := filepath.Join(root, "python")
	if err := os.WriteFile(slowPython, []byte("#!/bin/sh\ntouch "+strconv.Quote(started)+"\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(root, "datastore.json")
	if err := os.WriteFile(descriptor, []byte(`{"kind":"local","path":`+strconv.Quote(filepath.Join(root, "store"))+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	step := exec.Command(binary, "runtime", "step", "--plan", filepath.Join(bundle, "massive-plan.json"),
		"--bundle-dir", filepath.Join(bundle, "runtime-assets"), "--node", "add_one", "--workflow-input", `{"value": 41}`,
		"--output", filepath.Join(root, "result.json"), "--project", "argo/preflight", "--run-id", "pod",
		"--datastore-config", descriptor)
	step.Env = append(os.Environ(), "MASSIVE_PYTHON="+slowPython)
	if err := step.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = step.Process.Kill()
			t.Fatal("the preflight probe never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := step.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = step.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == runtimeExitPreflight || exit.ExitCode() <= 0 {
		t.Fatalf("terminated preflight exit = %v, want a retryable failure", err)
	}
}
