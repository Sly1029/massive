package argo

import (
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/spec"
)

// Argo WorkflowTemplate structs. These cover only the fields the v0 wedge emits;
// json tags with omitempty keep the rendered manifest minimal and deterministic.

type workflowTemplate struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   objectMeta           `json:"metadata"`
	Spec       workflowTemplateSpec `json:"spec"`
}

type objectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type workflowTemplateSpec struct {
	Entrypoint         string     `json:"entrypoint"`
	Arguments          *arguments `json:"arguments,omitempty"`
	ServiceAccountName string     `json:"serviceAccountName,omitempty"`
	Templates          []template `json:"templates"`
}

// arguments/parameter declare the WorkflowTemplate's workflow-level parameters.
// The wedge declares them with names only (no defaults): the step containers
// reference them for datastore and project configuration, and WS-8 supplies real
// values at submit time.
type arguments struct {
	Parameters []parameter `json:"parameters,omitempty"`
}

type parameter struct {
	Name  string  `json:"name"`
	Value *string `json:"value,omitempty"`
}

type template struct {
	Name      string       `json:"name"`
	DAG       *dagTemplate `json:"dag,omitempty"`
	Container *container   `json:"container,omitempty"`
}

type dagTemplate struct {
	Tasks []dagTask `json:"tasks"`
}

type dagTask struct {
	Name         string   `json:"name"`
	Template     string   `json:"template"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type container struct {
	Image     string                `json:"image"`
	Command   []string              `json:"command,omitempty"`
	Args      []string              `json:"args,omitempty"`
	Env       []envVar              `json:"env,omitempty"`
	Resources *resourceRequirements `json:"resources,omitempty"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

const (
	apiVersion             = "argoproj.io/v1alpha1"
	workflowTemplateKind   = "WorkflowTemplate"
	entrypointTemplateName = "main"
	stepTemplatePrefix     = "step-"

	// stepDriverCommand + stepDriverSubcommand are the Massive step driver the
	// runtime image ships alongside the TS runner. Each step pod runs one driver
	// invocation that executes exactly one plan node: it loads the plan from the
	// datastore, materializes the node input from upstream outputs, builds a
	// schema-valid StepInvocationDescriptor, and invokes the runner. No
	// descriptor is embedded in the template — descriptors are built at run time,
	// so they are always schema-valid and identical to a local run's.
	stepDriverCommand            = "massive-orchestrator"
	stepDriverSubcommand         = "step"
	stepDriverFinalizeSubcommand = "finalize"

	// wfSystemPrefix namespaces compiler-generated system resources (argo-backend.md
	// reserves wf-system-*). User step ids may not use it (see validateNames);
	// full reserved-name invariant enforcement is WS-10.
	wfSystemPrefix = "wf-system-"
	// finalizeTaskName is the system task that finalizes the run: it composes the
	// terminal run manifest from the per-node entries and writes result.json.
	finalizeTaskName = wfSystemPrefix + "finalize"

	// argoRunIDVariable is Argo's per-run unique id, substituted into the run-id
	// argument when the pod starts, so one WorkflowTemplate serves every run.
	argoRunIDVariable = "{{workflow.uid}}"

	// PlanHashAnnotation carries the compiled plan hash on the generated template.
	// The plan-provenance invariant asserts it exists and matches the plan.
	PlanHashAnnotation = "massive.dev/plan-hash"
)

// Workflow parameter names and the container env var names each is bound to. The
// step driver reads datastore and project configuration from these env vars; the
// template binds each env var to a workflow parameter reference, so the runtime
// image and the generated YAML carry no datastore coordinates or credentials —
// only parameter references and env var names. WS-8 supplies the values.
const (
	paramDatastoreKind = "datastore-kind"
	paramDatastorePath = "datastore-path"
	paramProjectID     = "project-id"

	envDatastoreKind = "MASSIVE_DATASTORE_KIND"
	envDatastorePath = "MASSIVE_DATASTORE_PATH"
	envProjectID     = "MASSIVE_PROJECT_ID"
)

// buildWorkflowTemplate materializes the Argo WorkflowTemplate tree from the
// plan. The DAG mirrors the plan topology exactly: one task per step node with
// dependencies equal to the plan's step-to-step edges (the __start/__end
// sentinels are skipped). Each step template runs the Massive step driver in the
// container-env image; the driver builds its StepInvocationDescriptor at run
// time from the plan and the datastore, so data flows step-to-step through the
// datastore in DAG order rather than through embedded artifacts.
func buildWorkflowTemplate(index planIndex, input compileContext) (*workflowTemplate, error) {
	graph := input.plan.GetGraph()

	name := input.config.WorkflowTemplateName
	if name == "" {
		name = graph.GetWorkflowName()
	}
	if name == "" {
		return nil, fmt.Errorf("plan has no workflow name and target set no workflowTemplateName")
	}
	if err := validateNames(name, index); err != nil {
		return nil, err
	}

	dag := &dagTemplate{Tasks: make([]dagTask, 0, len(index.stepOrder)+1)}
	templates := make([]template, 0, len(index.stepOrder)+2)

	for _, nodeID := range index.stepOrder {
		node := index.nodesByID[nodeID]
		stepTemplate, err := buildStepTemplate(index, input.plan.GetPlanHash(), node)
		if err != nil {
			return nil, err
		}
		templates = append(templates, stepTemplate)
		dag.Tasks = append(dag.Tasks, dagTask{
			Name:         nodeID,
			Template:     stepTemplateName(nodeID),
			Dependencies: index.stepDependencies[nodeID],
		})
	}

	// A system finalize task depends on the steps feeding the end node and runs
	// the finalize driver, so an all-green DAG composes the terminal run manifest
	// and writes result.json — the run reaches Succeeded instead of hanging.
	finalizeTemplate, err := buildFinalizeTemplate(index, input.plan.GetPlanHash())
	if err != nil {
		return nil, err
	}
	templates = append(templates, finalizeTemplate)
	dag.Tasks = append(dag.Tasks, dagTask{
		Name:         finalizeTaskName,
		Template:     finalizeTaskName,
		Dependencies: index.endUpstreamSteps,
	})

	allTemplates := append([]template{{Name: entrypointTemplateName, DAG: dag}}, templates...)

	return &workflowTemplate{
		APIVersion: apiVersion,
		Kind:       workflowTemplateKind,
		Metadata: objectMeta{
			Name:        name,
			Namespace:   input.config.Namespace,
			Annotations: map[string]string{PlanHashAnnotation: input.plan.GetPlanHash()},
		},
		Spec: workflowTemplateSpec{
			Entrypoint:         entrypointTemplateName,
			Arguments:          runParameters(),
			ServiceAccountName: input.config.ServiceAccountName,
			Templates:          allTemplates,
		},
	}, nil
}

// runParameters declares the datastore/project workflow parameters the step
// containers read, with names only and no defaults, so submit-time values (WS-8)
// supply the datastore location and project identity.
func runParameters() *arguments {
	return &arguments{Parameters: []parameter{
		{Name: paramDatastoreKind},
		{Name: paramDatastorePath},
		{Name: paramProjectID},
	}}
}

func buildStepTemplate(index planIndex, planHash string, node *planpb.GraphNode) (template, error) {
	contract, ok := index.contractsByRef[node.GetContractRef()]
	if !ok {
		return template{}, fmt.Errorf("step %q references unknown contract %q", node.GetId(), node.GetContractRef())
	}
	environment, ok := index.environmentsByRef[contract.GetEnvironmentRef()]
	if !ok {
		return template{}, fmt.Errorf("step %q references unknown environment %q", node.GetId(), contract.GetEnvironmentRef())
	}

	return template{
		Name: stepTemplateName(node.GetId()),
		Container: &container{
			Image:   environment.GetContainer().GetImage(),
			Command: []string{stepDriverCommand, stepDriverSubcommand},
			// Static template args: which node, which plan. The run id is Argo's
			// per-run uid, substituted at pod start.
			Args: []string{
				"--node", node.GetId(),
				"--run-id", argoRunIDVariable,
				"--plan-hash", planHash,
			},
			Env: []envVar{
				{Name: envDatastoreKind, Value: parameterRef(paramDatastoreKind)},
				{Name: envDatastorePath, Value: parameterRef(paramDatastorePath)},
				{Name: envProjectID, Value: parameterRef(paramProjectID)},
			},
			Resources: containerResources(contract.GetResources()),
		},
	}, nil
}

// buildFinalizeTemplate builds the system finalize container template. It reuses
// the runtime image of the (sorted-first) step feeding the end node — every wedge
// step ships the driver, so any qualifies — and runs the finalize subcommand with
// the same datastore/project env contract as the step containers.
func buildFinalizeTemplate(index planIndex, planHash string) (template, error) {
	if len(index.endUpstreamSteps) == 0 {
		return template{}, fmt.Errorf("plan has no step feeding the end node; the argo target cannot finalize the run")
	}
	image, err := stepImage(index, index.endUpstreamSteps[0])
	if err != nil {
		return template{}, err
	}
	return template{
		Name: finalizeTaskName,
		Container: &container{
			Image:   image,
			Command: []string{stepDriverCommand, stepDriverFinalizeSubcommand},
			Args: []string{
				"--run-id", argoRunIDVariable,
				"--plan-hash", planHash,
			},
			Env: []envVar{
				{Name: envDatastoreKind, Value: parameterRef(paramDatastoreKind)},
				{Name: envDatastorePath, Value: parameterRef(paramDatastorePath)},
				{Name: envProjectID, Value: parameterRef(paramProjectID)},
			},
		},
	}, nil
}

// stepImage resolves a step node's container-env image via its contract.
func stepImage(index planIndex, nodeID string) (string, error) {
	node, ok := index.nodesByID[nodeID]
	if !ok {
		return "", fmt.Errorf("step %q not found in plan", nodeID)
	}
	contract, ok := index.contractsByRef[node.GetContractRef()]
	if !ok {
		return "", fmt.Errorf("step %q references unknown contract %q", nodeID, node.GetContractRef())
	}
	environment, ok := index.environmentsByRef[contract.GetEnvironmentRef()]
	if !ok {
		return "", fmt.Errorf("step %q references unknown environment %q", nodeID, contract.GetEnvironmentRef())
	}
	return environment.GetContainer().GetImage(), nil
}

func parameterRef(name string) string {
	return "{{workflow.parameters." + name + "}}"
}

func containerResources(resources *planpb.ResourceRequirements) *resourceRequirements {
	if resources == nil {
		return nil
	}
	quantities := map[string]string{}
	if cpu := resources.GetCpu(); cpu != "" {
		quantities["cpu"] = cpu
	}
	if memory := resources.GetMemory(); memory != "" {
		quantities["memory"] = memory
	}
	if len(quantities) == 0 {
		return nil
	}
	// The wedge pins requests == limits (Guaranteed QoS) so a step gets exactly
	// the contract's declared resources.
	return &resourceRequirements{Requests: quantities, Limits: quantities}
}

func stepTemplateName(nodeID string) string {
	return stepTemplatePrefix + nodeID
}

// planIndex is the plan's cross-reference tables plus the derived step topology,
// built once per compile.
type planIndex struct {
	nodesByID         map[string]*planpb.GraphNode
	symbolsByRef      map[string]*planpb.SymbolEntry
	packagesByID      map[string]*planpb.SourcePackageRef
	contractsByRef    map[string]*planpb.ExecutionContract
	environmentsByRef map[string]*planpb.MaterializedEnvironment
	// stepOrder is the step node ids in plan (schedule) order; stepDependencies
	// maps each step to its upstream step ids (sentinels excluded), sorted.
	stepOrder        []string
	stepDependencies map[string][]string
	// endUpstreamSteps are the step ids with an edge into the end node, sorted.
	// The finalize task depends on them.
	endUpstreamSteps []string
}

func buildPlanIndex(plan *planpb.WorkflowPlan) planIndex {
	graph := plan.GetGraph()
	index := planIndex{
		nodesByID:         make(map[string]*planpb.GraphNode),
		symbolsByRef:      make(map[string]*planpb.SymbolEntry),
		packagesByID:      make(map[string]*planpb.SourcePackageRef),
		contractsByRef:    make(map[string]*planpb.ExecutionContract),
		environmentsByRef: make(map[string]*planpb.MaterializedEnvironment),
		stepDependencies:  make(map[string][]string),
	}

	isStep := make(map[string]bool)
	for _, node := range graph.GetNodes() {
		index.nodesByID[node.GetId()] = node
		if node.GetKind() == spec.NodeKindStep {
			isStep[node.GetId()] = true
			index.stepOrder = append(index.stepOrder, node.GetId())
		}
	}
	for _, symbol := range plan.GetSymbols() {
		index.symbolsByRef[symbol.GetSymbolRef()] = symbol
	}
	for _, sourcePackage := range plan.GetSourcePackages() {
		index.packagesByID[sourcePackage.GetPackageId()] = sourcePackage
	}
	for _, contract := range plan.GetContracts() {
		index.contractsByRef[contract.GetContractRef()] = contract
	}
	for _, environment := range plan.GetEnvironments() {
		index.environmentsByRef[environment.GetEnvRef()] = environment
	}

	// Step-to-step dependencies come straight from the plan edges; edges touching
	// the __start/__end sentinels (non-step nodes) are not DAG dependencies.
	for _, stepID := range index.stepOrder {
		index.stepDependencies[stepID] = []string{}
	}
	endNode := graph.GetEndNode()
	for _, edge := range graph.GetEdges() {
		if isStep[edge.GetFrom()] && isStep[edge.GetTo()] {
			index.stepDependencies[edge.GetTo()] = append(index.stepDependencies[edge.GetTo()], edge.GetFrom())
		}
		if isStep[edge.GetFrom()] && edge.GetTo() == endNode {
			index.endUpstreamSteps = append(index.endUpstreamSteps, edge.GetFrom())
		}
	}
	for stepID := range index.stepDependencies {
		deps := index.stepDependencies[stepID]
		sortStrings(deps)
	}
	sortStrings(index.endUpstreamSteps)

	return index
}
