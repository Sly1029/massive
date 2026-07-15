package orchestrator

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

			// (3) Step outputs (and their per-step input artifacts) must be
			// byte-identical between the local run and the step-driver run.
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
		})
	}
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
	cmd := exec.Command("go", "run", "./cmd/massive-orchestrator", "step",
		"--node", nodeID, "--run-id", runID, "--plan-hash", planHash)
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
