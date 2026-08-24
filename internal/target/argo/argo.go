// Package argo lowers verified static Graph IR and bound runtime assets into an
// executable Argo WorkflowTemplate bundle.
package argo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	RuntimeTransport          = "embedded-v0"
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

type RuntimeAssets struct {
	SourceArchives map[string][]byte
}

const maxEmbeddedRuntimeBytes = 700 * 1024

var argoFieldNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

// Compile accepts only separately verified deployment configuration and a
// canonical plan. The artifact store binding is an opaque Argo ConfigMap name:
// credentials are resolved by its repository configuration and pod identity.
func Compile(planJSON []byte, deploymentSpec *deployment.Spec, assets RuntimeAssets) (*Bundle, error) {
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
	if err := validateRuntimeAssets(p, planJSON, assets); err != nil {
		return nil, err
	}
	template, runtimeName, needsNetworkPolicy, err := workflowTemplate(p, deploymentSpec)
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
	templateYAML, err := yaml.JSONToYAML(templateJSON)
	if err != nil {
		return nil, fmt.Errorf("argo target: render YAML: %w", err)
	}
	configMapJSON, err := runtimeConfigMapJSON(p, deploymentSpec, runtimeName, planJSON, assets)
	if err != nil {
		return nil, err
	}
	files := []File{
		{"massive-plan.json", "application/json", "plan", planJSON},
		{"runtime-configmap.json", "application/json", "runtime-config", configMapJSON},
		{"workflow-template.json", "application/json", "workflow-template", templateJSON},
		{"workflow-template.yaml", "application/yaml", "workflow-template", templateYAML},
	}
	for _, sourcePackage := range p.GetSourcePackages() {
		name := "runtime-assets/source-sha256-" + strings.TrimPrefix(sourcePackage.GetPackageHash(), "sha256:") + ".tar"
		files = append(files, File{
			Path: name, ContentType: "application/vnd.massive.source-tar", Role: "source-archive",
			Bytes: assets.SourceArchives[sourcePackage.GetPackageHash()],
		})
	}
	if needsNetworkPolicy {
		networkPolicyJSON, err := runtimeNetworkPolicyJSON(deploymentSpec, runtimeName)
		if err != nil {
			return nil, err
		}
		files = append(files, File{"runtime-network-policy.json", "application/json", "network-policy", networkPolicyJSON})
	}
	return buildBundle(p, deploymentSpec, files)
}

func validateRuntimeAssets(p *planpb.WorkflowPlan, planJSON []byte, assets RuntimeAssets) error {
	total := len(planJSON)
	for _, sourcePackage := range p.GetSourcePackages() {
		archive := assets.SourceArchives[sourcePackage.GetPackageHash()]
		if len(archive) == 0 {
			return fmt.Errorf("argo target: verified source archive %s is required", sourcePackage.GetPackageHash())
		}
		total += len(archive)
	}
	if total > maxEmbeddedRuntimeBytes {
		return fmt.Errorf("argo target: embedded plan and source archives are %d bytes; 0.1 ConfigMap transport supports at most %d bytes", total, maxEmbeddedRuntimeBytes)
	}
	return nil
}

func validateStaticGraph(p *planpb.WorkflowPlan) error {
	g := p.GetGraph()
	if g.GetStartNode() == "" || g.GetEndNode() == "" {
		return errors.New("argo target: static graph requires start and end nodes")
	}
	nodes := map[string]*planpb.GraphNode{}
	for _, n := range g.GetNodes() {
		if n.GetId() == "" {
			return errors.New("argo target: graph contains a node without an id")
		}
		if nodes[n.GetId()] != nil {
			return fmt.Errorf("argo target: graph contains duplicate node id %q", n.GetId())
		}
		nodes[n.GetId()] = n
	}
	if nodes[g.GetStartNode()] == nil || nodes[g.GetStartNode()].GetKind() != "start" || nodes[g.GetEndNode()] == nil || nodes[g.GetEndNode()].GetKind() != "end" {
		return errors.New("argo target: graph must contain declared start/end sentinel nodes")
	}
	for _, n := range g.GetNodes() {
		if n.GetKind() == "decision" || n.GetKind() == "select" || n.GetKind() == "map" {
			return fmt.Errorf("argo target: graph semantic %q is unsupported; static Argo lowering supports only start, step, and end", n.GetKind())
		}
		if n.GetKind() != "start" && n.GetKind() != "step" && n.GetKind() != "end" {
			return fmt.Errorf("argo target: node %q has unsupported kind %q", n.GetId(), n.GetKind())
		}
		if n.GetKind() == "step" && (n.GetSymbolRef() == "" || n.GetContractRef() == "") {
			return fmt.Errorf("argo target: step %q lacks symbol or contract", n.GetId())
		}
	}
	inbound := make(map[string][]string, len(nodes))
	outbound := make(map[string][]string, len(nodes))
	edges := make(map[string]bool, len(g.GetEdges()))
	for _, edge := range g.GetEdges() {
		if nodes[edge.GetFrom()] == nil || nodes[edge.GetTo()] == nil {
			return fmt.Errorf("argo target: edge %q -> %q references an unknown node", edge.GetFrom(), edge.GetTo())
		}
		key := edge.GetFrom() + "\x00" + edge.GetTo()
		if edges[key] {
			return fmt.Errorf("argo target: duplicate edge %q -> %q", edge.GetFrom(), edge.GetTo())
		}
		edges[key] = true
		outbound[edge.GetFrom()] = append(outbound[edge.GetFrom()], edge.GetTo())
		inbound[edge.GetTo()] = append(inbound[edge.GetTo()], edge.GetFrom())
	}
	if len(inbound[g.GetStartNode()]) != 0 || len(outbound[g.GetStartNode()]) != 1 {
		return errors.New("argo target: start node must have no inbound edge and exactly one outbound edge")
	}
	if len(outbound[g.GetEndNode()]) != 0 || len(inbound[g.GetEndNode()]) != 1 {
		return errors.New("argo target: end node must have exactly one inbound edge and no outbound edge")
	}
	for _, node := range g.GetNodes() {
		if node.GetKind() != "step" || (len(inbound[node.GetId()]) <= 1 && len(node.GetMergeInputs()) == 0) {
			continue
		}
		declared := make(map[string]bool, len(node.GetMergeInputs()))
		for _, source := range node.GetMergeInputs() {
			declared[source] = true
		}
		if len(declared) != len(inbound[node.GetId()]) {
			return fmt.Errorf("argo target: step %q mergeInputs do not exactly match inbound edges", node.GetId())
		}
		for _, source := range inbound[node.GetId()] {
			if !declared[source] {
				return fmt.Errorf("argo target: step %q mergeInputs do not exactly match inbound edges", node.GetId())
			}
		}
	}
	seen := map[string]bool{g.GetStartNode(): true}
	queue := []string{g.GetStartNode()}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range outbound[current] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	if len(seen) != len(nodes) || !seen[g.GetEndNode()] {
		return errors.New("argo target: every node must be reachable from start and reach end")
	}
	canReachEnd := map[string]bool{g.GetEndNode(): true}
	queue = []string{g.GetEndNode()}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, previous := range inbound[current] {
			if !canReachEnd[previous] {
				canReachEnd[previous] = true
				queue = append(queue, previous)
			}
		}
	}
	if len(canReachEnd) != len(nodes) {
		return errors.New("argo target: every node must be reachable from start and reach end")
	}
	remaining := make(map[string]int, len(nodes))
	for id := range nodes {
		remaining[id] = len(inbound[id])
	}
	queue = queue[:0]
	for id, degree := range remaining {
		if degree == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range outbound[current] {
			remaining[next]--
			if remaining[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(nodes) {
		return errors.New("argo target: graph contains a cycle")
	}
	return nil
}

func workflowTemplate(p *planpb.WorkflowPlan, d *deployment.Spec) (map[string]any, string, bool, error) {
	g := p.GetGraph()
	name := d.Profile.Target.WorkflowTemplateName
	if name == "" {
		name = g.GetWorkflowName()
	}
	if name == "" {
		return nil, "", false, errors.New("argo target: deployment needs workflowTemplateName or plan workflow name")
	}
	name = argoFieldName(name)
	runtimeName := "massive-runtime-" + strings.TrimPrefix(d.DeploymentHash, "sha256:")[:16]
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
	taskNames := make(map[string]string, len(steps))
	for _, step := range steps {
		taskNames[step.GetId()] = argoFieldName(step.GetId())
	}
	dependencies := map[string][]string{}
	inbound := map[string][]string{}
	for _, step := range steps {
		dependencies[step.GetId()] = []string{}
	}
	for _, edge := range g.GetEdges() {
		inbound[edge.GetTo()] = append(inbound[edge.GetTo()], edge.GetFrom())
		if _, ok := dependencies[edge.GetTo()]; ok {
			if _, fromStep := dependencies[edge.GetFrom()]; fromStep {
				dependencies[edge.GetTo()] = append(dependencies[edge.GetTo()], edge.GetFrom())
			}
		}
	}
	tasks, templates := make([]any, 0, len(steps)), make([]any, 0, len(steps)+1)
	needsNetworkPolicy := false
	for _, step := range steps {
		contract := contracts[step.GetContractRef()]
		if contract == nil {
			return nil, "", false, fmt.Errorf("argo target: step %q references unknown contract", step.GetId())
		}
		env := envs[contract.GetEnvironmentRef()]
		if env == nil || env.GetKind() != "container-plan" || env.GetContainer().GetImage() == "" {
			return nil, "", false, fmt.Errorf("argo target: step %q requires a directly runnable container plan with image", step.GetId())
		}
		if len(contract.GetSecrets()) > 0 {
			return nil, "", false, fmt.Errorf("argo target: step %q declares secrets; 0.1 has no secret-ref lowering", step.GetId())
		}
		deps := dependencies[step.GetId()]
		sort.Strings(deps)
		argoDependencies := make([]string, len(deps))
		for index, dependency := range deps {
			argoDependencies[index] = taskNames[dependency]
		}
		inputExpression, err := argoInputExpression(step, inbound[step.GetId()], g.GetStartNode(), taskNames)
		if err != nil {
			return nil, "", false, err
		}
		task := map[string]any{
			"name": taskNames[step.GetId()], "template": argoFieldName("step-" + step.GetId()),
			"arguments": map[string]any{"parameters": []any{map[string]any{"name": "input", "value": inputExpression}}},
		}
		if len(argoDependencies) > 0 {
			task["dependencies"] = argoDependencies
		}
		tasks = append(tasks, task)
		args := []string{
			"runtime", "step",
			"--plan", "/var/run/massive/massive-plan.json",
			"--bundle-dir", "/var/run/massive",
			"--node", step.GetId(),
			"--input", "{{inputs.parameters.input}}",
			"--output", "/tmp/massive/result.json",
			"--project", "argo/" + name,
			"--run-id", "{{workflow.uid}}",
			"--store", "/tmp/massive/store",
		}
		runtime := env.GetContainer()
		command := runtime.GetCommand()
		if len(command) == 0 {
			command = []string{"massive"}
		}
		container := map[string]any{
			"image": runtime.GetImage(), "command": command, "args": args,
			"volumeMounts": []any{map[string]any{"name": "massive-runtime", "mountPath": "/var/run/massive", "readOnly": true}},
		}
		if runtime.GetWorkingDirectory() != "" {
			container["workingDir"] = runtime.GetWorkingDirectory()
		}
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
		stepTemplate := map[string]any{
			"name":   argoFieldName("step-" + step.GetId()),
			"inputs": map[string]any{"parameters": []any{map[string]any{"name": "input"}}},
			"outputs": map[string]any{"parameters": []any{map[string]any{
				"name": "result", "valueFrom": map[string]any{"path": "/tmp/massive/result.json"},
			}}},
			"container": container,
		}
		if network := contract.GetNetwork(); network != nil {
			switch network.GetEgress() {
			case "none":
				needsNetworkPolicy = true
				stepTemplate["metadata"] = map[string]any{"labels": map[string]string{"massive.dev/network-policy": runtimeName}}
			case "", "any":
			default:
				return nil, "", false, fmt.Errorf("argo target: step %q has unsupported egress policy %q", step.GetId(), network.GetEgress())
			}
		}
		platform := strings.Split(runtime.GetPlatform(), "/")
		if len(platform) != 2 {
			return nil, "", false, fmt.Errorf("argo target: step %q has invalid container platform %q", step.GetId(), runtime.GetPlatform())
		}
		stepTemplate["nodeSelector"] = map[string]string{
			"kubernetes.io/os":   platform[0],
			"kubernetes.io/arch": platform[1],
		}
		templates = append(templates, stepTemplate)
	}
	endInbound := inbound[g.GetEndNode()]
	if len(endInbound) != 1 {
		return nil, "", false, errors.New("argo target: end node requires one result source")
	}
	resultExpression := "{{tasks." + taskNames[endInbound[0]] + ".outputs.parameters.result}}"
	if endInbound[0] == g.GetStartNode() {
		resultExpression = "{{workflow.parameters.input}}"
	}
	main := map[string]any{
		"name": "main",
		"dag":  map[string]any{"tasks": tasks},
		"outputs": map[string]any{"parameters": []any{map[string]any{
			"name": "result", "valueFrom": map[string]any{"parameter": resultExpression},
		}}},
	}
	templates = append([]any{main}, templates...)
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "WorkflowTemplate",
		"metadata": map[string]any{
			"name": name, "namespace": d.Profile.Target.Namespace,
			"annotations": map[string]string{
				"massive.dev/plan-hash": p.GetPlanHash(), "massive.dev/deployment-hash": d.DeploymentHash,
				"massive.dev/execution-status": "executable-static", "massive.dev/runtime-transport": RuntimeTransport,
			},
		},
		"spec": map[string]any{
			"entrypoint": "main", "serviceAccountName": d.Profile.Target.ServiceAccountName,
			"automountServiceAccountToken": false,
			"arguments":                    map[string]any{"parameters": []any{map[string]any{"name": "input", "value": "null"}}},
			"volumes":                      []any{map[string]any{"name": "massive-runtime", "configMap": map[string]any{"name": runtimeName}}},
			"templates":                    templates,
		},
	}, runtimeName, needsNetworkPolicy, nil
}

func argoInputExpression(step *planpb.GraphNode, inbound []string, startNode string, taskNames map[string]string) (string, error) {
	if len(step.GetMergeInputs()) > 0 {
		parts := make([]string, 0, len(step.GetMergeInputs()))
		for _, source := range step.GetMergeInputs() {
			parts = append(parts, "{{tasks."+taskNames[source]+".outputs.parameters.result}}")
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	}
	if len(inbound) != 1 {
		return "", fmt.Errorf("argo target: step %q requires exactly one input source", step.GetId())
	}
	if inbound[0] == startNode {
		return "{{workflow.parameters.input}}", nil
	}
	return "{{tasks." + taskNames[inbound[0]] + ".outputs.parameters.result}}", nil
}

// argoFieldName projects the broader proto node-id space onto names accepted
// by Argo and Kubernetes. A digest suffix keeps normalized and truncated names
// stable without changing the semantic node id passed to the runtime.
func argoFieldName(source string) string {
	if len(source) <= 63 && argoFieldNamePattern.MatchString(source) {
		return source
	}
	digest := strings.TrimPrefix(canonical.DigestBytes([]byte(source)), "sha256:")[:12]
	var slug strings.Builder
	separator := false
	for _, character := range strings.ToLower(source) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			slug.WriteRune(character)
			separator = false
		} else if slug.Len() > 0 && !separator {
			slug.WriteByte('-')
			separator = true
		}
	}
	prefix := strings.Trim(slug.String(), "-")
	if prefix == "" {
		prefix = "node"
	}
	maximumPrefix := 63 - len(digest) - 1
	if len(prefix) > maximumPrefix {
		prefix = strings.TrimRight(prefix[:maximumPrefix], "-")
	}
	return prefix + "-" + digest
}

func runtimeConfigMapJSON(p *planpb.WorkflowPlan, d *deployment.Spec, name string, planJSON []byte, assets RuntimeAssets) ([]byte, error) {
	binaryData := map[string]string{"massive-plan.json": base64.StdEncoding.EncodeToString(planJSON)}
	for _, sourcePackage := range p.GetSourcePackages() {
		filename := "source-sha256-" + strings.TrimPrefix(sourcePackage.GetPackageHash(), "sha256:") + ".tar"
		binaryData[filename] = base64.StdEncoding.EncodeToString(assets.SourceArchives[sourcePackage.GetPackageHash()])
	}
	value := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": d.Profile.Target.Namespace},
		"binaryData": binaryData, "immutable": true,
	}
	return canonicalJSON(value)
}

func runtimeNetworkPolicyJSON(d *deployment.Spec, name string) ([]byte, error) {
	value := map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": d.Profile.Target.Namespace},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"massive.dev/network-policy": name}},
			"policyTypes": []string{"Egress"}, "egress": []any{},
		},
	}
	return canonicalJSON(value)
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
	manifest := &planpb.TargetBundleManifest{SchemaVersion: u32(0), Target: str(Kind), PlanHash: str(p.GetPlanHash()), BundleHash: str(bundleHash), Files: entries, Validations: []*planpb.ValidationResult{{Name: str("argo-schema"), Passed: boolp(true)}, {Name: str("dag-integrity"), Passed: boolp(true)}, {Name: str("credential-free-binding"), Passed: boolp(true)}}, Provenance: &planpb.BundleProvenance{CompilerName: str(p.GetProvenance().GetCompilerName()), CompilerVersion: str(p.GetProvenance().GetCompilerVersion())}, DeploymentHash: str(d.DeploymentHash)}
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
