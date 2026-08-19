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
