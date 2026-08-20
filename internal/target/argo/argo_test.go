package argo

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/deployment"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"sigs.k8s.io/yaml"
)

func TestStaticDAGBundleIsDeterministicAndCredentialFree(t *testing.T) {
	first := compileFixture(t, "diamond")
	second := compileFixture(t, "diamond")
	if !bytes.Equal(first.ManifestJSON, second.ManifestJSON) {
		t.Fatal("bundle manifest is not deterministic")
	}
	if len(first.Files) != 2 || first.Files[0].Path != "massive-plan.json" || first.Files[1].Path != "workflow-template.yaml" {
		t.Fatalf("bundle files = %#v", first.Files)
	}
	var template map[string]any
	if err := yaml.Unmarshal(first.Files[1].Bytes, &template); err != nil {
		t.Fatal(err)
	}
	serialized := string(first.Files[1].Bytes)
	for _, forbidden := range []string{"accessKey", "secretKey", "credential", "sourceFetch", "/tmp/"} {
		if strings.Contains(strings.ToLower(serialized), strings.ToLower(forbidden)) {
			t.Fatalf("template leaked %q: %s", forbidden, serialized)
		}
	}
	specValue := template["spec"].(map[string]any)
	if specValue["serviceAccountName"] != "massive-runner" {
		t.Fatalf("service account=%v", specValue["serviceAccountName"])
	}
	if specValue["artifactRepositoryRef"].(map[string]any)["configMap"] != "staging-artifacts" {
		t.Fatal("opaque artifact-store binding missing")
	}
	main := specValue["templates"].([]any)[0].(map[string]any)
	tasks := main["dag"].(map[string]any)["tasks"].([]any)
	merge := taskByName(t, tasks, "merge")
	deps := merge["dependencies"].([]any)
	if len(deps) != 2 || deps[0] != "left" || deps[1] != "right" {
		t.Fatalf("merge readiness dependencies=%v", deps)
	}
	stepTemplate := templateByName(t, specValue["templates"].([]any), "step-merge")
	nodeSelector := stepTemplate["nodeSelector"].(map[string]any)
	if nodeSelector["kubernetes.io/os"] != "linux" || nodeSelector["kubernetes.io/arch"] != "amd64" {
		t.Fatalf("container platform was not lowered to node selector: %v", nodeSelector)
	}
	args := stepTemplate["container"].(map[string]any)["args"].([]any)
	if !containsArgs(args, "--merge-inputs", "left,right") {
		t.Fatalf("ordered merge inputs not preserved: %v", args)
	}
	if first.Manifest.GetBundleHash() == "" || first.Manifest.GetPlanHash() == "" || first.Manifest.GetDeploymentHash() == "" {
		t.Fatal("manifest lacks identity hashes")
	}
}

func TestStaticDAGAcceptsBothPythonAndTypeScriptSymbols(t *testing.T) {
	goldens := map[string]string{
		"typescript": "sha256:6339558310aa379e4877a759a344b5e160c92c7da16a155e69d8c0518eab3ce4",
		"python":     "sha256:7b777c4c7ac9c507a8a9c18e2baaf7cb5b9050d65e563f5a6299b42de779069f",
	}
	for _, language := range []string{"typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			r := fixturePlan(t, "linear-chain")
			for _, symbol := range r.Plan.Symbols {
				symbol.Language = pointer(language)
			}
			for _, pkg := range r.Plan.SourcePackages {
				pkg.Language = pointer(language)
			}
			canonicalPlan, hash := rehashPlan(t, r.Plan)
			b, err := Compile(canonicalPlan, deploymentForPlan(t, hash))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b.Files[1].Bytes), "massive-runner") {
				t.Fatal("missing language-neutral runtime contract")
			}
			got := canonical.DigestBytes(b.Files[1].Bytes)
			if got != goldens[language] {
				t.Fatalf("%s YAML golden = %s, want %s", language, got, goldens[language])
			}
		})
	}
}

func TestPythonFrontendFixtureLowersThroughArgoSchema(t *testing.T) {
	bundle := compileFixture(t, "python-linear")
	verifiedPlan, err := plan.VerifyCanonicalJSON(bundle.Files[0].Bytes, bundle.Manifest.GetPlanHash())
	if err != nil {
		t.Fatal(err)
	}
	if len(verifiedPlan.GetSymbols()) != 1 || verifiedPlan.GetSymbols()[0].GetLanguage() != "python" || verifiedPlan.GetSymbols()[0].GetModule() != "workflow" {
		t.Fatalf("compiled bundle plan lost Python symbol identity: %#v", verifiedPlan.GetSymbols())
	}

	var template map[string]any
	if err := yaml.Unmarshal(bundle.Files[1].Bytes, &template); err != nil {
		t.Fatal(err)
	}
	templateSpec := template["spec"].(map[string]any)
	step := templateByName(t, templateSpec["templates"].([]any), "step-add_one")
	container := step["container"].(map[string]any)
	if container["image"] != "example.invalid/python-runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("generated Python step image = %v", container["image"])
	}
	annotations := template["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["massive.dev/execution-status"] != "structural-only" {
		t.Fatal("generated Python WorkflowTemplate must preserve the honest execution boundary")
	}
}

func TestStaticDAGRejectsUnverifiedOrUnsupportedPlan(t *testing.T) {
	result := fixturePlan(t, "linear-chain")
	d := deploymentForPlan(t, result.PlanHash)
	bad := append([]byte(nil), result.CanonicalJSON...)
	bad[len(bad)-1] = ' '
	if _, err := Compile(bad, d); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("error=%v, want canonical verification", err)
	}

	result = fixturePlan(t, "linear-chain")
	duplicate := result.Plan.GetGraph().GetEdges()[0]
	result.Plan.Graph.Edges = append(result.Plan.Graph.Edges, duplicate)
	canonicalPlan, hash := rehashPlan(t, result.Plan)
	if _, err := Compile(canonicalPlan, deploymentForPlan(t, hash)); err == nil || !strings.Contains(err.Error(), "duplicate edge") {
		t.Fatalf("error=%v, want static graph diagnostic", err)
	}
}

func TestStaticDAGRejectsExhaustiveDecisionSemantics(t *testing.T) {
	result := fixturePlan(t, "exhaustive-decision")
	_, err := Compile(result.CanonicalJSON, deploymentForPlan(t, result.PlanHash))
	if err == nil || !strings.Contains(err.Error(), `graph semantic "decision" is unsupported`) {
		t.Fatalf("error=%v, want explicit decision semantic diagnostic", err)
	}
}

func TestStaticDAGRejectsFiniteMapSemantics(t *testing.T) {
	result := fixturePlan(t, "finite-map")
	_, err := Compile(result.CanonicalJSON, deploymentForPlan(t, result.PlanHash))
	if err == nil || !strings.Contains(err.Error(), `graph semantic "map" is unsupported`) {
		t.Fatalf("error=%v, want explicit map semantic diagnostic", err)
	}
}

func rehashPlan(t *testing.T, value *planpb.WorkflowPlan) ([]byte, string) {
	t.Helper()
	value.PlanHash = nil
	unhashed, err := plan.MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := canonical.DigestJSON(unhashed)
	if err != nil {
		t.Fatal(err)
	}
	value.PlanHash = pointer(hash)
	canonicalPlan, err := plan.MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return canonicalPlan, hash
}

func compileFixture(t *testing.T, name string) *Bundle {
	t.Helper()
	r := fixturePlan(t, name)
	b, err := Compile(r.CanonicalJSON, deploymentForPlan(t, r.PlanHash))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func fixturePlan(t *testing.T, name string) *plan.CompileResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "conformance", "fixtures", "specs", name, "workflow-spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	r, err := plan.Compile(s, data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func deploymentForPlan(t *testing.T, hash string) *deployment.Spec {
	t.Helper()
	v := map[string]any{"kind": "DeploymentSpec", "schemaVersion": 0, "encoding": "json-v0", "hashing": map[string]any{"algorithm": "sha256", "canonicalization": "canonical-json-v0", "recipe": "deployment-spec", "recipeVersion": 1}, "planHash": hash, "profile": map[string]any{"name": "argo-staging", "artifactStoreBinding": "staging-artifacts", "target": map[string]any{"kind": "argo", "namespace": "workflows", "serviceAccountName": "massive-runner", "workflowTemplateName": "massive-static"}}}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	v["deploymentHash"] = mustHash(t, raw)
	raw, err = json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	d, err := deployment.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func mustHash(t *testing.T, data []byte) string {
	t.Helper()
	h, err := canonical.DigestJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func taskByName(t *testing.T, tasks []any, name string) map[string]any {
	t.Helper()
	for _, v := range tasks {
		m := v.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("missing task %s", name)
	return nil
}
func templateByName(t *testing.T, templates []any, name string) map[string]any {
	t.Helper()
	for _, v := range templates {
		m := v.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("missing template %s", name)
	return nil
}
func containsArgs(args []any, values ...string) bool {
	for i := 0; i+len(values) <= len(args); i++ {
		ok := true
		for j := range values {
			if args[i+j] != values[j] {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}
func pointer(v string) *string { return &v }
