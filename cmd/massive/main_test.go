package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/runjournal"
)

func TestCLIExplainsInvalidArguments(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "massive")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"inspect conflicting views", []string{"inspect", "run-id", "--project", "test", "--step", "task", "--json"}, "--step"},
		{"inspect requires project", []string{"inspect", "run-id"}, "--project"},
		{"unknown flag", []string{"run", "example.py", "--invalid"}, "--invalid"},
		{"missing build option", []string{"build", "example.py"}, "--output"},
		{"invalid target", []string{"build", "example.py", "--target", "invalid", "--output", "bundle", "--namespace", "default", "--service-account", "runner"}, "argo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(binary, tc.args...)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			var exit *exec.ExitError
			if err := command.Run(); !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("exit = %v, want 2", err)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want diagnostic containing %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("argument errors wrote to stdout: %q", stdout.String())
			}
		})
	}
}

func TestRunInputDefaultsToNull(t *testing.T) {
	input, err := (&RunCommand{}).input()
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != "null" {
		t.Fatalf("input = %q, want null", input)
	}
}

func TestRunInputReadsARealJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(`{"value": 41}`), 0o644); err != nil {
		t.Fatal(err)
	}
	input, err := (&RunCommand{InputFile: path}).input()
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != `{"value": 41}` {
		t.Fatalf("input = %q", input)
	}
}

func TestRunInputRejectsConflictingSources(t *testing.T) {
	_, err := (&RunCommand{Input: `{"value": 41}`, InputFile: "input.json"}).input()
	if err == nil {
		t.Fatal("expected --input and --input-file conflict")
	}
}

func TestRuntimeMapTransportExpandsAndCollectsThroughFiles(t *testing.T) {
	root := t.TempDir()
	expandedPath := filepath.Join(root, "expanded.json")
	if err := (&RuntimeMapExpandCommand{Input: `[3,3]`, Output: expandedPath}).Run(); err != nil {
		t.Fatal(err)
	}
	expanded, err := os.ReadFile(expandedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(expanded), `[{"index":0,"value":3},{"index":1,"value":3}]`; got != want {
		t.Fatalf("expanded = %s, want %s", got, want)
	}

	collectedPath := filepath.Join(root, "collected.json")
	if err := (&RuntimeMapCollectCommand{
		Input:  `[{"index":1,"value":"second"},{"index":0,"value":"first"}]`,
		Output: collectedPath,
	}).Run(); err != nil {
		t.Fatal(err)
	}
	collected, err := os.ReadFile(collectedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(collected), `["first","second"]`; got != want {
		t.Fatalf("collected = %s, want %s", got, want)
	}
}

func TestRuntimeMapEmptyMarkerDoesNotLoadOrInvokeAPlan(t *testing.T) {
	output := filepath.Join(t.TempDir(), "result.json")
	command := RuntimeMapItemCommand{Item: `{"empty":true}`, Output: output}
	if err := command.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"empty":true}` {
		t.Fatalf("empty marker result = %s", result)
	}
}

func TestInspectCommandRendersAndFiltersStoredJournals(t *testing.T) {
	store := t.TempDir()
	projectKey := orchestrator.NormalizeProjectKey("test/inspect")
	path := filepath.Join(store, "projects", projectKey, "runs", "recorded", "run-manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	journal := runjournal.Manifest{Kind: "RunManifest", SchemaVersion: 3, Encoding: "json-v3", PlanHash: "sha256:plan", ProjectKey: projectKey, RunID: "recorded", Status: "failed", Steps: []runjournal.Step{{NodeID: "task", Status: "failed", Attempts: []runjournal.Attempt{{Attempt: 1, Status: "failed", Input: runjournal.DataArtifact{Key: "input", Hash: "hash", ContentType: "application/json", Schema: "schema"}, Diagnostic: "step failed"}}}}, Decisions: []runjournal.Decision{}}
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		step  string
		json  bool
		want  string
		fails bool
	}{
		{"text", "", false, "step failed", false},
		{"step", "task", false, "task  failed", false},
		{"json", "", true, `"schemaVersion":3`, false},
		{"unknown step", "missing", false, "omit --step", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			err := (&InspectCommand{RunID: "recorded", Project: "test/inspect", Store: store, Step: tc.step, JSON: tc.json}).Run(context.Background(), &output)
			if tc.fails {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || !strings.Contains(output.String(), tc.want) {
				t.Fatalf("output=%q error=%v", output.String(), err)
			}
		})
	}
}
