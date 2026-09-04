package deployment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/plan"
	workflowspec "github.com/Sly1029/massive/internal/spec"
)

const testPlanHash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func TestMaterializationBindingRequiresVersionOne(t *testing.T) {
	profile := Profile{Name: "local", ArtifactStoreBinding: "artifacts", Target: Target{Kind: "local"}}
	if _, _, err := New(testPlanHash, profile, ""); err == nil {
		t.Fatal("deployment accepted missing materialization identity")
	}
	bound, body, err := New(testPlanHash, profile, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if bound.SchemaVersion != 1 || bound.MaterializationHash != testPlanHash {
		t.Fatal("deployment omitted its required materialization binding")
	}
	for _, mutation := range []string{"v0-with-manifest", "v1-without-manifest"} {
		t.Run(mutation, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			if mutation == "v0-with-manifest" {
				value["schemaVersion"] = 0
			} else {
				delete(value, "materializationHash")
			}
			delete(value, "deploymentHash")
			unhashed, err := canonical.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			value["deploymentHash"] = canonical.DigestBytes(unhashed)
			invalid, err := canonical.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(invalid); err == nil {
				t.Fatal("invalid version/materialization combination accepted")
			}
		})
	}
}

func TestNewConstructsCanonicalValidatedDeployment(t *testing.T) {
	deployment, body, err := New(testPlanHash, Profile{
		Name:                 "argo-staging",
		ArtifactStoreBinding: "staging-artifacts",
		Target: Target{
			Kind:                 "argo",
			Namespace:            "workflows",
			ServiceAccountName:   "massive-runner",
			WorkflowTemplateName: "example-workflow",
		},
	}, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.PlanHash != testPlanHash || deployment.DeploymentHash == "" {
		t.Fatalf("deployment identities = %#v", deployment)
	}
	parsed, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DeploymentHash != deployment.DeploymentHash {
		t.Fatalf("parsed hash = %q, want %q", parsed.DeploymentHash, deployment.DeploymentHash)
	}
}

func TestDeploymentIdentityIsSeparateFromPlanIdentity(t *testing.T) {
	localData := deploymentJSON(t, map[string]any{
		"name":                 "local-dev",
		"artifactStoreBinding": "local-artifacts",
		"target":               map[string]any{"kind": "local"},
	})
	argoData := deploymentJSON(t, map[string]any{
		"name":                 "argo-staging",
		"artifactStoreBinding": "staging-artifacts",
		"target": map[string]any{
			"kind":                 "argo",
			"namespace":            "staging",
			"serviceAccountName":   "massive-runner",
			"workflowTemplateName": "example-workflow",
		},
	})

	local, err := Parse(localData)
	if err != nil {
		t.Fatal(err)
	}
	argo, err := Parse(argoData)
	if err != nil {
		t.Fatal(err)
	}

	if local.PlanHash != testPlanHash || argo.PlanHash != testPlanHash {
		t.Fatalf("deployment plan hashes = %q, %q; want %q", local.PlanHash, argo.PlanHash, testPlanHash)
	}
	if local.DeploymentHash == argo.DeploymentHash {
		t.Fatal("different deployment profiles must have distinct deployment hashes")
	}

	secondLocal, err := Parse(localData)
	if err != nil {
		t.Fatal(err)
	}
	if secondLocal.DeploymentHash != local.DeploymentHash {
		t.Fatalf("deployment hash is not deterministic: first %s, second %s", local.DeploymentHash, secondLocal.DeploymentHash)
	}
}

func TestParseSharedDeploymentFixtures(t *testing.T) {
	for _, name := range []string{"local", "argo"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "deployments", name, "deployment-spec.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeploymentProfilesReferenceTheSameCompiledPlan(t *testing.T) {
	workflowData, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "specs", "linear-chain", "workflow-spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := workflowspec.Parse(workflowData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(workflow, workflowData)
	if err != nil {
		t.Fatal(err)
	}
	local, err := Parse(deploymentJSONForPlan(t, compiled.PlanHash, map[string]any{
		"name":                 "local",
		"artifactStoreBinding": "local-artifacts",
		"target":               map[string]any{"kind": "local"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	argo, err := Parse(deploymentJSONForPlan(t, compiled.PlanHash, map[string]any{
		"name":                 "argo-staging",
		"artifactStoreBinding": "staging-artifacts",
		"target": map[string]any{
			"kind":               "argo",
			"namespace":          "staging",
			"serviceAccountName": "massive-runner",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if local.PlanHash != compiled.PlanHash || argo.PlanHash != compiled.PlanHash {
		t.Fatalf("deployment profiles must retain compiled plan hash %s; got %s and %s", compiled.PlanHash, local.PlanHash, argo.PlanHash)
	}
	if local.DeploymentHash == argo.DeploymentHash {
		t.Fatal("different deployment profiles must have different deployment hashes")
	}
}

func TestParseRejectsCredentialShapedDeploymentFields(t *testing.T) {
	data := deploymentJSON(t, map[string]any{
		"name":                 "argo-staging",
		"artifactStoreBinding": "staging-artifacts",
		"target": map[string]any{
			"kind":               "argo",
			"namespace":          "staging",
			"serviceAccountName": "massive-runner",
			"accessKeyId":        "not-allowed",
		},
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid deployment spec")
	}
}

func TestParseRejectsUnsupportedTargetKind(t *testing.T) {
	data := deploymentJSON(t, map[string]any{
		"name":                 "unsupported",
		"artifactStoreBinding": "artifacts",
		"target":               map[string]any{"kind": "unknown"},
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid deployment spec")
	}
}

func TestParseRejectsMismatchedDeploymentHash(t *testing.T) {
	data := deploymentJSON(t, map[string]any{
		"name":                 "local-dev",
		"artifactStoreBinding": "local-artifacts",
		"target":               map[string]any{"kind": "local"},
	})
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	value["deploymentHash"] = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Parse(data)
	if err == nil {
		t.Fatal("expected invalid deployment hash")
	}
	if !strings.Contains(err.Error(), "deploymentHash") {
		t.Fatalf("error = %v, want deployment hash diagnostic", err)
	}
}

func TestParseRequiresSupportedDeploymentHashRecipe(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing", mutate: func(value map[string]any) { delete(value, "hashing") }},
		{name: "future", mutate: func(value map[string]any) {
			value["hashing"].(map[string]any)["recipeVersion"] = 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(deploymentJSON(t, map[string]any{
				"name": "local", "artifactStoreBinding": "local-artifacts",
				"target": map[string]any{"kind": "local"},
			}), &value); err != nil {
				t.Fatal(err)
			}
			test.mutate(value)
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "hashing") {
				t.Fatalf("Parse() error = %v, want hashing diagnostic", err)
			}
		})
	}
}

func deploymentJSON(t *testing.T, profile map[string]any) []byte {
	t.Helper()
	return deploymentJSONForPlan(t, testPlanHash, profile)
}

func deploymentJSONForPlan(t *testing.T, planHash string, profile map[string]any) []byte {
	t.Helper()
	value := map[string]any{
		"kind":          "DeploymentSpec",
		"schemaVersion": 1,
		"encoding":      "json-v0",
		"hashing": map[string]any{
			"algorithm": "sha256", "canonicalization": "canonical-json-v0",
			"recipe": "deployment-spec", "recipeVersion": 1,
		},
		"planHash":            planHash,
		"materializationHash": testPlanHash,
		"profile":             profile,
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := RecomputedHash(body)
	if err != nil {
		t.Fatal(err)
	}
	value["deploymentHash"] = hash
	body, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
