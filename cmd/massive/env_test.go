package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
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
