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
