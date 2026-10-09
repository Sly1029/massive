package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/internal/environment"
)

// The equivalent composed workflows emit the same Graph IR projection
// (conformance/fixtures/composition); this checks they also run to one result.
func TestComposedWorkflowsRunToTheSameResultInBothSDKs(t *testing.T) {
	if _, err := exec.LookPath("deno"); err != nil {
		t.Skip("Deno is unavailable; run pnpm check in a provisioned development environment")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, "node_modules", "zod")); err != nil {
		t.Skip("SDK dependencies are unavailable; run pnpm install")
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable; run uv sync --project packages/python")
	}
	t.Setenv("MASSIVE_PYTHON", python)
	t.Setenv("MASSIVE_TYPESCRIPT_FRONTEND", filepath.Join(repository, "scripts", "massive-typescript-frontend"))
	t.Setenv("MASSIVE_TYPESCRIPT_RUNNER", filepath.Join(repository, "scripts", "massive-typescript-runner"))

	for _, entry := range []string{
		filepath.Join("conformance", "workflows", "ts-composed"),
		filepath.Join("conformance", "workflows", "python-composed", "workflow.py") + "#graph",
	} {
		t.Run(entry, func(t *testing.T) {
			frontend, err := Emit(context.Background(), filepath.Join(repository, entry), environment.Execution)
			if err != nil {
				t.Fatal(err)
			}
			local, err := RunLocal(context.Background(), LocalRunRequest{
				Frontend: frontend,
				Input:    []byte(`2`),
				Store:    writableStoreForTest(t),
				Project:  "massive/composition",
				RunID:    "composed",
			})
			if err != nil {
				t.Fatal(err)
			}
			var result string
			if err := json.Unmarshal(local.Result, &result); err != nil {
				t.Fatal(err)
			}
			// prepare 20; normalize 21; side 23; total 44; normalize 45; label.
			if result != "value:45" {
				t.Fatalf("result = %q, want value:45", result)
			}
		})
	}
}
