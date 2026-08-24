package argo

import (
	"bytes"
	"encoding/base64"
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
	if len(first.Files) != 4 {
		t.Fatalf("bundle files = %#v", first.Files)
	}
	var template map[string]any
	workflowTemplate := fileByPath(t, first, "workflow-template.yaml")
	if err := yaml.Unmarshal(workflowTemplate.Bytes, &template); err != nil {
		t.Fatal(err)
	}
	serialized := string(workflowTemplate.Bytes)
	for _, forbidden := range []string{"accessKey", "secretKey", "credential", "sourceFetch"} {
		if strings.Contains(strings.ToLower(serialized), strings.ToLower(forbidden)) {
			t.Fatalf("template leaked %q: %s", forbidden, serialized)
		}
	}
	specValue := template["spec"].(map[string]any)
	if specValue["serviceAccountName"] != "massive-runner" {
		t.Fatalf("service account=%v", specValue["serviceAccountName"])
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
	if !containsArgs(args, "runtime", "step") || !containsArgs(args, "--node", "merge") {
		t.Fatalf("remote runtime command missing: %v", args)
	}
	input := merge["arguments"].(map[string]any)["parameters"].([]any)[0].(map[string]any)["value"]
	if input != "[{{tasks.left.outputs.parameters.result}},{{tasks.right.outputs.parameters.result}}]" {
		t.Fatalf("ordered merge input expression = %v", input)
	}
	fileByPath(t, first, "runtime-configmap.json")
	fileByPath(t, first, "runtime-network-policy.json")
	if first.Manifest.GetBundleHash() == "" || first.Manifest.GetPlanHash() == "" || first.Manifest.GetDeploymentHash() == "" {
		t.Fatal("manifest lacks identity hashes")
	}
}

func TestStaticDAGAcceptsBothPythonAndTypeScriptSymbols(t *testing.T) {
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
			b, err := Compile(canonicalPlan, deploymentForPlan(t, hash), runtimeAssetsForPlan(r.Plan))
			if err != nil {
				t.Fatal(err)
			}
			template := fileByPath(t, b, "workflow-template.yaml")
			if !strings.Contains(string(template.Bytes), "command:\n") || !strings.Contains(string(template.Bytes), "- massive") {
				t.Fatal("missing language-neutral runtime contract")
			}
		})
	}
}

func TestRuntimeConfigMapContainsExactVerifiedAssets(t *testing.T) {
	compiled := fixturePlan(t, "python-linear")
	assets := runtimeAssetsForPlan(compiled.Plan)
	bundle, err := Compile(compiled.CanonicalJSON, deploymentForPlan(t, compiled.PlanHash), assets)
	if err != nil {
		t.Fatal(err)
	}
	var configMap struct {
		BinaryData map[string]string `json:"binaryData"`
	}
	if err := json.Unmarshal(fileByPath(t, bundle, "runtime-configmap.json").Bytes, &configMap); err != nil {
		t.Fatal(err)
	}
	decodedPlan, err := base64.StdEncoding.DecodeString(configMap.BinaryData["massive-plan.json"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decodedPlan, compiled.CanonicalJSON) {
		t.Fatal("runtime ConfigMap changed the canonical plan bytes")
	}
	for hash, expected := range assets.SourceArchives {
		name := "source-sha256-" + strings.TrimPrefix(hash, "sha256:") + ".tar"
		actual, err := base64.StdEncoding.DecodeString(configMap.BinaryData[name])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("runtime ConfigMap changed source archive %s", hash)
		}
	}
}

func TestExecutableBundleRequiresBoundedRuntimeAssets(t *testing.T) {
	compiled := fixturePlan(t, "python-linear")
	if _, err := Compile(compiled.CanonicalJSON, deploymentForPlan(t, compiled.PlanHash), RuntimeAssets{}); err == nil || !strings.Contains(err.Error(), "source archive") {
		t.Fatalf("missing source assets error = %v", err)
	}
	assets := runtimeAssetsForPlan(compiled.Plan)
	for hash := range assets.SourceArchives {
		assets.SourceArchives[hash] = make([]byte, maxEmbeddedRuntimeBytes)
	}
	if _, err := Compile(compiled.CanonicalJSON, deploymentForPlan(t, compiled.PlanHash), assets); err == nil || !strings.Contains(err.Error(), "ConfigMap transport") {
		t.Fatalf("oversized source assets error = %v", err)
	}
}

func TestPassthroughReturnsWorkflowInputWithoutLaunchingAPod(t *testing.T) {
	bundle := compileFixture(t, "passthrough")
	var template map[string]any
	if err := yaml.Unmarshal(fileByPath(t, bundle, "workflow-template.yaml").Bytes, &template); err != nil {
		t.Fatal(err)
	}
	main := template["spec"].(map[string]any)["templates"].([]any)[0].(map[string]any)
	if tasks := main["dag"].(map[string]any)["tasks"].([]any); len(tasks) != 0 {
		t.Fatalf("passthrough tasks = %v, want none", tasks)
	}
	result := main["outputs"].(map[string]any)["parameters"].([]any)[0].(map[string]any)
	if result["valueFrom"].(map[string]any)["parameter"] != "{{workflow.parameters.input}}" {
		t.Fatalf("passthrough result = %v", result)
	}
}

func TestExecutableBundleRejectsUnloweredSecrets(t *testing.T) {
	compiled := fixturePlan(t, "python-linear")
	compiled.Plan.Contracts[0].Secrets = []*planpb.SecretRef{{Name: pointer("token"), Ref: pointer("secret/key")}}
	canonicalPlan, hash := rehashPlan(t, compiled.Plan)
	_, err := Compile(canonicalPlan, deploymentForPlan(t, hash), runtimeAssetsForPlan(compiled.Plan))
	if err == nil || !strings.Contains(err.Error(), "secret-ref lowering") {
		t.Fatalf("secret lowering error = %v", err)
	}
}

func TestArgoFieldNamesProjectBroaderProtoIdentitiesDeterministically(t *testing.T) {
	for _, valid := range []string{"step", "step-2"} {
		if got := argoFieldName(valid); got != valid {
			t.Fatalf("argoFieldName(%q) = %q", valid, got)
		}
	}
	for _, source := range []string{"add_one", ".hidden", strings.Repeat("long", 40)} {
		first := argoFieldName(source)
		second := argoFieldName(source)
		if first != second || len(first) > 63 || !argoFieldNamePattern.MatchString(first) {
			t.Fatalf("argoFieldName(%q) = %q, then %q", source, first, second)
		}
	}
}

func TestPythonFrontendFixtureLowersThroughArgoSchema(t *testing.T) {
	bundle := compileFixture(t, "python-linear")
	verifiedPlan, err := plan.VerifyCanonicalJSON(fileByPath(t, bundle, "massive-plan.json").Bytes, bundle.Manifest.GetPlanHash())
	if err != nil {
		t.Fatal(err)
	}
	if len(verifiedPlan.GetSymbols()) != 1 || verifiedPlan.GetSymbols()[0].GetLanguage() != "python" || verifiedPlan.GetSymbols()[0].GetModule() != "workflow" {
		t.Fatalf("compiled bundle plan lost Python symbol identity: %#v", verifiedPlan.GetSymbols())
	}

	var template map[string]any
	if err := yaml.Unmarshal(fileByPath(t, bundle, "workflow-template.yaml").Bytes, &template); err != nil {
		t.Fatal(err)
	}
	templateSpec := template["spec"].(map[string]any)
	main := templateSpec["templates"].([]any)[0].(map[string]any)
	task := main["dag"].(map[string]any)["tasks"].([]any)[0].(map[string]any)
	taskName := task["name"].(string)
	if strings.Contains(taskName, "_") || taskName == "add_one" {
		t.Fatalf("Python node id was not projected to an Argo-safe task name: %q", taskName)
	}
	step := templateByName(t, templateSpec["templates"].([]any), argoFieldName("step-add_one"))
	container := step["container"].(map[string]any)
	if container["image"] != "example.invalid/python-runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("generated Python step image = %v", container["image"])
	}
	annotations := template["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["massive.dev/execution-status"] != "executable-static" {
		t.Fatal("generated Python WorkflowTemplate is not marked executable")
	}
	if !containsArgs(container["args"].([]any), "--node", "add_one") {
		t.Fatal("runtime command did not preserve the proto node id")
	}
}

func TestStaticDAGRejectsUnverifiedOrUnsupportedPlan(t *testing.T) {
	result := fixturePlan(t, "linear-chain")
	d := deploymentForPlan(t, result.PlanHash)
	bad := append([]byte(nil), result.CanonicalJSON...)
	bad[len(bad)-1] = ' '
	if _, err := Compile(bad, d, runtimeAssetsForPlan(result.Plan)); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("error=%v, want canonical verification", err)
	}

	result = fixturePlan(t, "linear-chain")
	duplicate := result.Plan.GetGraph().GetEdges()[0]
	result.Plan.Graph.Edges = append(result.Plan.Graph.Edges, duplicate)
	canonicalPlan, hash := rehashPlan(t, result.Plan)
	if _, err := Compile(canonicalPlan, deploymentForPlan(t, hash), runtimeAssetsForPlan(result.Plan)); err == nil || !strings.Contains(err.Error(), "duplicate edge") {
		t.Fatalf("error=%v, want static graph diagnostic", err)
	}
}

func TestStaticDAGRejectsExhaustiveDecisionSemantics(t *testing.T) {
	result := fixturePlan(t, "exhaustive-decision")
	_, err := Compile(result.CanonicalJSON, deploymentForPlan(t, result.PlanHash), runtimeAssetsForPlan(result.Plan))
	if err == nil || !strings.Contains(err.Error(), `graph semantic "decision" is unsupported`) {
		t.Fatalf("error=%v, want explicit decision semantic diagnostic", err)
	}
}

func TestStaticDAGRejectsFiniteMapSemantics(t *testing.T) {
	result := fixturePlan(t, "finite-map")
	_, err := Compile(result.CanonicalJSON, deploymentForPlan(t, result.PlanHash), runtimeAssetsForPlan(result.Plan))
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
	b, err := Compile(r.CanonicalJSON, deploymentForPlan(t, r.PlanHash), runtimeAssetsForPlan(r.Plan))
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

func fileByPath(t *testing.T, bundle *Bundle, path string) File {
	t.Helper()
	for _, file := range bundle.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("bundle is missing %s", path)
	return File{}
}

func runtimeAssetsForPlan(plan *planpb.WorkflowPlan) RuntimeAssets {
	archives := make(map[string][]byte, len(plan.GetSourcePackages()))
	for _, sourcePackage := range plan.GetSourcePackages() {
		archives[sourcePackage.GetPackageHash()] = []byte("verified source archive")
	}
	return RuntimeAssets{SourceArchives: archives}
}
func pointer(v string) *string { return &v }
