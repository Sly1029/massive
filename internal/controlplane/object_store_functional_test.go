package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/datastore/miniotest"
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
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable; run uv sync --project packages/python")
	}
	t.Setenv("MASSIVE_PYTHON", python)
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
	frontend, err := Emit(context.Background(), filepath.Join(workflowRoot, "workflow.py"))
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
	pinned := orchestrator.PublishedSources{}
	for _, item := range template.Spec.Templates {
		if item.Name != "step-areas" {
			continue
		}
		for _, arg := range item.Container.Args {
			if value, ok := strings.CutPrefix(arg, "--source-archive="); ok {
				packageHash, archiveHash, _ := strings.Cut(value, "=")
				pinned[packageHash] = archiveHash
			}
		}
	}
	if len(pinned) != len(workflowPlan.GetSourcePackages()) {
		t.Fatalf("template pins %v for %d source packages", pinned, len(workflowPlan.GetSourcePackages()))
	}

	run := func(t *testing.T, descriptor orchestrator.DatastoreDescriptor) {
		config := orchestrator.IsolatedStepConfig{
			Plan: workflowPlan, Datastore: descriptor, ProjectID: "argo/large-source", RunID: "remote",
			RunnerCommand: []string{python, "-m", "massive.runner", "{descriptor}"}, Sources: pinned,
		}
		config.NodeID = "areas"
		if _, err := orchestrator.RunIsolatedStep(context.Background(), config, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "massive publish") {
			t.Fatalf("unpublished archive error = %v", err)
		}
		for attempt, created := range []bool{true, false} {
			published, err := PublishArgoSources(context.Background(), request.OutputDirectory, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if len(published) != 1 || published[0].Created != created || published[0].ArchiveHash != pinned[published[0].PackageHash] {
				t.Fatalf("publication %d = %+v", attempt, published)
			}
		}
		areasJSON, err := orchestrator.RunIsolatedStep(context.Background(), config, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
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
		run(t, orchestrator.LocalDatastoreDescriptor{Kind: "local", Path: writableStoreForTest(t)})
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
		})
	})
}
