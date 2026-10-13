package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/executionphase"
	"github.com/Sly1029/massive/internal/mapexec"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/runjournal"
	"github.com/Sly1029/massive/internal/valueparam"
)

// TestArgoMapOutcomesMatchLocalExecution runs a collecting map's items through
// the Argo item runtime with the real Python runner, retrying the way Argo's
// retry expression does, and collects them as the Argo collector would. The
// outcome list must equal the local run's byte for byte.
func TestArgoMapOutcomesMatchLocalExecution(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable; run uv sync --project packages/python")
	}
	t.Setenv("MASSIVE_PYTHON", python)
	workflow := filepath.Join(repository, "conformance", "workflows", "python-map-outcomes", "workflow.py")
	tasks := []string{"ok", "flaky", "raise", "refuse", "segfault", "sigkill", "hang"}
	var entries []string
	for _, behavior := range tasks {
		entries = append(entries, fmt.Sprintf(`{"behavior":%q,"name":%q}`, behavior, behavior))
	}
	input := `{"tasks":[` + strings.Join(entries, ",") + `]}`

	// Local runs install read-only source snapshots in the store.
	store := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	var stdout bytes.Buffer
	if err := (&RunCommand{Entry: workflow + "#graph", Input: input, Store: store, Project: "test/map-outcomes", RunID: "local", JSON: true}).Run(t.Context(), &stdout); err != nil {
		t.Fatal(err)
	}
	local := collectedMapOutput(t, store, "test/map-outcomes", "local", "scan")

	bundle := filepath.Join(t.TempDir(), "bundle")
	if err := (&BuildCommand{
		Entry: workflow + "#graph", Output: bundle, Namespace: "workflows", ServiceAccount: "massive-runner",
		ArtifactStore: "massive-artifacts", RuntimeTransport: "embedded-v0", Profile: "argo", Target: "argo",
	}).Run(t.Context(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	datastoreConfig := localDatastoreConfig(t)
	codec, _, err := openRuntimeDatastore(t.Context(), datastoreConfig)
	if err != nil {
		t.Fatal(err)
	}
	expanded := filepath.Join(t.TempDir(), "expanded.json")
	tasksList := `[` + strings.Join(entries, ",") + `]`
	if err := (&RuntimeMapExpandCommand{Input: tasksList, Output: expanded, DatastoreConfig: datastoreConfig}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(readFile(t, expanded), &items); err != nil {
		t.Fatal(err)
	}
	const maxAttempts = 2
	outcomes := make([]string, len(items))
	for index, item := range items {
		output := filepath.Join(t.TempDir(), "result.json")
		termination := filepath.Join(t.TempDir(), "termination.json")
		for retries := range maxAttempts {
			err := (&RuntimeMapItemCommand{
				Plan: filepath.Join(bundle, "massive-plan.json"), RuntimeSources: RuntimeSources{BundleDir: filepath.Join(bundle, "runtime-assets")},
				Node: "scan", Item: string(item), Output: output, Project: "argo/map-outcomes", RunID: "argo",
				DatastoreConfig: datastoreConfig, RetryCount: retries, TerminationLog: termination,
			}).Run(t.Context())
			if err == nil {
				break
			}
			// Argo's retry expression stops on these exits; the rest retry.
			if slices.Contains([]int{64, 65, 67, 68, 69, 70}, exitCodeFor(err)) || retries == maxAttempts-1 {
				t.Fatalf("item %d failed terminally instead of reporting an outcome: %v", index, err)
			}
		}
		evidence, err := executionphase.Parse(string(readFile(t, termination)))
		if err != nil || evidence.Phase != executionphase.Publish {
			t.Fatalf("completed item phase = %+v, error = %v", evidence, err)
		}
		outcomes[index] = string(readFile(t, output))
	}

	// Block the real filesystem datastore's failure-reference metadata path.
	// This fails publication after the author exception, rather than hydration.
	var binding struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(readFile(t, datastoreConfig), &binding); err != nil {
		t.Fatal(err)
	}
	failureBody, err := canonical.Marshal(mapexec.NewFailure(mapexec.FailureError, 2, "step-execution-failure (exit 66): raise cannot be parsed"))
	if err != nil {
		t.Fatal(err)
	}
	failureKey := "blobs/sha256/" + strings.TrimPrefix(canonical.DigestBytes(failureBody), "sha256:")
	metadata := filepath.Join(binding.Path, ".massive-datastore-metadata", strings.TrimPrefix(canonical.DigestBytes([]byte(failureKey)), "sha256:")+".json")
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metadata, 0o700); err != nil {
		t.Fatal(err)
	}
	fatalOutput := filepath.Join(t.TempDir(), "publication.json")
	err = (&RuntimeMapItemCommand{
		Plan: filepath.Join(bundle, "massive-plan.json"), RuntimeSources: RuntimeSources{BundleDir: filepath.Join(bundle, "runtime-assets")},
		Node: "scan", Item: string(items[2]), Output: fatalOutput, Project: "argo/map-outcomes", RunID: "publication-error",
		DatastoreConfig: datastoreConfig, RetryCount: 1,
	}).Run(t.Context())
	if err == nil || string(readFile(t, fatalOutput)) != valueparam.FatalItemEnvelope {
		t.Fatalf("failure reference publication = %v", err)
	}
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"contentType":"application/json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	collected := filepath.Join(t.TempDir(), "collected.json")
	if err := (&RuntimeMapCollectOutcomesCommand{
		Input: `[` + strings.Join(outcomes, ",") + `]`, MaxAttempts: maxAttempts, Output: collected, DatastoreConfig: datastoreConfig,
	}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	argo, err := codec.Decode(t.Context(), readFile(t, collected))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(argo.Body, local) {
		t.Fatalf("Argo outcomes differ from local execution:\nargo  %s\nlocal %s", argo.Body, local)
	}
	for _, want := range []string{
		`"diagnostic":"step-execution-failure (exit 66): raise cannot be parsed","kind":"error"`,
		`"diagnostic":"non-retryable-step-failure (exit 67): refuse is not supported","kind":"non-retryable"`,
		`"diagnostic":"runner-killed (signal)","kind":"killed"`,
		`"diagnostic":"step-timeout (timed out after 2s)","kind":"timeout"`,
	} {
		if !strings.Contains(string(local), want) {
			t.Fatalf("outcomes %s lack %s", local, want)
		}
	}
}

func collectedMapOutput(t *testing.T, store, project, runID, nodeID string) []byte {
	t.Helper()
	runDirectory := filepath.Join(store, "projects", orchestrator.NormalizeProjectKey(project), "runs", runID)
	journal, err := runjournal.Parse(readFile(t, filepath.Join(runDirectory, "run-manifest.json")))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range journal.Steps {
		if step.NodeID == nodeID {
			return readFile(t, filepath.Join(store, filepath.FromSlash(step.Attempts[0].Output.Body.Key)))
		}
	}
	t.Fatalf("journal has no step %s", nodeID)
	return nil
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCollectOutcomesReportsLostAndFatalItems(t *testing.T) {
	datastoreConfig := localDatastoreConfig(t)
	codec, _, err := openRuntimeDatastore(t.Context(), datastoreConfig)
	if err != nil {
		t.Fatal(err)
	}
	failure, err := codec.EncodeItemFailure(t.Context(), 1, failureRecord("timeout", 3, "step-timeout (timed out after 1m0s)"))
	if err != nil {
		t.Fatal(err)
	}
	collected := filepath.Join(t.TempDir(), "collected.json")
	command := RuntimeMapCollectOutcomesCommand{
		Input:       `[{"index":0,"value":7},` + string(failure) + `]`,
		MaxAttempts: 3, Output: collected, DatastoreConfig: datastoreConfig,
	}
	if err := command.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := `[{"status":"succeeded","value":7},` +
		`{"failure":{"attempts":3,"diagnostic":"step-timeout (timed out after 1m0s)","kind":"timeout"},"status":"failed"}]`
	if got := string(readFile(t, collected)); got != want {
		t.Fatalf("collected = %s\nwant %s", got, want)
	}

	command.Input = `[{"lost":"terminal-pod-id"}]`
	if err := command.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "require the Argo workflow") {
		t.Fatalf("missing outcome without scheduler evidence = %v", err)
	}
	command.Input = `[{"index":0,"value":7},{"fatal":true}]`
	if err := command.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "map item 1 failed") {
		t.Fatalf("fatal marker error = %v", err)
	}
	for name, input := range map[string]string{
		"outcome in a failing map": `[` + string(failure) + `]`,
		"misplaced outcome":        `[` + string(failure) + `,{"index":0,"value":7}]`,
		"attempts beyond budget":   `[{"failure":{"attempts":4,"diagnostic":"","kind":"error"},"index":0}]`,
		"unknown kind":             `[{"failure":{"attempts":1,"diagnostic":"","kind":"lost"},"index":0}]`,
		"value and failure":        `[{"failure":{"attempts":1,"diagnostic":"","kind":"error"},"index":0,"value":1}]`,
		"indexed lost marker":      `[{"index":0,"lost":"terminal-pod-id"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if name == "outcome in a failing map" {
				err = (&RuntimeMapCollectCommand{Input: input, Output: collected, DatastoreConfig: datastoreConfig}).Run(t.Context())
			} else {
				err = (&RuntimeMapCollectOutcomesCommand{Input: input, MaxAttempts: 3, Output: collected, DatastoreConfig: datastoreConfig}).Run(t.Context())
			}
			if exitCodeFor(err) != 64 {
				t.Fatalf("error = %v (exit %d), want a contract violation", err, exitCodeFor(err))
			}
		})
	}
}

func failureRecord(kind string, attempts int, diagnostic string) mapexec.Failure {
	return mapexec.NewFailure(mapexec.FailureKind(kind), attempts, diagnostic)
}

func TestMapItemReportsFatalBeforeHydrationAndAfterPublicationErrors(t *testing.T) {
	for _, mode := range []string{"deleted", "corrupt", "invalid envelope", "unwritable output"} {
		t.Run(mode, func(t *testing.T) {
			config := localDatastoreConfig(t)
			codec, _, err := openRuntimeDatastore(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`[{"name":"` + strings.Repeat("x", 5000) + `"}]`)
			parameter, err := codec.Encode(t.Context(), body)
			if err != nil {
				t.Fatal(err)
			}
			expanded, err := codec.ExpandItems(t.Context(), parameter)
			if err != nil {
				t.Fatal(err)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(expanded, &items); err != nil {
				t.Fatal(err)
			}
			var binding struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(readFile(t, config), &binding); err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(binding.Path, "blobs", "sha256", strings.TrimPrefix(canonical.DigestBytes(body), "sha256:"))
			if mode == "deleted" || mode == "unwritable output" {
				if err := os.Remove(key); err != nil {
					t.Fatal(err)
				}
			} else if mode == "corrupt" {
				if err := os.WriteFile(key, bytes.Repeat([]byte("x"), len(body)), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				items[0] = json.RawMessage(`{"index":0,"value":null,"extra":true}`)
			}
			root := t.TempDir()
			output := filepath.Join(root, "result.json")
			if mode == "unwritable output" {
				if err := os.Mkdir(output, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			termination := filepath.Join(root, "termination.json")
			err = (&RuntimeMapItemCommand{Item: string(items[0]), Output: output, DatastoreConfig: config, TerminationLog: termination}).Run(t.Context())
			if err == nil {
				t.Fatal("runtime accepted damaged input")
			}
			evidence, phaseErr := executionphase.Parse(string(readFile(t, termination)))
			if phaseErr != nil || !evidence.Fatal {
				t.Fatalf("no fatal termination marker: %+v, %v", evidence, phaseErr)
			}
			if mode != "unwritable output" {
				marker := readFile(t, output)
				if string(marker) != valueparam.FatalItemEnvelope {
					t.Fatalf("marker = %s", marker)
				}
				if _, err := codec.CollectOutcomes(t.Context(), append(append([]byte("["), marker...), ']'), 3, nil); err == nil {
					t.Fatal("collector accepted fatal hydration")
				}
			}
			if mode == "corrupt" && exitCodeFor(err) != 64 {
				t.Fatalf("corruption exit = %d, error = %v", exitCodeFor(err), err)
			}
		})
	}
}

func TestMapItemRejectsSignalOutsideAuthorExecution(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(repository, "packages", "python", ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("Python SDK environment is unavailable")
	}
	t.Setenv("MASSIVE_PYTHON", python)
	root := t.TempDir()
	workflow := filepath.Join(root, "workflow.py")
	if err := os.WriteFile(workflow, []byte(`import os
import signal
from massive import GraphBuilder, MapItemOutcome, StepContext, container, execution
from pydantic import BaseModel, field_validator, field_serializer

class Item(BaseModel):
    value: int
    @field_validator("value")
    @classmethod
    def hydrate(cls, value: int) -> int:
        if value == 1:
            os.kill(os.getpid(), signal.SIGKILL)
        return value

class Output(BaseModel):
    value: int
    @field_serializer("value")
    def publish(self, value: int) -> int:
        if value == 3:
            os.kill(os.getpid(), signal.SIGKILL)
        return value

def scan(context: StepContext[Item]) -> Output:
    if context.inputs.value == 2:
        os.kill(os.getpid(), signal.SIGKILL)
    return Output(value=context.inputs.value)

graph = GraphBuilder(name="phase-evidence", input_type=list[Item], output_type=list[MapItemOutcome[Output]],
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "0" * 64)))
outcomes = graph.map(graph.start, scan, id="scan", item_failures="collect")
graph.edge_from(outcomes).to_end(graph.end)
`), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if err := (&BuildCommand{Entry: workflow + "#graph", Output: bundle, Profile: "test", RuntimeTransport: "embedded-v0", Namespace: "argo", ServiceAccount: "runner", ArtifactStore: "artifacts"}).Run(t.Context(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		value  int
		author bool
	}{
		{"hydrate", 1, false}, {"author", 2, true}, {"publish", 3, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			output, termination := filepath.Join(root, "result.json"), filepath.Join(root, "termination.json")
			err := (&RuntimeMapItemCommand{
				Plan: filepath.Join(bundle, "massive-plan.json"), RuntimeSources: RuntimeSources{BundleDir: filepath.Join(bundle, "runtime-assets")},
				Node: "scan", Item: fmt.Sprintf(`{"index":0,"value":{"value":%d}}`, test.value),
				Output: output, Project: "test/phase-evidence", RunID: test.name,
				DatastoreConfig: localDatastoreConfig(t), TerminationLog: termination,
			}).Run(t.Context())
			if test.author {
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(readFile(t, output), []byte(`"kind":"killed"`)) {
					t.Fatalf("no killed item outcome: %s", readFile(t, output))
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "outside author execution (phase "+test.name+")") {
					t.Fatalf("runtime signal became author failure: %v", err)
				}
				if string(readFile(t, output)) != valueparam.FatalItemEnvelope {
					t.Fatal("missing fatal result")
				}
				evidence, err := executionphase.Parse(string(readFile(t, termination)))
				if err != nil || !evidence.Fatal {
					t.Fatalf("missing fatal evidence: %+v, %v", evidence, err)
				}
			}
		})
	}
}
