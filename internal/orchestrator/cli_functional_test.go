package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func TestCompilerCLIFunctional(t *testing.T) {
	for _, fixture := range []string{"passthrough", "linear-chain", "diamond"} {
		t.Run(fixture, func(t *testing.T) {
			outDir := t.TempDir()
			specPath := filepath.Join(repoRootForTest(t), "conformance", "fixtures", "specs", fixture, "workflow-spec.json")
			result := runCommand(t, "go", "run", "./cmd/massive-compiler", "compile", "--spec", specPath, "--out", outDir)
			if result.err != nil {
				t.Fatalf("compiler failed\nstdout:\n%s\nstderr:\n%s\nerror: %v", result.stdout, result.stderr, result.err)
			}

			outputPath := filepath.Join(outDir, "workflow-plan.json")
			actual, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			specData := readRepoFile(t, "conformance", "fixtures", "specs", fixture, "workflow-spec.json")
			workflowSpec, err := spec.Parse(specData)
			if err != nil {
				t.Fatal(err)
			}
			compiled, err := plan.Compile(workflowSpec, specData)
			if err != nil {
				t.Fatal(err)
			}
			expected := append(append([]byte{}, compiled.CanonicalJSON...), '\n')
			if !bytes.Equal(actual, expected) {
				t.Fatalf("workflow-plan.json does not match compiler output\nactual:   %s\nexpected: %s", actual, expected)
			}
		})
	}

	t.Run("invalid missing contract ref", func(t *testing.T) {
		specPath := filepath.Join(repoRootForTest(t), "conformance", "fixtures", "specs", "invalid-missing-contract-ref", "workflow-spec.json")
		result := runCommand(t, "go", "run", "./cmd/massive-compiler", "compile", "--spec", specPath, "--out", t.TempDir())
		if result.err == nil {
			t.Fatal("compiler succeeded for invalid spec")
		}
		if !strings.Contains(result.stderr, "$.graph.nodes") || !strings.Contains(result.stderr, "contractRef") {
			t.Fatalf("stderr = %q, want JSON path naming contractRef", result.stderr)
		}
	})

	t.Run("missing spec flag", func(t *testing.T) {
		result := runCommand(t, "go", "run", "./cmd/massive-compiler", "compile", "--out", t.TempDir())
		if result.err == nil {
			t.Fatal("compiler succeeded without --spec")
		}
		if !strings.Contains(result.stderr, "requires --spec") {
			t.Fatalf("stderr = %q, want requires --spec", result.stderr)
		}
	})
}

func TestOrchestratorCLILinearChainRealRunner(t *testing.T) {
	workspace := prepareRunWorkspace(t, "linear-chain", "linear-chain")
	storeRoot := t.TempDir()
	runID := "run-linear-e2e"
	result := runCommand(t,
		"go", "run", "./cmd/massive-orchestrator", "run",
		"--spec", filepath.Join(workspace, "workflow-spec.json"),
		"--store", storeRoot,
		"--project", "acme/security-workflows",
		"--run-id", runID,
		"--input", "20",
	)
	if result.err != nil {
		t.Fatalf("orchestrator failed\nstdout:\n%s\nstderr:\n%s\nerror: %v", result.stdout, result.stderr, result.err)
	}
	if !strings.Contains(result.stdout, "step double: succeeded") || !strings.Contains(result.stdout, "result: ") {
		t.Fatalf("stdout = %q, want step statuses and result", result.stdout)
	}

	projectKey := NormalizeProjectKey("acme/security-workflows")
	assertResultArtifact(t, storeRoot, projectKey, runID, `"value:41"`)
	assertStepOutputs(t, storeRoot, projectKey, runID, "linear-chain")
	assertRunManifestSucceeded(t, storeRoot, projectKey, runID, []string{"double", "increment", "label"})
}

func TestOrchestratorCLIDiamondFanInRealRunner(t *testing.T) {
	workspace := prepareRunWorkspace(t, "diamond", "diamond")
	storeRoot := t.TempDir()
	runID := "run-diamond-e2e"
	result := runCommand(t,
		"go", "run", "./cmd/massive-orchestrator", "run",
		"--spec", filepath.Join(workspace, "workflow-spec.json"),
		"--store", storeRoot,
		"--project", "acme/security-workflows",
		"--run-id", runID,
		"--input", "20",
	)
	if result.err != nil {
		t.Fatalf("orchestrator failed\nstdout:\n%s\nstderr:\n%s\nerror: %v", result.stdout, result.stderr, result.err)
	}

	projectKey := NormalizeProjectKey("acme/security-workflows")
	assertResultArtifact(t, storeRoot, projectKey, runID, `81`)
	assertStepOutputs(t, storeRoot, projectKey, runID, "diamond")
	assertRunManifestSucceeded(t, storeRoot, projectKey, runID, []string{"split", "left", "right", "merge"})
	assertStoredJSON(t, storeRoot, runInputKey(projectKey, runID, "merge").String(), `[21,60]`)
}

func TestOrchestratorCLISchemaFailureRealRunner(t *testing.T) {
	workspace := prepareRunWorkspace(t, "linear-chain", "invalid-output")
	storeRoot := t.TempDir()
	runID := "run-invalid-output-e2e"
	result := runCommand(t,
		"go", "run", "./cmd/massive-orchestrator", "run",
		"--spec", filepath.Join(workspace, "workflow-spec.json"),
		"--store", storeRoot,
		"--project", "acme/security-workflows",
		"--run-id", runID,
		"--input", "20",
	)
	if result.err == nil {
		t.Fatal("orchestrator succeeded for schema-invalid output")
	}
	if !strings.Contains(result.stderr, "schema-validation-failure") {
		t.Fatalf("stderr = %q, want schema-validation-failure", result.stderr)
	}

	projectKey := NormalizeProjectKey("acme/security-workflows")
	manifest := readRunManifest(t, storeRoot, projectKey, runID)
	if manifest.Status != StatusFailed {
		t.Fatalf("manifest status = %s, want failed", manifest.Status)
	}
	if manifest.Steps[0].NodeID != "double" || manifest.Steps[0].Status != StatusFailed {
		t.Fatalf("first step = %#v, want failed double", manifest.Steps[0])
	}
}

type commandResult struct {
	stdout string
	stderr string
	err    error
}

func runCommand(t *testing.T, name string, args ...string) commandResult {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = repoRootForTest(t)
	cmd.Env = append(os.Environ(), "GOCACHE=/tmp/massive-go-cache")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func prepareRunWorkspace(t *testing.T, specFixture string, sourceFixture string) string {
	t.Helper()

	workspace := t.TempDir()
	specData := readRepoFile(t, "conformance", "fixtures", "specs", specFixture, "workflow-spec.json")
	if err := os.WriteFile(filepath.Join(workspace, "workflow-spec.json"), specData, 0o644); err != nil {
		t.Fatal(err)
	}
	sourceData := readRepoFile(t, "internal", "orchestrator", "testdata", sourceFixture, "workflow.ts")
	if err := os.WriteFile(filepath.Join(workspace, "workflow.ts"), sourceData, 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func assertResultArtifact(t *testing.T, storeRoot string, projectKey string, runID string, expected string) {
	t.Helper()

	assertStoredJSON(t, storeRoot, runResultKey(projectKey, runID).String(), expected)
}

func assertStepOutputs(t *testing.T, storeRoot string, projectKey string, runID string, fixture string) {
	t.Helper()

	compiled := compileFixture(t, fixture)
	for _, node := range compiled.Plan.GetGraph().GetNodes() {
		if node.GetKind() != "step" {
			continue
		}
		key := runOutputKey(projectKey, runID, node.GetId(), 1)
		object := getObject(t, storeRoot, key.String())
		if canonical.DigestBytes(object.Body) == "" {
			t.Fatalf("empty digest for %s", key)
		}
		if _, err := canonical.CanonicalizeJSON(object.Body); err != nil {
			t.Fatalf("%s is not canonical JSON: %v", key, err)
		}
	}
}

func assertRunManifestSucceeded(t *testing.T, storeRoot string, projectKey string, runID string, steps []string) {
	t.Helper()

	manifest := readRunManifest(t, storeRoot, projectKey, runID)
	if manifest.Status != StatusSucceeded {
		t.Fatalf("manifest status = %s, want succeeded", manifest.Status)
	}
	if len(manifest.Steps) != len(steps) {
		t.Fatalf("manifest steps = %d, want %d", len(manifest.Steps), len(steps))
	}
	for index, stepID := range steps {
		step := manifest.Steps[index]
		if step.NodeID != stepID || step.Status != StatusSucceeded {
			t.Fatalf("step[%d] = %#v, want %s succeeded", index, step, stepID)
		}
		if len(step.Attempts) != 1 || step.Attempts[0].Output == nil {
			t.Fatalf("step[%d] attempts = %#v, want one output attempt", index, step.Attempts)
		}
		output := step.Attempts[0].Output
		object := getObject(t, storeRoot, output.Key)
		if canonical.DigestBytes(object.Body) != output.Hash {
			t.Fatalf("%s hash = %s, manifest records %s", output.Key, canonical.DigestBytes(object.Body), output.Hash)
		}
	}
}

func readRunManifest(t *testing.T, storeRoot string, projectKey string, runID string) runManifest {
	t.Helper()

	object := getObject(t, storeRoot, runManifestKey(projectKey, runID).String())
	var manifest runManifest
	if err := json.Unmarshal(object.Body, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertStoredJSON(t *testing.T, storeRoot string, key string, expected string) {
	t.Helper()

	object := getObject(t, storeRoot, key)
	if string(object.Body) != expected {
		t.Fatalf("%s = %s, want %s", key, object.Body, expected)
	}
}

func getObject(t *testing.T, storeRoot string, key string) datastore.Object {
	t.Helper()

	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Get(context.Background(), datastore.MustKey(key))
	if err != nil {
		t.Fatal(err)
	}
	return object
}
