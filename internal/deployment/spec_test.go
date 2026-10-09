package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
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
			RuntimeTransport:     "embedded-v0",
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
			"runtimeTransport":     "embedded-v0",
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

func TestRuntimeTransportIsARequiredDeploymentBinding(t *testing.T) {
	profile := Profile{Name: "argo", ArtifactStoreBinding: "artifacts", Target: Target{
		Kind: "argo", Namespace: "workflows", ServiceAccountName: "runner", RuntimeTransport: "embedded-v0",
	}}
	embedded, _, err := New(testPlanHash, profile, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	profile.Target.RuntimeTransport = "object-store-v0"
	objectStore, _, err := New(testPlanHash, profile, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if embedded.DeploymentHash == objectStore.DeploymentHash || embedded.PlanHash != objectStore.PlanHash {
		t.Fatalf("transport must change only deployment identity: %s/%s, %s/%s", embedded.DeploymentHash, embedded.PlanHash, objectStore.DeploymentHash, objectStore.PlanHash)
	}
	for _, transport := range []string{"", "configmap"} {
		profile.Target.RuntimeTransport = transport
		_, _, err := New(testPlanHash, profile, testPlanHash)
		var diagnostics *DiagnosticsError
		if !errors.As(err, &diagnostics) || !strings.Contains(fmt.Sprint(diagnostics.Diagnostics), "runtimeTransport") {
			t.Fatalf("transport %q error = %v, want a runtimeTransport diagnostic: %v", transport, err, diagnostics)
		}
	}
}

func TestTargetDiagnosticsNameOnlyTheFailingBranch(t *testing.T) {
	for name, target := range map[string]map[string]any{
		"missing kind":         {"namespace": "workflows"},
		"missing argo binding": {"kind": "argo", "namespace": "workflows", "serviceAccountName": "runner"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(deploymentJSON(t, map[string]any{"name": "argo", "artifactStoreBinding": "artifacts", "target": target}))
			var diagnostics *DiagnosticsError
			if !errors.As(err, &diagnostics) {
				t.Fatalf("error = %v", err)
			}
			rendered := fmt.Sprint(diagnostics.Diagnostics)
			missing := map[string]string{"missing kind": "'kind'", "missing argo binding": "'runtimeTransport'"}[name]
			if !strings.Contains(rendered, missing) || strings.Contains(rendered, "additional properties") {
				t.Fatalf("diagnostics = %s, want only the missing %s", rendered, missing)
			}
			for _, diagnostic := range diagnostics.Diagnostics {
				if !strings.HasPrefix(diagnostic.Ref, "/properties/profile/") {
					t.Fatalf("diagnostic ref %q is not a keyword location", diagnostic.Ref)
				}
			}
		})
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
			"runtimeTransport":   "object-store-v0",
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

func TestSecretBindingsValidateKubernetesNamesAndKeys(t *testing.T) {
	valid := []byte(`{"service-token":{"name":"service.credentials","key":".token"}}`)
	bindings, err := ParseSecretBindings(valid)
	if err != nil || bindings["service-token"].Key != ".token" {
		t.Fatalf("valid bindings: %v %v", bindings, err)
	}
	for _, body := range []string{
		`{"ref":{"name":"Invalid","key":"token"}}`,
		`{"ref":{"name":"service","key":"../token"}}`,
		`{"ref":{"name":"service","key":"..hidden"}}`,
		`{"ref":{"name":"service","key":"."}}`,
		`{"ref":{"name":"service","key":"token","value":"never-a-binding"}}`,
		`{"":{"name":"service","key":"token"}}`,
		`null`,
	} {
		if _, err := ParseSecretBindings([]byte(body)); err == nil {
			t.Fatalf("invalid bindings accepted: %s", body)
		}
	}
	profile := Profile{Name: "argo", ArtifactStoreBinding: "artifacts", Target: Target{Kind: "argo", Namespace: "default", ServiceAccountName: "runner", RuntimeTransport: "embedded-v0", SecretBindings: bindings}}
	first, _, err := New(testPlanHash, profile, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	profile.Target.SecretBindings["service-token"] = SecretKeyRef{Name: "other.credentials", Key: "token"}
	second, _, err := New(testPlanHash, profile, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanHash != second.PlanHash || first.DeploymentHash == second.DeploymentHash {
		t.Fatal("secret binding did not stay in deployment identity")
	}
}

func TestPlacementValidatesKubernetesFieldsStrictly(t *testing.T) {
	valid := []byte(`{
		"defaults": {
			"labels": {"cost-center": "research", "example.com/team": "data"},
			"annotations": {"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"},
			"tolerations": [{"key": "spot", "operator": "Exists", "effect": "NoSchedule"}],
			"priorityClassName": "batch"
		},
		"nodes": {
			"scan": {
				"nodeSelector": {"pool": "gpu"},
				"affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [{"matchExpressions": [{"key": "gpu", "operator": "In", "values": ["a100"]}]}]}}},
				"tolerations": [{"key": "nvidia.com/gpu", "operator": "Equal", "value": "present", "effect": "NoSchedule", "tolerationSeconds": 30}],
				"runtimeClassName": "gvisor",
				"podSpecPatch": {"containers": [{"name": "main", "resources": {"limits": {"nvidia.com/gpu": "1"}}}], "securityContext": {"runAsNonRoot": true}}
			}
		}
	}`)
	placement, err := ParsePlacement(valid)
	if err != nil {
		t.Fatal(err)
	}
	if placement.Nodes["scan"].RuntimeClassName != "gvisor" || len(placement.Defaults.Tolerations) != 1 {
		t.Fatalf("placement = %#v", placement)
	}
	profile := Profile{Name: "argo", ArtifactStoreBinding: "artifacts", Target: Target{Kind: "argo", Namespace: "default", ServiceAccountName: "runner", RuntimeTransport: "embedded-v0"}}
	unplaced, _, err := New(testPlanHash, profile, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	profile.Target.Placement = placement
	placed, body, err := New(testPlanHash, profile, testPlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if placed.PlanHash != unplaced.PlanHash || placed.DeploymentHash == unplaced.DeploymentHash {
		t.Fatal("placement did not stay in deployment identity")
	}
	if !strings.Contains(string(body), `"tolerationSeconds":30`) {
		t.Fatalf("Kubernetes values were not preserved verbatim: %s", body)
	}

	for name, test := range map[string]struct{ body, diagnostic string }{
		"empty":                     {`{}`, "minProperties"},
		"empty defaults":            {`{"defaults": {}}`, "minProperties"},
		"unknown placement field":   {`{"defaults": {"schedulerName": "custom"}}`, "schedulerName"},
		"misspelled affinity field": {`{"defaults": {"affinity": {"nodeAffinity": {"requiredDuringScheduling": {}}}}}`, "requiredDuringScheduling"},
		"misspelled toleration":     {`{"defaults": {"tolerations": [{"key": "spot", "operator": "Exists", "tolerationSecond": 5}]}}`, "tolerationSecond"},
		"toleration operator":       {`{"defaults": {"tolerations": [{"key": "spot", "operator": "Matches"}]}}`, "operator"},
		"exists with value":         {`{"defaults": {"tolerations": [{"key": "spot", "operator": "Exists", "value": "true"}]}}`, "tolerations/0"},
		"platform selector":         {`{"defaults": {"nodeSelector": {"kubernetes.io/arch": "arm64"}}}`, "nodeSelector"},
		"reserved label":            {`{"defaults": {"labels": {"workflows.argoproj.io/workflow": "x"}}}`, "labels"},
		"reserved annotation":       {`{"nodes": {"scan": {"annotations": {"massive.dev/plan-hash": "x"}}}}`, "annotations"},
		"label value":               {`{"defaults": {"labels": {"team": "not a label value"}}}`, "labels/team"},
		"runtime class":             {`{"defaults": {"runtimeClassName": "gVisor"}}`, "runtimeClassName"},
		"patched volumes":           {`{"defaults": {"podSpecPatch": {"volumes": [{"name": "massive-runtime", "emptyDir": {}}]}}}`, "volumes"},
		"patched runtime class":     {`{"defaults": {"podSpecPatch": {"runtimeClassName": "gvisor"}}}`, "runtimeClassName"},
		"patched sidecar":           {`{"defaults": {"podSpecPatch": {"containers": [{"name": "wait", "securityContext": {}}]}}}`, "containers/0/name"},
		"patched image":             {`{"defaults": {"podSpecPatch": {"containers": [{"name": "main", "image": "other"}]}}}`, "image"},
		"patched plan cpu":          {`{"defaults": {"podSpecPatch": {"containers": [{"name": "main", "resources": {"limits": {"cpu": "8"}}}]}}}`, "limits"},
		"patch directive":           {`{"defaults": {"podSpecPatch": {"$patch": "replace", "securityContext": {}}}}`, "$patch"},
		"misspelled security field": {`{"defaults": {"podSpecPatch": {"securityContext": {"runAsNonroot": true}}}}`, "runAsNonroot"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePlacement([]byte(test.body))
			var diagnostics *DiagnosticsError
			if !errors.As(err, &diagnostics) {
				t.Fatalf("invalid placement accepted or undiagnosed: %v", err)
			}
			found := false
			for _, diagnostic := range diagnostics.Diagnostics {
				found = found || strings.Contains(diagnostic.String(), test.diagnostic)
			}
			if !found {
				t.Fatalf("diagnostics do not name %q: %v", test.diagnostic, diagnostics.Diagnostics)
			}
		})
	}
}
