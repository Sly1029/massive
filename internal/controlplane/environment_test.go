package controlplane

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/environment"
)

// The committed fixture lock must stay current: this syncs a real environment
// from it, checks it exactly, and runs every task with the probed interpreter.
func TestLockedWorkflowRunsWithThePreflightInterpreter(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Fatal("this test requires uv on PATH")
	}
	project := filepath.Join(repository, "conformance", "workflows", "python-locked")
	prefix := filepath.Join(t.TempDir(), "venv")
	sync := exec.Command("uv", "sync", "--quiet", "--locked", "--project", project)
	sync.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+prefix)
	if output, err := sync.CombinedOutput(); err != nil {
		t.Fatalf("uv sync --locked: %v\n%s", err, output)
	}
	python := filepath.Join(prefix, "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(prefix, "Scripts", "python.exe")
	}
	t.Setenv("MASSIVE_PYTHON", python)

	frontend, err := Emit(t.Context(), filepath.Join(project, "workflow.py"), environment.Execution)
	if err != nil {
		t.Fatal(err)
	}
	if frontend.Environment.Verification != environment.LockSyncChecked {
		t.Fatalf("verification = %s", frontend.Environment.Verification)
	}
	// Tasks use the interpreter pinned by preflight, not a later lookup.
	t.Setenv("MASSIVE_PYTHON", filepath.Join(t.TempDir(), "missing-python"))
	store := writableStoreForTest(t)
	result, err := RunLocal(t.Context(), LocalRunRequest{
		Frontend: frontend, Input: []byte(`{"value": 21}`), Store: store,
		Project: "massive/environment-test", RunID: "locked",
	})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Value int    `json:"value"`
		Table string `json:"table"`
	}
	if err := json.Unmarshal(result.Result, &output); err != nil || output.Value != 42 || output.Table != "value  42" {
		t.Fatalf("result = %s, %v", result.Result, err)
	}
	journal, record, err := InspectEnvironment(t.Context(), store, "massive/environment-test", "locked")
	if err != nil {
		t.Fatal(err)
	}
	if journal.Environment.RealizationHash != frontend.Environment.Record.GetRealizationHash() ||
		record.GetRequirement().GetLockHash() == "" || record.GetRealization().GetMaterializerName() != environment.MaterializerName {
		t.Fatalf("journal environment = %+v, record = %v", journal.Environment, record)
	}
}

func TestPreflightFailureImportsNoWorkflowModule(t *testing.T) {
	useCancellationPython(t)
	root := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "imported")
	files := map[string]string{
		"pyproject.toml": "[project]\nname = \"missing\"\nversion = \"0.1.0\"\ndependencies = [\"absent-package>=1\"]\n",
		"workflow.py":    "open(r'" + sentinel + "', 'w').close()\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Emit(t.Context(), filepath.Join(root, "workflow.py"), environment.Execution)
	var preflight *environment.PreflightError
	if !errors.As(err, &preflight) || preflight.Findings[0].Code != "MISSING_REQUIREMENT" {
		t.Fatalf("error = %v, want a missing requirement finding", err)
	}
	if !strings.Contains(err.Error(), "no workflow module was imported") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workflow module was imported: %v", err)
	}

	t.Setenv("MASSIVE_PYTHON", "")
	if _, err := Emit(t.Context(), filepath.Join(root, "workflow.py"), environment.Execution); err == nil || !strings.Contains(err.Error(), "uv run --locked massive") {
		t.Fatalf("error = %v, want interpreter guidance", err)
	}
}
