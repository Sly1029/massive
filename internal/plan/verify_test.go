package plan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/spec"
)

func TestVerifyCanonicalJSONAcceptsCompilerOutput(t *testing.T) {
	data := readFixture(t, "specs", "diamond", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}

	verified, err := VerifyCanonicalJSON(compiled.CanonicalJSON, compiled.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if verified.GetPlanHash() != compiled.PlanHash {
		t.Fatalf("verified plan hash = %q, want %q", verified.GetPlanHash(), compiled.PlanHash)
	}
}

func TestVerifyCanonicalJSONRejectsObsoletePlanSchema(t *testing.T) {
	data := readFixture(t, "plans", "linear-chain", "workflow-plan.json")
	mutated, hash := mutateAndResignPlan(t, data, func(root map[string]any) {
		root["schemaVersion"] = 0
	})
	if _, err := VerifyCanonicalJSON(mutated, hash); err == nil || !strings.Contains(err.Error(), "rebuild older plans") {
		t.Fatalf("obsolete plan accepted or missing rebuild diagnostic: %v", err)
	}
}

func TestVerifyCanonicalJSONAuthenticatesEnvironmentRequirements(t *testing.T) {
	data := readFixture(t, "plans", "linear-chain", "workflow-plan.json")
	for _, mutation := range []string{"missing-runtime", "duplicate", "mismatched-identity"} {
		t.Run(mutation, func(t *testing.T) {
			mutated, hash := mutateAndResignPlan(t, data, func(root map[string]any) {
				environments := root["environments"].([]any)
				requirement := environments[0].(map[string]any)
				switch mutation {
				case "missing-runtime":
					delete(requirement, "container")
				case "duplicate":
					root["environments"] = append(environments, requirement)
				case "mismatched-identity":
					requirement["container"].(map[string]any)["platform"] = "linux/arm64"
				}
			})
			if _, err := VerifyCanonicalJSON(mutated, hash); err == nil {
				t.Fatal("invalid environment requirement accepted")
			}
		})
	}
}

func TestVerifyCanonicalJSONRequiresEveryIdentityVersion(t *testing.T) {
	data := readFixture(t, "specs", "passthrough", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path []string
		want string
	}{
		{name: "plan schema", path: []string{"schemaVersion"}, want: "schemaVersion must be present"},
		{name: "plan hash recipe", path: []string{"hashing", "recipeVersion"}, want: "hashing descriptor must be present and complete"},
		{name: "spec hash recipe", path: []string{"specHashing", "recipeVersion"}, want: "specHash hashing descriptor must be present and complete"},
		{name: "graph IR", path: []string{"graph", "irVersion"}, want: "graph.irVersion must be present"},
		{name: "spec hash", path: []string{"specHash"}, want: "specHash must be present"},
		{name: "compiler", path: []string{"provenance", "compilerVersion"}, want: "provenance compiler name, version, and source spec hash must be present"},
		{name: "source package hash recipe", path: []string{"sourcePackages", "0", "hashing", "recipeVersion"}, want: "sourcePackages[0] hashing descriptor must be present and complete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := deleteCanonicalJSONField(t, compiled.CanonicalJSON, test.path)
			_, err := VerifyCanonicalJSON(mutated, compiled.PlanHash)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyCanonicalJSON() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestVerifyCanonicalJSONRejectsUnsupportedHashRecipeValues(t *testing.T) {
	data := readFixture(t, "specs", "passthrough", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Replace(
		compiled.CanonicalJSON,
		[]byte(`"recipe":"workflow-plan"`),
		[]byte(`"recipe":"source-package"`),
		1,
	)
	if bytes.Equal(mutated, compiled.CanonicalJSON) {
		t.Fatal("test did not alter the plan hash recipe")
	}
	_, err = VerifyCanonicalJSON(mutated, compiled.PlanHash)
	if err == nil || !strings.Contains(err.Error(), "workflow-plan@1") {
		t.Fatalf("VerifyCanonicalJSON() error = %v, want unsupported recipe rejection", err)
	}
}

func TestVerifyCanonicalJSONRejectsMalformedForeignHashReferences(t *testing.T) {
	data := readFixture(t, "specs", "passthrough", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "spec hash", mutate: func(root map[string]any) {
			root["specHash"] = "not-a-digest"
			root["provenance"].(map[string]any)["sourceSpecHash"] = "not-a-digest"
		}},
		{name: "source package hash", mutate: func(root map[string]any) {
			root["sourcePackages"].([]any)[0].(map[string]any)["packageHash"] = "not-a-digest"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated, expectedHash := mutateAndResignPlan(t, compiled.CanonicalJSON, test.mutate)
			if _, err := VerifyCanonicalJSON(mutated, expectedHash); err == nil || !strings.Contains(err.Error(), "sha256:<64 lowercase hex>") {
				t.Fatalf("VerifyCanonicalJSON() error = %v, want strict hash reference rejection", err)
			}
		})
	}
}

func deleteCanonicalJSONField(t *testing.T, data []byte, path []string) []byte {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	current := value
	for _, segment := range path[:len(path)-1] {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[segment]
		case []any:
			current = typed[0]
		default:
			t.Fatalf("path %v cannot traverse %T", path, current)
		}
	}
	delete(current.(map[string]any), path[len(path)-1])
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	result, err := canonical.CanonicalizeJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mutateAndResignPlan(t *testing.T, data []byte, mutate func(map[string]any)) ([]byte, string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	unsigned, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := canonical.DigestJSONWithRootMemberExcluded(unsigned, "planHash")
	if err != nil {
		t.Fatal(err)
	}
	root["planHash"] = hash
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalJSON, err := canonical.CanonicalizeJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return canonicalJSON, hash
}

func TestVerifyCanonicalJSONRejectsNonCanonicalBytes(t *testing.T) {
	data := readFixture(t, "specs", "passthrough", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical := append([]byte(" \n"), compiled.CanonicalJSON...)

	_, err = VerifyCanonicalJSON(nonCanonical, compiled.PlanHash)
	if err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("VerifyCanonicalJSON() error = %v, want non-canonical rejection", err)
	}
}

func TestVerifyCanonicalJSONRejectsTamperingAndWrongExpectedIdentity(t *testing.T) {
	data := readFixture(t, "specs", "linear-chain", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Replace(compiled.CanonicalJSON, []byte(`"workflowName":"linear-chain"`), []byte(`"workflowName":"linear-chains"`), 1)
	if bytes.Equal(tampered, compiled.CanonicalJSON) {
		t.Fatal("test did not alter plan bytes")
	}
	if _, err := VerifyCanonicalJSON(tampered, compiled.PlanHash); err == nil || !strings.Contains(err.Error(), "does not match canonical plan content") {
		t.Fatalf("tampered plan error = %v, want hash mismatch", err)
	}

	wrongHash := "sha256:" + strings.Repeat("0", 64)
	if _, err := VerifyCanonicalJSON(compiled.CanonicalJSON, wrongHash); err == nil || !strings.Contains(err.Error(), "does not match canonical plan content") {
		t.Fatalf("wrong identity error = %v, want hash mismatch", err)
	}
}
