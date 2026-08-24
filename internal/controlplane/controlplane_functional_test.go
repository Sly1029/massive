package controlplane

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonWorkflowRunsLocallyAndBuildsForArgo(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable; run uv sync --project packages/python")
	}
	t.Setenv("MASSIVE_PYTHON", python)

	entry := filepath.Join(repository, "packages", "cli", "test", "fixtures", "python-linear", "workflow.py")
	frontend, err := Emit(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if frontend.Spec.Workflow.Name != "python-linear" {
		t.Fatalf("workflow name = %q, want python-linear", frontend.Spec.Workflow.Name)
	}

	store := writableStoreForTest(t)
	local, err := RunLocal(context.Background(), LocalRunRequest{
		Frontend: frontend,
		Input:    []byte(`{"value": 41}`),
		Store:    store,
		Project:  "massive/functional-test",
		RunID:    "python-linear",
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(local.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Value != 42 {
		t.Fatalf("result value = %d, want 42", result.Value)
	}

	output := t.TempDir()
	bundle, err := BundleArgo(ArgoBundleRequest{
		Frontend:             frontend,
		OutputDirectory:      output,
		ProfileName:          "functional-test",
		ArtifactStoreBinding: "massive-artifacts",
		Namespace:            "workflows",
		ServiceAccountName:   "massive-runner",
		WorkflowTemplateName: "python-linear",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.PlanHash != local.Plan.PlanHash {
		t.Fatalf("local plan hash %q differs from Argo plan hash %q", local.Plan.PlanHash, bundle.PlanHash)
	}
	template, err := os.ReadFile(filepath.Join(output, "workflow-template.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"kind: WorkflowTemplate", "serviceAccountName: massive-runner", "massive.dev/plan-hash: " + bundle.PlanHash} {
		if !strings.Contains(string(template), expected) {
			t.Fatalf("WorkflowTemplate does not contain %q:\n%s", expected, template)
		}
	}
}

func writableStoreForTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	return root
}
