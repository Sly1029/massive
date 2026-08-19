package sourceidentity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDigestMatchesSharedVersionedRecipeVector(t *testing.T) {
	fixtureRoot := filepath.Join("..", "..", "conformance", "fixtures", "hashing")
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "source-package-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var input hashInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join(fixtureRoot, "source-package-v1.sha256"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := Digest(input.Files)
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.TrimSpace(string(expected)) {
		t.Fatalf("Digest() = %q, want %q", got, strings.TrimSpace(string(expected)))
	}
}

func TestDigestRejectsNoncanonicalFileManifests(t *testing.T) {
	validHash := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name  string
		files []File
	}{
		{name: "unsorted", files: []File{{Path: "b.py", Hash: validHash}, {Path: "a.py", Hash: validHash}}},
		{name: "duplicate", files: []File{{Path: "a.py", Hash: validHash}, {Path: "a.py", Hash: validHash}}},
		{name: "dot prefix", files: []File{{Path: "./a.py", Hash: validHash}}},
		{name: "leading parent", files: []File{{Path: "../a.py", Hash: validHash}}},
		{name: "double slash", files: []File{{Path: "src//a.py", Hash: validHash}}},
		{name: "trailing slash", files: []File{{Path: "src/", Hash: validHash}}},
		{name: "backslash", files: []File{{Path: `src\a.py`, Hash: validHash}}},
		{name: "invalid hash", files: []File{{Path: "a.py", Hash: "sha256:not-a-digest"}}},
		{name: "empty", files: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Digest(test.files); err == nil {
				t.Fatal("Digest() accepted a noncanonical file manifest")
			}
		})
	}
}
