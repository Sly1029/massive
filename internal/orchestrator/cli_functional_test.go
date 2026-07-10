package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
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
	storeRoot := newStoreRoot(t)
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
	compiled, _ := compileConsistentFixture(t, "linear-chain", workspace)
	assertResultArtifact(t, storeRoot, projectKey, runID, `"value:41"`)
	assertStepOutputs(t, storeRoot, projectKey, runID, compiled)
	assertRunManifestSucceeded(t, storeRoot, projectKey, runID, []string{"double", "increment", "label"})
}

func TestOrchestratorCLIDiamondFanInRealRunner(t *testing.T) {
	workspace := prepareRunWorkspace(t, "diamond", "diamond")
	storeRoot := newStoreRoot(t)
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
	compiled, _ := compileConsistentFixture(t, "diamond", workspace)
	assertResultArtifact(t, storeRoot, projectKey, runID, `81`)
	assertStepOutputs(t, storeRoot, projectKey, runID, compiled)
	assertRunManifestSucceeded(t, storeRoot, projectKey, runID, []string{"split", "left", "right", "merge"})
	assertStoredJSON(t, storeRoot, runInputKey(projectKey, runID, "merge").String(), `[21,60]`)
}

func TestOrchestratorCLIMultiStageFanInRealRunner(t *testing.T) {
	workspace := prepareRunWorkspaceFromTestdata(t, "multi-stage-merge")
	storeRoot := newStoreRoot(t)
	runID := "run-multi-stage-e2e"
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
	compiled := compileTestdataFixture(t, "multi-stage-merge")
	// split=20; a1=21,a2=22,b1=23,b2=24; merge-a=43,merge-b=47; merge-final=90.
	assertResultArtifact(t, storeRoot, projectKey, runID, `90`)
	assertStoredJSON(t, storeRoot, runInputKey(projectKey, runID, "merge-a").String(), `[21,22]`)
	assertStoredJSON(t, storeRoot, runInputKey(projectKey, runID, "merge-b").String(), `[23,24]`)
	assertStoredJSON(t, storeRoot, runInputKey(projectKey, runID, "merge-final").String(), `[43,47]`)
	assertStepOutputs(t, storeRoot, projectKey, runID, compiled)
	assertRunManifestSucceeded(t, storeRoot, projectKey, runID, stepIDs(compiled))
}

func TestOrchestratorCLIUnevenFanInRealRunner(t *testing.T) {
	workspace := prepareRunWorkspaceFromTestdata(t, "uneven-fan-in")
	storeRoot := newStoreRoot(t)
	runID := "run-uneven-fan-in-e2e"
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
	compiled := compileTestdataFixture(t, "uneven-fan-in")
	// split=20; short=21; long=40; long-tail=140; merge([21,140])=161.
	assertResultArtifact(t, storeRoot, projectKey, runID, `161`)
	assertStoredJSON(t, storeRoot, runInputKey(projectKey, runID, "merge").String(), `[21,140]`)
	assertStepOutputs(t, storeRoot, projectKey, runID, compiled)
	assertRunManifestSucceeded(t, storeRoot, projectKey, runID, stepIDs(compiled))
}

func TestOrchestratorCLIExternalSpecRoot(t *testing.T) {
	// Source tree lives in one directory, the spec in another, and the spec's
	// sourcePackages root points at the source tree by absolute path.
	sourceDir := t.TempDir()
	sourceData := readRepoFile(t, "internal", "orchestrator", "testdata", "linear-chain", "workflow.ts")
	if err := os.WriteFile(filepath.Join(sourceDir, "workflow.ts"), sourceData, 0o644); err != nil {
		t.Fatal(err)
	}

	specData := patchSpecSource(t, readRepoFile(t, "conformance", "fixtures", "specs", "linear-chain", "workflow-spec.json"), sourceDir)
	specData = setSpecPackageRoots(t, specData, sourceDir)

	specDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(specDir, "workflow-spec.json"), specData, 0o644); err != nil {
		t.Fatal(err)
	}

	storeRoot := newStoreRoot(t)
	runID := "run-external-root-e2e"
	result := runCommand(t,
		"go", "run", "./cmd/massive-orchestrator", "run",
		"--spec", filepath.Join(specDir, "workflow-spec.json"),
		"--store", storeRoot,
		"--project", "acme/security-workflows",
		"--run-id", runID,
		"--input", "20",
	)
	if result.err != nil {
		t.Fatalf("orchestrator failed\nstdout:\n%s\nstderr:\n%s\nerror: %v", result.stdout, result.stderr, result.err)
	}

	projectKey := NormalizeProjectKey("acme/security-workflows")
	assertResultArtifact(t, storeRoot, projectKey, runID, `"value:41"`)
}

func TestOrchestratorCLISourceDriftFailsRun(t *testing.T) {
	workspace := prepareRunWorkspace(t, "linear-chain", "linear-chain")
	// Edit the source after "compile" (the spec's manifest reflects the
	// original bytes); the run must refuse to execute the drifted source.
	drifted := "export function double(args: { readonly input: number }): number {\n  return args.input * 3;\n}\n"
	if err := os.WriteFile(filepath.Join(workspace, "workflow.ts"), []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}
	storeRoot := newStoreRoot(t)
	result := runCommand(t,
		"go", "run", "./cmd/massive-orchestrator", "run",
		"--spec", filepath.Join(workspace, "workflow-spec.json"),
		"--store", storeRoot,
		"--project", "acme/security-workflows",
		"--run-id", "run-drift-e2e",
		"--input", "20",
	)
	if result.err == nil {
		t.Fatalf("orchestrator succeeded despite source drift\nstdout:\n%s", result.stdout)
	}
	if !strings.Contains(result.stderr, "drifted since compile") || !strings.Contains(result.stderr, "workflow.ts") {
		t.Fatalf("stderr = %q, want a drift diagnostic naming workflow.ts", result.stderr)
	}
}

func TestOrchestratorCLISchemaFailureRealRunner(t *testing.T) {
	workspace := prepareRunWorkspace(t, "linear-chain", "invalid-output")
	storeRoot := newStoreRoot(t)
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

	specData := readRepoFile(t, "conformance", "fixtures", "specs", specFixture, "workflow-spec.json")
	sourceData := readRepoFile(t, "internal", "orchestrator", "testdata", sourceFixture, "workflow.ts")
	return writeRunWorkspace(t, specData, sourceData)
}

// prepareRunWorkspaceFromTestdata mirrors prepareRunWorkspace for cases whose
// spec lives alongside its source under internal/orchestrator/testdata rather
// than in the shared conformance spec fixtures.
func prepareRunWorkspaceFromTestdata(t *testing.T, caseName string) string {
	t.Helper()

	specData := readRepoFile(t, "internal", "orchestrator", "testdata", caseName, "workflow-spec.json")
	sourceData := readRepoFile(t, "internal", "orchestrator", "testdata", caseName, "workflow.ts")
	return writeRunWorkspace(t, specData, sourceData)
}

func writeRunWorkspace(t *testing.T, specData []byte, sourceData []byte) string {
	t.Helper()

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "workflow.ts"), sourceData, 0o644); err != nil {
		t.Fatal(err)
	}
	// The compiled plan's source-package hash must match the workflow.ts that
	// ships next to it, or the orchestrator's integrity check fails the run.
	// Fixture specs carry placeholder digests, so patch them to the real hashes
	// of the source written into this workspace.
	patched := patchSpecSource(t, specData, workspace)
	if err := os.WriteFile(filepath.Join(workspace, "workflow-spec.json"), patched, 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// patchSpecSource rewrites each source package's per-file hashes and package
// hash to match the real files under sourceDir, keeping the spec internally
// consistent with the source it will run against.
func patchSpecSource(t *testing.T, specData []byte, sourceDir string) []byte {
	t.Helper()

	var doc map[string]any
	if err := json.Unmarshal(specData, &doc); err != nil {
		t.Fatal(err)
	}
	packages, ok := doc["sourcePackages"].(map[string]any)
	if !ok {
		t.Fatal("spec has no sourcePackages object")
	}
	for _, raw := range packages {
		pkg, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("source package is not an object")
		}
		filesRaw, ok := pkg["files"].([]any)
		if !ok {
			t.Fatal("source package has no files array")
		}
		files := make([]SourcePackageFile, 0, len(filesRaw))
		for _, fileRaw := range filesRaw {
			fileMap, ok := fileRaw.(map[string]any)
			if !ok {
				t.Fatal("source package file is not an object")
			}
			path, ok := fileMap["path"].(string)
			if !ok {
				t.Fatal("source package file has no path")
			}
			content, err := os.ReadFile(filepath.Join(sourceDir, filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			hash := canonical.DigestBytes(content)
			fileMap["hash"] = hash
			files = append(files, SourcePackageFile{Path: path, Hash: hash})
		}
		packageHash, err := recomputeSourcePackageHash(files)
		if err != nil {
			t.Fatal(err)
		}
		pkg["packageHash"] = packageHash
		pkg["artifact"] = "packages/" + strings.Replace(packageHash, "sha256:", "sha256-", 1) + "/source.tar.zst"
	}
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return patched
}

// setSpecPackageRoots rewrites every source package's root to the given path,
// used to exercise a spec whose source tree lives outside the spec directory.
func setSpecPackageRoots(t *testing.T, specData []byte, root string) []byte {
	t.Helper()

	var doc map[string]any
	if err := json.Unmarshal(specData, &doc); err != nil {
		t.Fatal(err)
	}
	packages, ok := doc["sourcePackages"].(map[string]any)
	if !ok {
		t.Fatal("spec has no sourcePackages object")
	}
	for _, raw := range packages {
		pkg, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("source package is not an object")
		}
		pkg["root"] = root
	}
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return patched
}

// newStoreRoot is a datastore root whose read-only source snapshots (0555
// dirs / 0444 files) are made writable again before t.TempDir's own RemoveAll
// cleanup runs, so the temp tree can be unlinked. t.Cleanup is LIFO, so this
// registered-after cleanup executes first.
func newStoreRoot(t *testing.T) string {
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

func manifestsFromSpec(workflowSpec *spec.WorkflowSpec) map[string]SourcePackageManifest {
	manifests := make(map[string]SourcePackageManifest, len(workflowSpec.SourcePackages))
	for packageID, sourcePackage := range workflowSpec.SourcePackages {
		files := make([]SourcePackageFile, 0, len(sourcePackage.Files))
		for _, file := range sourcePackage.Files {
			files = append(files, SourcePackageFile{Path: file.Path, Hash: file.Hash})
		}
		// Root is left empty so RunConfig.SourcePackageRoot is the fallback the
		// direct-Run tests set explicitly.
		manifests[packageID] = SourcePackageManifest{Files: files}
	}
	return manifests
}

// compileConsistentFixture reads a conformance spec fixture, patches its source
// hashes to match the real source under sourceDir, and compiles it, returning
// the plan and the file manifests to thread into RunConfig.
func compileConsistentFixture(t *testing.T, name string, sourceDir string) (*plan.CompileResult, map[string]SourcePackageManifest) {
	t.Helper()

	specData := patchSpecSource(t, readRepoFile(t, "conformance", "fixtures", "specs", name, "workflow-spec.json"), sourceDir)
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}
	return compiled, manifestsFromSpec(workflowSpec)
}

// compileTestdataFixture compiles a case whose spec + source live together
// under internal/orchestrator/testdata, patching the source hashes first.
func compileTestdataFixture(t *testing.T, caseName string) *plan.CompileResult {
	t.Helper()

	sourceDir := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", caseName)
	specData := patchSpecSource(t, readRepoFile(t, "internal", "orchestrator", "testdata", caseName, "workflow-spec.json"), sourceDir)
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func stepIDs(compiled *plan.CompileResult) []string {
	var ids []string
	for _, node := range compiled.Plan.GetGraph().GetNodes() {
		if node.GetKind() == "step" {
			ids = append(ids, node.GetId())
		}
	}
	return ids
}

func assertResultArtifact(t *testing.T, storeRoot string, projectKey string, runID string, expected string) {
	t.Helper()

	assertStoredJSON(t, storeRoot, runResultKey(projectKey, runID).String(), expected)
}

func assertStepOutputs(t *testing.T, storeRoot string, projectKey string, runID string, compiled *plan.CompileResult) {
	t.Helper()

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
