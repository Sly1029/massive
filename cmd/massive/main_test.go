package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
