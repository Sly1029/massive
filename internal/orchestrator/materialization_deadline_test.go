package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func TestDeadlineDuringSourceMaterializationPublishesFailedRun(t *testing.T) {
	sourceRoot := t.TempDir()
	storeRoot := newStoreRoot(t)
	var document map[string]any
	if err := json.Unmarshal(readRepoFile(t, "conformance", "fixtures", "specs", "linear-chain", "workflow-spec.json"), &document); err != nil {
		t.Fatal(err)
	}
	files := []any{}
	for _, raw := range document["sourcePackages"].(map[string]any) {
		for _, entry := range raw.(map[string]any)["files"].([]any) {
			name := entry.(map[string]any)["path"].(string)
			body := readRepoFile(t, "internal", "orchestrator", "testdata", "linear-chain", name)
			if err := os.WriteFile(filepath.Join(sourceRoot, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
			files = append(files, map[string]any{"path": name, "hash": canonical.DigestBytes(body)})
		}
	}
	content := []byte(strings.Repeat("source fixture\n", 600))
	for i := range 4096 {
		name := fmt.Sprintf("source-%04d.ts", i)
		if err := os.WriteFile(filepath.Join(sourceRoot, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, map[string]any{"path": name, "hash": canonical.DigestBytes(content)})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].(map[string]any)["path"].(string) < files[j].(map[string]any)["path"].(string)
	})
	for _, raw := range document["sourcePackages"].(map[string]any) {
		raw.(map[string]any)["files"] = files
	}
	document["workflow"].(map[string]any)["deadlineSeconds"] = 1
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	data = patchSpecSource(t, data, sourceRoot)
	workflow, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(workflow, data)
	if err != nil {
		t.Fatal(err)
	}
	// A shorter inherited lifetime makes the regression independent of machine
	// throughput while using the same real deadline cause as the plan's timer.
	ctx, stop := context.WithTimeoutCause(t.Context(), 100*time.Millisecond, &RunDeadlineError{Seconds: 1})
	defer stop()
	started := time.Now()
	result, runErr := Run(ctx, RunConfig{
		Plan: compiled.Plan, DatastoreRoot: storeRoot, ProjectID: "test/materialization", RunID: "deadline",
		SourcePackageRoot: sourceRoot, SourceManifests: manifestsFromSpec(workflow),
	}, []byte("20"))
	var deadline *RunDeadlineError
	var failure *RunError
	if !errors.As(runErr, &deadline) || !errors.As(runErr, &failure) || result == nil || result.Status != StatusFailed {
		t.Fatalf("materialization deadline did not return a failed summary and typed cause: result=%#v error=%v", result, runErr)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("materialization did not stop promptly")
	}
	manifest := readRunManifest(t, storeRoot, result.ProjectKey, result.RunID)
	if manifest.Status != StatusFailed || !strings.Contains(manifest.Diagnostic, "deadline") {
		t.Fatalf("journal = %#v", manifest)
	}
	for _, step := range manifest.Steps {
		if len(step.Attempts) != 0 || step.Status != StatusNotStarted {
			t.Fatalf("author code dispatched during materialization: %#v", step)
		}
	}
	// Source materialization began: it published verified schema artifacts, but
	// did not get as far as publishing the source archive or invoking a runner.
	for _, schema := range compiled.Plan.GetSchemas() {
		key, err := blobKeyForHash(schema.GetHash())
		if err != nil {
			t.Fatal(err)
		}
		getObject(t, storeRoot, key.String())
	}
	archives, err := filepath.Glob(filepath.Join(storeRoot, "packages", "*", "archives", "*"))
	if err != nil || len(archives) != 0 {
		t.Fatalf("unexpected completed archives: %v %v", archives, err)
	}
}
