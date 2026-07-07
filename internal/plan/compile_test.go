package plan

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/spec"
)

var digestPattern = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

func TestCompileFixturesMatchGoldenPlans(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "passthrough"},
		{name: "linear-chain"},
		{name: "diamond"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			specData := readFixture(t, "specs", test.name, "workflow-spec.json")
			workflowSpec, err := spec.Parse(specData)
			if err != nil {
				t.Fatal(err)
			}

			first, err := Compile(workflowSpec, specData)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Compile(workflowSpec, specData)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.CanonicalJSON, second.CanonicalJSON) {
				t.Fatalf("compiled plan is not byte-stable\nfirst:  %s\nsecond: %s", first.CanonicalJSON, second.CanonicalJSON)
			}

			golden := readFixture(t, "plans", test.name, "workflow-plan.json")
			actualNormalized := normalizePlanJSON(t, first.CanonicalJSON)
			goldenNormalized := normalizePlanJSON(t, golden)
			if !bytes.Equal(actualNormalized, goldenNormalized) {
				t.Fatalf("plan mismatch\nactual:   %s\nexpected: %s", actualNormalized, goldenNormalized)
			}
		})
	}
}

func readFixture(t *testing.T, fixtureKind, name, file string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", fixtureKind, name, file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func normalizePlanJSON(t *testing.T, data []byte) []byte {
	t.Helper()

	normalized := digestPattern.ReplaceAll(data, []byte("sha256:0000000000000000000000000000000000000000000000000000000000000000"))
	normalized = omitEmptyRepeatedFields(t, normalized)
	canonicalJSON, err := canonical.CanonicalizeJSON(normalized)
	if err != nil {
		t.Fatal(err)
	}
	return canonicalJSON
}

func omitEmptyRepeatedFields(t *testing.T, data []byte) []byte {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}

	output, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return output
}
