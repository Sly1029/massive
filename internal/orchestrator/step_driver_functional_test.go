package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/plan"
)

// TestArgoStepDriverMatchesLocalRun proves the Argo pod contract without a
// cluster: for a linear chain and a diamond fan-in, executing a plan node by
// node through the real `massive-orchestrator step` CLI (one invocation per node
// in schedule order, exactly as one WorkflowTemplate task would) against a
// seeded local datastore produces step outputs byte-identical to running the
// same plan through the normal local orchestrator. It is mock-free: real deno
// runner, real local datastore, real CLI processes.
func TestArgoStepDriverMatchesLocalRun(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{name: "linear-chain", source: "linear-chain"},
		{name: "diamond", source: "diamond"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := prepareRunWorkspace(t, tc.name, tc.source)
			project := "acme/security-workflows"
			projectKey := NormalizeProjectKey(project)
			runID := "run-" + tc.name

			// (1) Full local run into store A.
			storeA := newStoreRoot(t)
			resultA := runCommand(t, "go", "run", "./cmd/massive-orchestrator", "run",
				"--spec", filepath.Join(workspace, "workflow-spec.json"),
				"--store", storeA, "--project", project, "--run-id", runID, "--input", "20")
			if resultA.err != nil {
				t.Fatalf("local run failed\nstdout:\n%s\nstderr:\n%s\nerror: %v", resultA.stdout, resultA.stderr, resultA.err)
			}

			// (2) Step-driver run into store B: seed prerequisites and the plan,
			// then invoke the step CLI once per node in schedule order.
			storeB := newStoreRoot(t)
			compiled, manifests := compileConsistentFixture(t, tc.name, workspace)
			persistPlanForStepDriver(t, storeB, compiled)
			if _, err := Seed(context.Background(), RunConfig{
				Plan:              compiled.Plan,
				DatastoreRoot:     storeB,
				ProjectID:         project,
				RunID:             runID,
				SourcePackageRoot: workspace,
				SourceManifests:   manifests,
			}, []byte("20")); err != nil {
				t.Fatalf("seed step-driver datastore: %v", err)
			}
			for _, stepID := range compiled.Schedule.StepIDs {
				result := runStepCommand(t, storeB, project, stepID, runID, compiled.PlanHash)
				if result.err != nil {
					t.Fatalf("step %s failed\nstdout:\n%s\nstderr:\n%s\nerror: %v", stepID, result.stdout, result.stderr, result.err)
				}
			}
			if result := runFinalizeCommand(t, storeB, project, runID, compiled.PlanHash); result.err != nil {
				t.Fatalf("finalize failed\nstdout:\n%s\nstderr:\n%s\nerror: %v", result.stdout, result.stderr, result.err)
			}

			// (3) Step outputs and per-step inputs must be byte-identical between
			// the local run and the step-driver run, and so must the finalized run
			// result and the terminal run manifest.
			for _, stepID := range compiled.Schedule.StepIDs {
				outputKey := runOutputKey(projectKey, runID, stepID, 1).String()
				if a, b := getObject(t, storeA, outputKey).Body, getObject(t, storeB, outputKey).Body; !bytes.Equal(a, b) {
					t.Fatalf("step %s output differs\nlocal:  %s\ndriver: %s", stepID, a, b)
				}
				inputKey := runInputKey(projectKey, runID, stepID).String()
				if a, b := getObject(t, storeA, inputKey).Body, getObject(t, storeB, inputKey).Body; !bytes.Equal(a, b) {
					t.Fatalf("step %s input differs\nlocal:  %s\ndriver: %s", stepID, a, b)
				}
			}
			resultKey := runResultKey(projectKey, runID).String()
			if a, b := getObject(t, storeA, resultKey).Body, getObject(t, storeB, resultKey).Body; !bytes.Equal(a, b) {
				t.Fatalf("run result differs\nlocal:  %s\ndriver: %s", a, b)
			}
			manifestKey := runManifestKey(projectKey, runID).String()
			if a, b := getObject(t, storeA, manifestKey).Body, getObject(t, storeB, manifestKey).Body; !bytes.Equal(a, b) {
				t.Fatalf("terminal run manifest differs\nlocal:  %s\ndriver: %s", a, b)
			}
		})
	}
}

// TestArgoStepDriverConcurrentBranchesKeepBothStatuses proves the parallel-DAG
// safety of the step driver: a diamond's two independent branch steps, run as
// concurrent `massive-orchestrator step` processes against one datastore, each
// persist their status without clobbering the other (disjoint per-node entry
// keys). After finalize, the run manifest carries both branches as succeeded.
func TestArgoStepDriverConcurrentBranchesKeepBothStatuses(t *testing.T) {
	workspace := prepareRunWorkspace(t, "diamond", "diamond")
	project := "acme/security-workflows"
	projectKey := NormalizeProjectKey(project)
	runID := "run-diamond-concurrent"

	store := newStoreRoot(t)
	compiled, manifests := compileConsistentFixture(t, "diamond", workspace)
	persistPlanForStepDriver(t, store, compiled)
	if _, err := Seed(context.Background(), RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     store,
		ProjectID:         project,
		RunID:             runID,
		SourcePackageRoot: workspace,
		SourceManifests:   manifests,
	}, []byte("20")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// split feeds both branches; run it first.
	if result := runStepCommand(t, store, project, "split", runID, compiled.PlanHash); result.err != nil {
		t.Fatalf("split failed\nstderr:\n%s\nerror: %v", result.stderr, result.err)
	}

	// left and right are independent — run them as concurrent processes.
	var wg sync.WaitGroup
	results := make([]commandResult, 2)
	for i, branch := range []string{"left", "right"} {
		wg.Add(1)
		go func(i int, branch string) {
			defer wg.Done()
			results[i] = runStepCommand(t, store, project, branch, runID, compiled.PlanHash)
		}(i, branch)
	}
	wg.Wait()
	for i, branch := range []string{"left", "right"} {
		if results[i].err != nil {
			t.Fatalf("concurrent branch %s failed\nstderr:\n%s\nerror: %v", branch, results[i].stderr, results[i].err)
		}
	}

	// Both branch entries survive as disjoint objects.
	for _, branch := range []string{"left", "right"} {
		entry := getObject(t, store, runNodeEntryKey(projectKey, runID, branch).String())
		var step manifestStep
		if err := json.Unmarshal(entry.Body, &step); err != nil {
			t.Fatalf("decode %s node entry: %v", branch, err)
		}
		if step.Status != StatusSucceeded {
			t.Fatalf("branch %s status = %q, want succeeded", branch, step.Status)
		}
	}

	// Finish the run and confirm both branches are succeeded in the manifest.
	if result := runStepCommand(t, store, project, "merge", runID, compiled.PlanHash); result.err != nil {
		t.Fatalf("merge failed\nstderr:\n%s\nerror: %v", result.stderr, result.err)
	}
	if result := runFinalizeCommand(t, store, project, runID, compiled.PlanHash); result.err != nil {
		t.Fatalf("finalize failed\nstderr:\n%s\nerror: %v", result.stderr, result.err)
	}
	manifest := readManifest(t, store, projectKey, runID)
	if manifest.Status != StatusSucceeded {
		t.Fatalf("manifest status = %q, want succeeded", manifest.Status)
	}
	statuses := map[string]string{}
	for _, step := range manifest.Steps {
		statuses[step.NodeID] = step.Status
	}
	for _, branch := range []string{"left", "right"} {
		if statuses[branch] != StatusSucceeded {
			t.Fatalf("manifest branch %s status = %q, want succeeded (both concurrent updates must survive)", branch, statuses[branch])
		}
	}
}

func readManifest(t *testing.T, storeRoot, projectKey, runID string) runManifest {
	t.Helper()
	object := getObject(t, storeRoot, runManifestKey(projectKey, runID).String())
	var manifest runManifest
	if err := json.Unmarshal(object.Body, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// persistPlanForStepDriver writes the compiled plan to the datastore at the key
// the step driver loads it from (plans/<plan-key>/workflow.json), mirroring what
// the compiler CLI and the WS-8 submit harness do before steps run.
func persistPlanForStepDriver(t *testing.T, storeRoot string, compiled *plan.CompileResult) {
	t.Helper()
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	segment := "sha256-" + strings.TrimPrefix(compiled.PlanHash, "sha256:")
	key := datastore.MustKey("plans/" + segment + "/workflow.json")
	if _, err := store.Put(context.Background(), key, compiled.CanonicalJSON, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		t.Fatal(err)
	}
}

func runStepCommand(t *testing.T, storeRoot, project, nodeID, runID, planHash string) commandResult {
	t.Helper()
	return runDriverCommand(t, storeRoot, project,
		"step", "--node", nodeID, "--run-id", runID, "--plan-hash", planHash)
}

func runFinalizeCommand(t *testing.T, storeRoot, project, runID, planHash string) commandResult {
	t.Helper()
	return runDriverCommand(t, storeRoot, project,
		"finalize", "--run-id", runID, "--plan-hash", planHash)
}

func runDriverCommand(t *testing.T, storeRoot, project string, args ...string) commandResult {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "./cmd/massive-orchestrator"}, args...)...)
	cmd.Dir = repoRootForTest(t)
	cmd.Env = append(os.Environ(),
		"GOCACHE=/tmp/massive-go-cache",
		"MASSIVE_DATASTORE_KIND=local",
		"MASSIVE_DATASTORE_PATH="+storeRoot,
		"MASSIVE_PROJECT_ID="+project,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}
