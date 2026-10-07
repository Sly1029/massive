package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/datastore/miniotest"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/target/argo"
)

// A source package above the embedded limit runs remotely only through
// object-store-v0: build pins archive digests, publish uploads them, and the
// real Python runner fetches and verifies them after the checkout is gone.
func TestObjectStoreSourcesRunThroughPublishedArchives(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := requirePythonSDK(t, repository)
	workflowRoot := t.TempDir()
	fixture := filepath.Join(repository, "conformance", "workflows", "large-source")
	for _, name := range []string{"workflow.py", "pyproject.toml"} {
		body, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workflowRoot, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := exec.Command(python, filepath.Join(fixture, "generate.py"), workflowRoot).CombinedOutput(); err != nil {
		t.Fatalf("generate resources: %v\n%s", err, output)
	}
	frontend, err := Emit(context.Background(), filepath.Join(workflowRoot, "workflow.py"), environment.Execution)
	if err != nil {
		t.Fatal(err)
	}
	local, err := RunLocal(context.Background(), LocalRunRequest{
		Frontend: frontend, Input: []byte(`{}`), Store: writableStoreForTest(t),
		Project: "massive/large-source", RunID: "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	request := ArgoBundleRequest{
		Frontend: frontend, ProfileName: "functional-test", ArtifactStoreBinding: "massive-artifacts",
		Namespace: "workflows", ServiceAccountName: "massive-runner", WorkflowTemplateName: "large-source",
		RuntimeTransport: argo.TransportEmbedded, OutputDirectory: t.TempDir(),
	}
	if _, err := BundleArgo(request); err == nil || !strings.Contains(err.Error(), "--runtime-transport object-store-v0") {
		t.Fatalf("oversized embedded bundle error = %v", err)
	}
	request.RuntimeTransport, request.OutputDirectory = argo.TransportObjectStore, t.TempDir()
	bundle, err := BundleArgo(request)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.RuntimeTransport != argo.TransportObjectStore {
		t.Fatalf("runtime transport = %q", bundle.RuntimeTransport)
	}
	// Remote execution must depend only on the bundle and the datastore.
	if err := os.RemoveAll(workflowRoot); err != nil {
		t.Fatal(err)
	}
	planJSON, err := os.ReadFile(filepath.Join(request.OutputDirectory, "massive-plan.json"))
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
	var template struct {
		Spec struct {
			Templates []struct {
				Name      string `json:"name"`
				Container *struct {
					Args []string `json:"args"`
				} `json:"container"`
			} `json:"templates"`
		} `json:"spec"`
	}
	templateJSON, err := os.ReadFile(filepath.Join(request.OutputDirectory, "workflow-template.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(templateJSON, &template); err != nil {
		t.Fatal(err)
	}
	// Execute with exactly the digests the WorkflowTemplate pins.
	pinned := map[string]orchestrator.SourceArchive{}
	for _, item := range template.Spec.Templates {
		if item.Name != "step-areas" {
			continue
		}
		for _, arg := range item.Container.Args {
			if value, ok := strings.CutPrefix(arg, "--source-archive="); ok {
				packageHash, archiveHash, _ := strings.Cut(value, "=")
				pinned[packageHash] = orchestrator.SourceArchive{Digest: archiveHash}
			}
		}
	}
	if len(pinned) != len(workflowPlan.GetSourcePackages()) {
		t.Fatalf("template pins %v for %d source packages", pinned, len(workflowPlan.GetSourcePackages()))
	}

	config := func(descriptor orchestrator.DatastoreDescriptor) orchestrator.IsolatedStepConfig {
		return orchestrator.IsolatedStepConfig{
			Plan: workflowPlan, NodeID: "areas", Datastore: descriptor, ProjectID: "argo/large-source", RunID: "remote",
			Environment: environment.Request{Python: python, ControlPlaneVersion: Version}, SourceArchives: pinned,
		}
	}
	// read returns an object from a filesystem store; S3 stores pass nil.
	run := func(t *testing.T, descriptor orchestrator.DatastoreDescriptor, read func(key string) ([]byte, error)) {
		config := config(descriptor)
		// The pod's dependency preflight refuses a missing archive (exit 68),
		// which Argo's retry expression never retries, before any author code.
		_, err := orchestrator.RunIsolatedStep(context.Background(), config, []byte(`{}`))
		var preflight *orchestrator.PreflightError
		if !errors.As(err, &preflight) || !strings.Contains(err.Error(), "massive publish") {
			t.Fatalf("unpublished archive error = %v", err)
		}
		for attempt, created := range []bool{true, false} {
			published, err := PublishArgoSources(context.Background(), request.OutputDirectory, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if len(published) != 1 || published[0].Created != created || published[0].ArchiveHash != pinned[published[0].PackageHash].Digest {
				t.Fatalf("publication %d = %+v", attempt, published)
			}
		}
		areasJSON, err := orchestrator.RunIsolatedStep(context.Background(), config, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if read != nil {
			// Preflight read the fetched archive's pyproject.toml: an empty
			// extraction would have been an undeclared project.
			binding, err := read("projects/" + orchestrator.NormalizeProjectKey("argo/large-source") + "/runs/remote/steps/areas/1/environment.json")
			if err != nil {
				t.Fatal(err)
			}
			var realized struct{ Record struct{ Key string } }
			if err := json.Unmarshal(binding, &realized); err != nil {
				t.Fatal(err)
			}
			body, err := read(realized.Record.Key)
			if err != nil {
				t.Fatal(err)
			}
			record, err := environment.ParseRecord(body)
			if err != nil || record.GetRealization().GetVerification().String() != string(environment.DirectRequirementsSatisfied) {
				t.Fatalf("attempt realization = %v, %v", record, err)
			}
		}
		var areas []string
		if err := json.Unmarshal(areasJSON, &areas); err != nil {
			t.Fatal(err)
		}
		config.NodeID = "inventory"
		inventories := make([]json.RawMessage, len(areas))
		for index, area := range areas {
			item, _ := json.Marshal(area)
			inventories[index], err = orchestrator.RunIsolatedMapItem(context.Background(), config, item, index)
			if err != nil {
				t.Fatal(err)
			}
		}
		collected, _ := json.Marshal(inventories)
		config.NodeID = "summarize"
		result, err := orchestrator.RunIsolatedStep(context.Background(), config, collected)
		if err != nil {
			t.Fatal(err)
		}
		var remote, expected map[string]any
		if err := json.Unmarshal(result, &remote); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(local.Result, &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(remote, expected) {
			t.Fatalf("remote result %s differs from local %s", result, local.Result)
		}
		if files := expected["files"].(float64); files < 2000 {
			t.Fatalf("fixture has only %v files", files)
		}
	}
	t.Run("filesystem", func(t *testing.T) {
		root := writableStoreForTest(t)
		run(t, orchestrator.LocalDatastoreDescriptor{Kind: "local", Path: root}, func(key string) ([]byte, error) {
			return os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
		})
	})
	t.Run("mismatched object", func(t *testing.T) {
		root := writableStoreForTest(t)
		store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		for packageHash, archive := range pinned {
			key := "packages/" + strings.Replace(packageHash, "sha256:", "sha256-", 1) + "/archives/" + strings.Replace(archive.Digest, "sha256:", "sha256-", 1) + ".tar"
			if _, err := store.Put(context.Background(), datastore.MustKey(key), []byte("not the pinned archive"), datastore.PutOptions{ContentType: orchestrator.SourceArchiveContentType}); err != nil {
				t.Fatal(err)
			}
		}
		_, err = orchestrator.RunIsolatedStep(context.Background(), config(orchestrator.LocalDatastoreDescriptor{Kind: "local", Path: root}), []byte(`{}`))
		var preflight *orchestrator.PreflightError
		if !errors.As(err, &preflight) || !strings.Contains(err.Error(), "does not match its pinned digest") {
			t.Fatalf("mismatched archive error = %v", err)
		}
	})
	t.Run("minio", func(t *testing.T) {
		endpoint := miniotest.Start(t)
		t.Setenv("AWS_ACCESS_KEY_ID", miniotest.AccessKey)
		t.Setenv("AWS_SECRET_ACCESS_KEY", miniotest.SecretKey)
		t.Setenv("AWS_SESSION_TOKEN", "")
		if _, err := datastore.NewS3Datastore(context.Background(), datastore.S3Config{
			Endpoint: endpoint, Bucket: "massive-sources", Region: "us-east-1", CreateBucket: true,
		}); err != nil {
			t.Fatal(err)
		}
		forcePathStyle := true
		run(t, orchestrator.S3DatastoreDescriptor{
			Kind: "s3", Bucket: "massive-sources", Region: "us-east-1", Prefix: "published",
			Endpoint: "http://" + endpoint, ForcePathStyle: &forcePathStyle,
		}, nil)
	})
}

// requirePythonSDK returns the repository's Python SDK interpreter. Locally a
// missing environment skips the test; in CI (CI=true) it fails it.
func requirePythonSDK(t *testing.T, repository string) string {
	t.Helper()
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("Python SDK environment is unavailable in CI: %v", err)
		}
		t.Skip("Python SDK environment is unavailable; run uv sync --project packages/python")
	}
	t.Setenv("MASSIVE_PYTHON", python)
	return python
}

func TestPublishRejectsUnsafeBundlesAndNeverOverwrites(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	requirePythonSDK(t, repository)
	frontend, err := Emit(context.Background(), filepath.Join(repository, "examples", "06-map", "workflow.py"), environment.Emission)
	if err != nil {
		t.Fatal(err)
	}
	build := func(transport string) string {
		output := t.TempDir()
		if _, err := BundleArgo(ArgoBundleRequest{
			Frontend: frontend, OutputDirectory: output, ProfileName: "functional-test",
			ArtifactStoreBinding: "massive-artifacts", Namespace: "workflows",
			ServiceAccountName: "massive-runner", WorkflowTemplateName: "map-example", RuntimeTransport: transport,
		}); err != nil {
			t.Fatal(err)
		}
		return output
	}
	store := func() orchestrator.DatastoreDescriptor {
		return orchestrator.LocalDatastoreDescriptor{Kind: "local", Path: writableStoreForTest(t)}
	}

	if _, err := PublishArgoSources(context.Background(), build(argo.TransportEmbedded), store()); err == nil || !strings.Contains(err.Error(), "only object-store-v0 bundles are published") {
		t.Fatalf("embedded bundle publication error = %v", err)
	}

	bundle := build(argo.TransportObjectStore)
	tampered := func(t *testing.T, name string, change func([]byte) []byte) string {
		copied := t.TempDir()
		if err := os.CopyFS(copied, os.DirFS(bundle)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(copied, filepath.FromSlash(name))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, change(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return copied
	}
	archives, err := filepath.Glob(filepath.Join(bundle, "runtime-assets", "*.tar"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("bundle archives = %v, %v", archives, err)
	}
	archiveName := "runtime-assets/" + filepath.Base(archives[0])
	t.Run("tampered archive", func(t *testing.T) {
		copied := tampered(t, archiveName, func(body []byte) []byte {
			changed := append([]byte(nil), body...)
			changed[600] ^= 1 // first file body byte
			return changed
		})
		if _, err := PublishArgoSources(context.Background(), copied, store()); err == nil || !strings.Contains(err.Error(), "has digest") {
			t.Fatalf("tampered archive error = %v", err)
		}
	})
	t.Run("tampered materialization manifest", func(t *testing.T) {
		copied := tampered(t, "materialization-manifest.json", func(body []byte) []byte {
			return bytes.Replace(body, []byte(`"materializerVersion":"1"`), []byte(`"materializerVersion":"2"`), 1)
		})
		if _, err := PublishArgoSources(context.Background(), copied, store()); err == nil || !strings.Contains(err.Error(), "does not match the digest") {
			t.Fatalf("tampered manifest error = %v", err)
		}
	})
	t.Run("different bytes at the key", func(t *testing.T) {
		probe, err := PublishArgoSources(context.Background(), bundle, store())
		if err != nil {
			t.Fatal(err)
		}
		descriptor := store()
		planted := []byte("not the published archive")
		local, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: descriptor.(orchestrator.LocalDatastoreDescriptor).Path})
		if err != nil {
			t.Fatal(err)
		}
		key := datastore.MustKey(probe[0].Key)
		if _, err := local.Put(context.Background(), key, planted, datastore.PutOptions{ContentType: orchestrator.SourceArchiveContentType}); err != nil {
			t.Fatal(err)
		}
		if _, err := PublishArgoSources(context.Background(), bundle, descriptor); err == nil || !strings.Contains(err.Error(), "different source archive") {
			t.Fatalf("conflicting publication error = %v", err)
		}
		existing, err := local.Get(context.Background(), key)
		if err != nil || !bytes.Equal(existing.Body, planted) {
			t.Fatalf("publication replaced the existing object: %q, %v", existing.Body, err)
		}
	})
}

// Multi-MB JSON values pass between local steps, decisions, selects and map
// items as ordinary datastore artifacts; Argo carries the same values by
// reference and conformance/argo requires the same result.
func TestMultiMegabyteValuesRunLocally(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	requirePythonSDK(t, repository)
	frontend, err := Emit(context.Background(), filepath.Join(repository, "conformance", "argo", "large_values.py"))
	if err != nil {
		t.Fatal(err)
	}
	store := writableStoreForTest(t)
	local, err := RunLocal(context.Background(), LocalRunRequest{
		Frontend: frontend, Input: []byte(`{"count":20000}`), Store: store,
		Project: "massive/large-values", RunID: "multi-mb",
	})
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Count     int `json:"count"`
		Annotated int `json:"annotated"`
	}
	if err := json.Unmarshal(local.Result, &summary); err != nil || summary.Count != 20000 || summary.Annotated != 20000 {
		t.Fatalf("summary = %s, %v", local.Result, err)
	}
	largest := int64(0)
	if err := filepath.WalkDir(filepath.Join(store, "blobs"), func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			info, err := entry.Info()
			if err == nil {
				largest = max(largest, info.Size())
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if largest < 3*1024*1024 {
		t.Fatalf("largest stored value is %d bytes; the fixture must pass multi-MB values", largest)
	}
}
