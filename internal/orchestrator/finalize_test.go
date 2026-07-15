package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func succeededEntry(nodeID string) manifestStep {
	input := manifestDataArtifact{Key: "projects/p/runs/r/inputs/" + nodeID + ".json", Hash: "sha256:" + strings.Repeat("a", 64), ContentType: jsonContentType, Schema: "sha256:" + strings.Repeat("1", 64)}
	output := manifestDataArtifact{Key: "projects/p/runs/r/steps/" + nodeID + "/1/output.json", Hash: "sha256:" + strings.Repeat("b", 64), ContentType: jsonContentType, Schema: "sha256:" + strings.Repeat("1", 64)}
	return manifestStep{NodeID: nodeID, Status: StatusSucceeded, Attempts: []manifestAttempt{{Attempt: 1, Status: StatusSucceeded, Input: input, Output: &output}}}
}

func failedEntry(nodeID string) manifestStep {
	input := manifestDataArtifact{Key: "projects/p/runs/r/inputs/" + nodeID + ".json", Hash: "sha256:" + strings.Repeat("a", 64), ContentType: jsonContentType, Schema: "sha256:" + strings.Repeat("1", 64)}
	return manifestStep{NodeID: nodeID, Status: StatusFailed, Attempts: []manifestAttempt{{Attempt: 1, Status: StatusFailed, Input: input, Diagnostic: "step-execution-failure"}}}
}

// A node's entry is write-once: an identical replay is idempotent, but a replay
// with different bytes (a prior attempt recorded a divergent status) must error
// rather than silently keep the stale record.
func TestWriteNodeEntryRejectsDivergentReplay(t *testing.T) {
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := writeNodeEntry(ctx, store, "pk", "run1", succeededEntry("a")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeNodeEntry(ctx, store, "pk", "run1", succeededEntry("a")); err != nil {
		t.Fatalf("identical replay should be idempotent: %v", err)
	}
	err = writeNodeEntry(ctx, store, "pk", "run1", failedEntry("a"))
	if err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("divergent replay should error, got: %v", err)
	}
}

// FinalizeRun must not bless a run with any non-succeeded node: it records a
// terminal FAILED manifest naming the failing nodes and writes no result.json.
func TestFinalizeRunFailsWhenANodeFailed(t *testing.T) {
	compiled := compileLinearChainPlan(t)
	storeRoot := t.TempDir()
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project := "acme/security-workflows"
	projectKey := NormalizeProjectKey(project)
	runID := "run-fail"

	for _, stepID := range compiled.Schedule.StepIDs {
		entry := succeededEntry(stepID)
		if stepID == "increment" {
			entry = failedEntry(stepID)
		}
		if err := writeNodeEntry(ctx, store, projectKey, runID, entry); err != nil {
			t.Fatal(err)
		}
	}

	err = FinalizeRun(ctx, FinalizeConfig{Plan: compiled.Plan, DatastoreRoot: storeRoot, ProjectID: project, RunID: runID})
	if err == nil || !strings.Contains(err.Error(), "increment") {
		t.Fatalf("finalize should fail naming the failed node, got: %v", err)
	}

	manifest, err := store.Get(ctx, runManifestKey(projectKey, runID))
	if err != nil {
		t.Fatalf("read terminal manifest: %v", err)
	}
	var terminal runManifest
	if err := json.Unmarshal(manifest.Body, &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.Status != StatusFailed {
		t.Fatalf("terminal manifest status = %q, want failed", terminal.Status)
	}
	if _, err := store.Get(ctx, runResultKey(projectKey, runID)); err == nil {
		t.Fatal("a failed run must not write result.json")
	}
}

func compileLinearChainPlan(t *testing.T) *plan.CompileResult {
	t.Helper()
	specData := readRepoFile(t, "conformance", "fixtures", "specs", "linear-chain", "workflow-spec.json")
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
