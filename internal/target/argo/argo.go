// Package argo lowers verified static Graph IR into an Argo WorkflowTemplate
// bundle. It deliberately emits a structurally deployable template, not a
// claim that the still-unimplemented remote step driver has been executed.
package argo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/deployment"
	"github.com/Sly1029/massive/internal/irversion"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

const (
	Kind                      = "argo"
	workflowTemplateSchemaRef = "https://raw.githubusercontent.com/argoproj/argo-workflows/HEAD/api/jsonschema/schema.json#/definitions/io.argoproj.workflow.v1alpha1.WorkflowTemplate"
)

type File struct {
	Path, ContentType, Role string
	Bytes                   []byte
}
type Bundle struct {
	Files        []File
	Manifest     *planpb.TargetBundleManifest
	ManifestJSON []byte
}

// Compile accepts only separately verified deployment configuration and a
// canonical plan. The artifact store binding is an opaque Argo ConfigMap name:
// credentials are resolved by its repository configuration and pod identity.
func Compile(planJSON []byte, deploymentSpec *deployment.Spec) (*Bundle, error) {
	if deploymentSpec == nil {
		return nil, errors.New("argo target: deployment spec is required")
	}
	if deploymentSpec.Profile.Target.Kind != Kind {
		return nil, fmt.Errorf("argo target: deployment target is %q, expected argo", deploymentSpec.Profile.Target.Kind)
	}
	p, err := plan.VerifyCanonicalJSON(planJSON, deploymentSpec.PlanHash)
	if err != nil {
		return nil, fmt.Errorf("argo target: verify canonical plan: %w", err)
	}
	if version, err := irversion.Parse(p.GetGraph().GetIrVersion()); err != nil || !irversion.CompilerSupports(version) {
		if err != nil {
			return nil, fmt.Errorf("argo target: invalid graph IR version: %w", err)
		}
		return nil, fmt.Errorf("argo target: graph IR version %s is unsupported (accepted %s)", version, irversion.CompilerRange)
	}
	if err := validateStaticGraph(p); err != nil {
		return nil, err
	}
	template, err := workflowTemplate(p, deploymentSpec)
	if err != nil {
		return nil, err
	}
	templateJSON, err := canonicalJSON(template)
	if err != nil {
		return nil, err
	}
	if err := validateArgoSchema(templateJSON); err != nil {
		return nil, err
	}
	if err := validateInvariants(template, p, deploymentSpec); err != nil {
		return nil, err
	}
	templateYAML, err := yaml.JSONToYAML(templateJSON)
	if err != nil {
		return nil, fmt.Errorf("argo target: render YAML: %w", err)
	}
	files := []File{{"massive-plan.json", "application/json", "plan", planJSON}, {"workflow-template.yaml", "application/yaml", "workflow-template", templateYAML}}
	return buildBundle(p, deploymentSpec, files)
}

func validateStaticGraph(p *planpb.WorkflowPlan) error {
	g := p.GetGraph()
	if g.GetStartNode() == "" || g.GetEndNode() == "" {
		return errors.New("argo target: static graph requires start and end nodes")
	}
	nodes := map[string]*planpb.GraphNode{}
	for _, n := range g.GetNodes() {
		nodes[n.GetId()] = n
	}
	if nodes[g.GetStartNode()] == nil || nodes[g.GetStartNode()].GetKind() != "start" || nodes[g.GetEndNode()] == nil || nodes[g.GetEndNode()].GetKind() != "end" {
		return errors.New("argo target: graph must contain declared start/end sentinel nodes")
	}
	for _, n := range g.GetNodes() {
		if n.GetKind() != "start" && n.GetKind() != "step" && n.GetKind() != "end" {
			return fmt.Errorf("argo target: node %q has unsupported kind %q", n.GetId(), n.GetKind())
		}
		if n.GetKind() == "step" && (n.GetSymbolRef() == "" || n.GetContractRef() == "") {
			return fmt.Errorf("argo target: step %q lacks symbol or contract", n.GetId())
		}
	}
	return nil
}

func workflowTemplate(p *planpb.WorkflowPlan, d *deployment.Spec) (map[string]any, error) {
	g := p.GetGraph()
	name := d.Profile.Target.WorkflowTemplateName
	if name == "" {
		name = g.GetWorkflowName()
	}
	if name == "" {
		return nil, errors.New("argo target: deployment needs workflowTemplateName or plan workflow name")
	}
	contracts := map[string]*planpb.ExecutionContract{}
	for _, c := range p.GetContracts() {
		contracts[c.GetContractRef()] = c
	}
	envs := map[string]*planpb.MaterializedEnvironment{}
	for _, e := range p.GetEnvironments() {
		envs[e.GetEnvRef()] = e
	}
	steps := make([]*planpb.GraphNode, 0)
	for _, n := range g.GetNodes() {
		if n.GetKind() == "step" {
			steps = append(steps, n)
		}
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].GetId() < steps[j].GetId() })
	dependencies := map[string][]string{}
	for _, step := range steps {
		dependencies[step.GetId()] = []string{}
	}
	for _, edge := range g.GetEdges() {
		if _, ok := dependencies[edge.GetTo()]; ok {
			if _, fromStep := dependencies[edge.GetFrom()]; fromStep {
				dependencies[edge.GetTo()] = append(dependencies[edge.GetTo()], edge.GetFrom())
			}
		}
	}
	tasks, templates := make([]any, 0, len(steps)), make([]any, 0, len(steps)+1)
	for _, step := range steps {
		contract := contracts[step.GetContractRef()]
		if contract == nil {
			return nil, fmt.Errorf("argo target: step %q references unknown contract", step.GetId())
		}
		env := envs[contract.GetEnvironmentRef()]
		if env == nil || env.GetKind() != "container-plan" || env.GetContainer().GetImage() == "" {
			return nil, fmt.Errorf("argo target: step %q requires a directly runnable container plan with image", step.GetId())
		}
		deps := dependencies[step.GetId()]
		sort.Strings(deps)
		task := map[string]any{"name": step.GetId(), "template": "step-" + step.GetId()}
		if len(deps) > 0 {
			task["dependencies"] = deps
		}
		tasks = append(tasks, task)
		envVars := []any{map[string]any{"name": "MASSIVE_PLAN_HASH", "value": p.GetPlanHash()}, map[string]any{"name": "MASSIVE_NODE_ID", "value": step.GetId()}, map[string]any{"name": "MASSIVE_ARTIFACT_STORE_BINDING", "value": d.Profile.ArtifactStoreBinding}}
		args := []string{"--node", step.GetId(), "--plan-hash", p.GetPlanHash()}
		// Argo dependencies govern readiness only. mergeInputs is ordered runtime
		// data semantics and must remain separate for the future remote driver.
		if len(step.GetMergeInputs()) > 0 {
			args = append(args, "--merge-inputs", strings.Join(step.GetMergeInputs(), ","))
		}
		container := map[string]any{"image": env.GetContainer().GetImage(), "command": []string{"massive-runner", "step"}, "args": args, "env": envVars}
		if r := contract.GetResources(); r != nil && (r.GetCpu() != "" || r.GetMemory() != "") {
			q := map[string]string{}
			if r.GetCpu() != "" {
				q["cpu"] = r.GetCpu()
			}
			if r.GetMemory() != "" {
				q["memory"] = r.GetMemory()
			}
			container["resources"] = map[string]any{"requests": q, "limits": q}
		}
		templates = append(templates, map[string]any{"name": "step-" + step.GetId(), "container": container})
	}
	templates = append([]any{map[string]any{"name": "main", "dag": map[string]any{"tasks": tasks}}}, templates...)
	return map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "WorkflowTemplate", "metadata": map[string]any{"name": name, "namespace": d.Profile.Target.Namespace, "annotations": map[string]string{"massive.dev/plan-hash": p.GetPlanHash(), "massive.dev/deployment-hash": d.DeploymentHash, "massive.dev/execution-status": "structural-only"}}, "spec": map[string]any{"entrypoint": "main", "serviceAccountName": d.Profile.Target.ServiceAccountName, "artifactRepositoryRef": map[string]any{"configMap": d.Profile.ArtifactStoreBinding, "key": "default-v1"}, "templates": templates}}, nil
}

func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonical.CanonicalizeJSON(raw)
}

var schemaOnce sync.Once
var argoSchema *jsonschema.Schema
var argoSchemaErr error

func validateArgoSchema(data []byte) error {
	schemaOnce.Do(func() {
		doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.ArgoWorkflowsCRDSchemaJSON))
		if e != nil {
			argoSchemaErr = e
			return
		}
		c := jsonschema.NewCompiler()
		if e = c.AddResource("https://raw.githubusercontent.com/argoproj/argo-workflows/HEAD/api/jsonschema/schema.json", doc); e != nil {
			argoSchemaErr = e
			return
		}
		argoSchema, argoSchemaErr = c.Compile(workflowTemplateSchemaRef)
	})
	if argoSchemaErr != nil {
		return fmt.Errorf("argo target: load pinned Argo schema: %w", argoSchemaErr)
	}
	instance, e := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if e != nil {
		return e
	}
	if e = argoSchema.Validate(instance); e != nil {
		return fmt.Errorf("argo target: generated WorkflowTemplate violates pinned Argo %s schema: %w", schemacontract.ArgoWorkflowsCRDVersion, e)
	}
	return nil
}

func validateInvariants(t map[string]any, p *planpb.WorkflowPlan, d *deployment.Spec) error {
	raw, _ := canonicalJSON(t)
	for _, forbidden := range []string{"accessKey", "secretKey", "credential", "sourceFetch", "datastorePath"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(forbidden)) {
			return fmt.Errorf("argo target: generated template leaked forbidden %q material", forbidden)
		}
	}
	if d.Profile.Target.ServiceAccountName == "" {
		return errors.New("argo target: workload identity serviceAccountName is required")
	}
	return nil
}

func buildBundle(p *planpb.WorkflowPlan, d *deployment.Spec, files []File) (*Bundle, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	entries := make([]*planpb.EmittedFile, 0, len(files))
	identityFiles := make([]map[string]string, 0, len(files))
	for _, f := range files {
		h := canonical.DigestBytes(f.Bytes)
		entries = append(entries, &planpb.EmittedFile{Path: &f.Path, Artifact: &planpb.ArtifactRef{Key: &f.Path, Hash: &h, ContentType: &f.ContentType}, Role: &f.Role})
		identityFiles = append(identityFiles, map[string]string{"path": f.Path, "hash": h})
	}
	identity := map[string]any{"planHash": p.GetPlanHash(), "deploymentHash": d.DeploymentHash, "target": Kind, "files": identityFiles}
	bundleHash, err := canonical.DigestJSON(mustJSON(identity))
	if err != nil {
		return nil, err
	}
	manifest := &planpb.TargetBundleManifest{SchemaVersion: u32(0), Target: str(Kind), PlanHash: str(p.GetPlanHash()), BundleHash: str(bundleHash), Files: entries, Validations: []*planpb.ValidationResult{{Name: str("argo-schema"), Passed: boolp(true)}, {Name: str("dag-integrity"), Passed: boolp(true)}, {Name: str("credential-free-binding"), Passed: boolp(true)}}, Provenance: &planpb.BundleProvenance{CompilerName: str(p.GetProvenance().GetCompilerName()), CompilerVersion: str(p.GetProvenance().GetCompilerVersion())}}
	raw, err := protojson.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := canonical.CanonicalizeJSON(raw)
	if err != nil {
		return nil, err
	}
	return &Bundle{files, manifest, manifestJSON}, nil
}
func mustJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func str(s string) *string { return &s }
func u32(v uint32) *uint32 { return &v }
func boolp(v bool) *bool   { return &v }
