package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/orchestrator"
)

func TestEnvCheckReportsJSONForCI(t *testing.T) {
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
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "pyproject.toml"), []byte("[project]\nname = \"broken\"\nversion = \"0.1.0\"\ndependencies = [\"absent-package\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "workflow.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		entry, status, verification string
		exit                        int
	}{
		{filepath.Join(repository, "conformance", "workflows", "python-linear", "workflow.py"), "unverified", "UNDECLARED", 0},
		{filepath.Join(repository, "examples", "07-package", "workflow.py"), "ready", "DIRECT_REQUIREMENTS_SATISFIED", 0},
		{filepath.Join(broken, "workflow.py"), "failed", "DIRECT_REQUIREMENTS_SATISFIED", 1},
	} {
		command := exec.Command(binary, "env", "check", tc.entry, "--json")
		command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
		var stdout bytes.Buffer
		command.Stdout = &stdout
		err := command.Run()
		var exit *exec.ExitError
		if (tc.exit == 0 && err != nil) || (tc.exit != 0 && (!errors.As(err, &exit) || exit.ExitCode() != tc.exit)) {
			t.Fatalf("%s: exit = %v, want %d", tc.entry, err, tc.exit)
		}
		var report struct {
			Status       string `json:"status"`
			Verification string `json:"verification"`
			Interpreter  struct {
				Executable string `json:"executable"`
			} `json:"interpreter"`
			Findings []struct {
				Code string `json:"code"`
				Fix  string `json:"fix"`
			} `json:"findings"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("stdout is not one JSON report: %v\n%s", err, stdout.String())
		}
		if report.Status != tc.status || report.Verification != tc.verification || report.Interpreter.Executable == "" {
			t.Fatalf("report = %+v", report)
		}
		if tc.exit != 0 && (len(report.Findings) != 1 || report.Findings[0].Code != "MISSING_REQUIREMENT" || report.Findings[0].Fix == "") {
			t.Fatalf("findings = %+v", report.Findings)
		}
	}
}

// A target build checks only what emission needs; each executor attempt then
// checks its own interpreter against the archived project, exiting 68 before
// any author code runs when the image cannot run it.
func TestRuntimeStepPreflightsTheExecutorEnvironment(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(repository, "packages", "python", ".venv", "Scripts", "python.exe")
	}
	binary := filepath.Join(t.TempDir(), "massive")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	environment := append(os.Environ(), "MASSIVE_PYTHON="+python)
	for _, tc := range []struct {
		name, dependency string
		exit             int
	}{
		{"satisfied", "pydantic>=2", 0},
		{"missing", "absent-package-for-preflight>=1", runtimeExitPreflight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			for _, name := range []string{"workflow.py", "helper.py"} {
				body, err := os.ReadFile(filepath.Join(repository, "conformance", "workflows", "python-linear", name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(project, name), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			pyproject := "[project]\nname = \"preflight\"\nversion = \"0.1.0\"\ndependencies = [\"massive-workflows\", \"" + tc.dependency + "\"]\n"
			if err := os.WriteFile(filepath.Join(project, "pyproject.toml"), []byte(pyproject), 0o644); err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(t.TempDir(), "bundle")
			build := exec.Command(binary, "build", filepath.Join(project, "workflow.py"), "--output", bundle,
				"--namespace", "workflows", "--service-account", "runner", "--artifact-store", "artifacts")
			build.Env = environment
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build must not require the container's dependencies locally: %v\n%s", err, output)
			}
			store := t.TempDir()
			descriptor := filepath.Join(t.TempDir(), "datastore.json")
			if err := os.WriteFile(descriptor, []byte(`{"kind":"local","path":`+strconv.Quote(store)+`}`), 0o644); err != nil {
				t.Fatal(err)
			}
			step := exec.Command(binary, "runtime", "step", "--plan", filepath.Join(bundle, "massive-plan.json"),
				"--bundle-dir", filepath.Join(bundle, "runtime-assets"), "--node", "add_one", "--input", `{"value": 41}`,
				"--output", filepath.Join(t.TempDir(), "result.json"), "--project", "argo/preflight", "--run-id", "pod",
				"--datastore-config", descriptor)
			step.Env = environment
			var stderr bytes.Buffer
			step.Stderr = &stderr
			err := step.Run()
			var exit *exec.ExitError
			if (tc.exit == 0) != (err == nil) || (err != nil && (!errors.As(err, &exit) || exit.ExitCode() != tc.exit)) {
				t.Fatalf("runtime step exit = %v, want %d\n%s", err, tc.exit, stderr.String())
			}
			attempt := filepath.Join(store, "projects", orchestrator.NormalizeProjectKey("argo/preflight"), "runs", "pod", "steps", "add_one", "1")
			_, recorded := os.Stat(filepath.Join(attempt, "environment.json"))
			_, published := os.Stat(filepath.Join(attempt, "output-manifest.json"))
			if tc.exit == 0 && (recorded != nil || published != nil) {
				t.Fatalf("successful attempt lacks its environment record or output: %v %v", recorded, published)
			}
			if tc.exit != 0 && (!strings.Contains(stderr.String(), "MISSING_REQUIREMENT") || recorded == nil || published == nil) {
				t.Fatalf("failed preflight recorded an attempt or ran author code:\n%s", stderr.String())
			}
		})
	}
}
