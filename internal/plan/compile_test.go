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

func TestCompileDoesNotInventMaterializedSourceArtifacts(t *testing.T) {
	specData := readFixture(t, "specs", "passthrough", "workflow-spec.json")
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}

	for _, sourcePackage := range compiled.Plan.GetSourcePackages() {
		if sourcePackage.GetManifest() != nil || sourcePackage.GetSourceArchive() != nil {
			t.Fatalf("unmaterialized source package contains artifact refs: %#v", sourcePackage)
		}
	}
}

func TestCompilePreservesPythonFrontendIdentity(t *testing.T) {
	specData := readFixture(t, "specs", "python-linear", "workflow-spec.json")
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}

	if got := compiled.Plan.GetGraph().GetIrVersion(); got != "0.1" {
		t.Fatalf("Graph IR version = %q, want 0.1", got)
	}
	if len(compiled.Plan.GetSymbols()) != 1 || compiled.Plan.GetSymbols()[0].GetLanguage() != "python" {
		t.Fatalf("compiled symbols = %#v, want one Python symbol", compiled.Plan.GetSymbols())
	}
	if len(compiled.Plan.GetSourcePackages()) != 1 || compiled.Plan.GetSourcePackages()[0].GetLanguage() != "python" {
		t.Fatalf("compiled source packages = %#v, want one Python package", compiled.Plan.GetSourcePackages())
	}
	if len(compiled.Plan.GetEnvironments()) != 1 || compiled.Plan.GetEnvironments()[0].GetContainer().GetImage() == "" {
		t.Fatalf("compiled environments = %#v, want runnable container plan", compiled.Plan.GetEnvironments())
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
