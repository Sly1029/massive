package controlplane

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/sourceidentity"
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

	entry := filepath.Join(repository, "conformance", "workflows", "python-linear", "workflow.py")
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
	if bundle.RuntimeTransport != "embedded-v0" {
		t.Fatalf("runtime transport = %q, want embedded-v0", bundle.RuntimeTransport)
	}
	template, err := os.ReadFile(filepath.Join(output, "workflow-template.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"kind: WorkflowTemplate", "serviceAccountName: massive-runner", "massive.dev/plan-hash: " + bundle.PlanHash, "massive.dev/runtime-transport: embedded-v0"} {
		if !strings.Contains(string(template), expected) {
			t.Fatalf("WorkflowTemplate does not contain %q:\n%s", expected, template)
		}
	}

	packageHash := frontend.Spec.SourcePackages["python-main"].PackageHash
	archiveName, err := orchestrator.SourceArchiveBundleName(packageHash)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(output, "runtime-assets", archiveName)
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("build did not expose its verified source package at %s: %v", archivePath, err)
	}
	if err := sourceidentity.VerifyArchive(archive, packageHash); err != nil {
		t.Fatalf("emitted source package does not match the compiled package identity: %v", err)
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	var entries []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, header.Name)
		if header.Mode != 0o644 || header.ModTime.Unix() != 0 || header.Uid != 0 || header.Gid != 0 {
			t.Fatalf("source archive metadata is not reproducible: %#v", header)
		}
	}
	if strings.Join(entries, ",") != "helper.py,workflow.py" {
		t.Fatalf("source archive entries = %v, want helper.py and workflow.py", entries)
	}
}

func TestArgoMapItemRunsThroughTheRealPythonRunner(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable; run uv sync --project packages/python")
	}
	t.Setenv("MASSIVE_PYTHON", python)

	frontend, err := Emit(context.Background(), filepath.Join(repository, "examples", "06-map", "workflow.py"))
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	if _, err := BundleArgo(ArgoBundleRequest{
		Frontend: frontend, OutputDirectory: output, ProfileName: "functional-test",
		ArtifactStoreBinding: "massive-artifacts", Namespace: "workflows",
		ServiceAccountName: "massive-runner", WorkflowTemplateName: "map-example",
	}); err != nil {
		t.Fatal(err)
	}

	planJSON, err := os.ReadFile(filepath.Join(output, "massive-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := plan.ParseCanonicalJSON(planJSON)
	if err != nil {
		t.Fatal(err)
	}
	workflowPlan, err := plan.VerifyCanonicalJSON(planJSON, parsed.GetPlanHash())
	if err != nil {
		t.Fatal(err)
	}
	archives := make(map[string][]byte, len(workflowPlan.GetSourcePackages()))
	for _, sourcePackage := range workflowPlan.GetSourcePackages() {
		name, err := orchestrator.SourceArchiveBundleName(sourcePackage.GetPackageHash())
		if err != nil {
			t.Fatal(err)
		}
		archive, err := os.ReadFile(filepath.Join(output, "runtime-assets", name))
		if err != nil {
			t.Fatal(err)
		}
		archives[sourcePackage.GetPackageHash()] = archive
	}

	result, err := orchestrator.RunIsolatedMapItem(context.Background(), orchestrator.IsolatedStepConfig{
		Plan: workflowPlan, NodeID: "square-items", Datastore: orchestrator.LocalDatastoreDescriptor{Kind: "local", Path: t.TempDir()},
		ProjectID: "argo/map-example", RunID: "mapped-python-item",
		RunnerCommand:  []string{python, "-m", "massive.runner", "{descriptor}"},
		SourceArchives: archives,
	}, []byte(`{"value":3}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(result), `{"source":3,"squared":9}`; got != want {
		t.Fatalf("mapped result = %s, want %s", got, want)
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

func TestInspectKeepsProjectsSeparateAndDoesNotWrite(t *testing.T) {
	if _, err := exec.LookPath("deno"); err != nil {
		t.Skip("Deno is unavailable; run pnpm check in a provisioned development environment")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MASSIVE_TYPESCRIPT_FRONTEND", filepath.Join(repository, "scripts", "massive-typescript-frontend"))
	t.Setenv("MASSIVE_TYPESCRIPT_RUNNER", filepath.Join(repository, "scripts", "massive-typescript-runner"))
	if _, err := os.Stat(filepath.Join(repository, "node_modules", "zod")); err != nil {
		t.Skip("SDK dependencies are unavailable; run pnpm install")
	}
	frontend, err := Emit(context.Background(), filepath.Join(repository, "conformance", "workflows", "linear-chain"))
	if err != nil {
		t.Fatal(err)
	}
	store := writableStoreForTest(t)
	for _, project := range []string{"acme/one", "acme/two"} {
		_, err := RunLocal(context.Background(), LocalRunRequest{Frontend: frontend, Input: []byte(`2`), Store: store, Project: project, RunID: "same"})
		if err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]string{}
	if err := filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		before[path] = string(body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"acme/one", "acme/two"} {
		journal, err := Inspect(context.Background(), store, project, "same")
		if err != nil {
			t.Fatal(err)
		}
		if journal.Status != "succeeded" || journal.ProjectKey != orchestrator.NormalizeProjectKey(project) || journal.Result == nil {
			t.Fatalf("journal: %+v", journal)
		}
	}
	after := map[string]string{}
	if err := filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		after[path] = string(body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("inspection changed stored bytes")
	}
	for _, runID := range []string{"../escape", "a/b", ""} {
		if _, err := Inspect(context.Background(), store, "acme/one", runID); err == nil {
			t.Fatalf("unsafe run %q accepted", runID)
		}
	}
}

func TestTypeScriptFrontendStreamsLargeSpecsAndSeparatesAuthorLogs(t *testing.T) {
	if _, err := exec.LookPath("deno"); err != nil {
		t.Skip("Deno is unavailable")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, "node_modules", "zod")); err != nil {
		t.Skip("run pnpm install for SDK dependencies")
	}
	t.Setenv("MASSIVE_TYPESCRIPT_FRONTEND", filepath.Join(repository, "scripts", "massive-typescript-frontend"))
	entry := filepath.Join(t.TempDir(), "workflow.ts")
	source := `import { workflow } from "@massive/sdk";
import { z } from "zod";
console.log("author diagnostic belongs on stderr");
const schema = z.string().describe("x".repeat(100000));
const flow = workflow({name:"large-spec",input:schema,output:schema});
flow.start().to(flow.end());
export default flow;`
	if err := os.WriteFile(entry, []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	emitted, err := Emit(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(emitted.Canonical) < 100000 || emitted.Spec.Workflow.Name != "large-spec" {
		t.Fatal("large canonical transport was truncated")
	}
}

func TestFrontendDiagnosticsDistinguishEntrypointsAndAuthorFailures(t *testing.T) {
	directory := t.TempDir()
	if _, err := Emit(context.Background(), directory); err == nil || !strings.Contains(err.Error(), "Python entrypoints must name a .py file") {
		t.Fatalf("directory diagnostic = %v", err)
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable")
	}
	t.Setenv("MASSIVE_PYTHON", python)
	entry := filepath.Join(directory, "workflow.py")
	if err := os.WriteFile(entry, []byte("raise RuntimeError('author failure marker')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Emit(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "author failure marker") || strings.Contains(err.Error(), "install") {
		t.Fatalf("author diagnostic = %v", err)
	}
}

func TestProjectInferenceDoesNotEchoOriginCredentials(t *testing.T) {
	directory := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", "https://gitlab-ci-token:job-token-secret@gitlab.com/group/repository.git"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", directory}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	if _, err := projectFromGitOrigin(directory); err == nil || strings.Contains(err.Error(), "job-token-secret") {
		t.Fatalf("project inference error = %v", err)
	}
}
