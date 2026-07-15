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
	ServiceAccountName string     `json:"serviceAccountName,omitempty"`
	Templates          []template `json:"templates"`
}

type template struct {
	Name      string       `json:"name"`
	Inputs    *inputs      `json:"inputs,omitempty"`
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

type inputs struct {
	Artifacts []inputArtifact `json:"artifacts,omitempty"`
}

type inputArtifact struct {
	Name string      `json:"name"`
	Path string      `json:"path"`
	Raw  rawArtifact `json:"raw"`
}

type rawArtifact struct {
	Data string `json:"data"`
}

type container struct {
	Image     string                `json:"image"`
	Command   []string              `json:"command,omitempty"`
	Args      []string              `json:"args,omitempty"`
	Resources *resourceRequirements `json:"resources,omitempty"`
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
	descriptorArtifactName = "descriptor"
	stepRunnerCommand      = "massive-step-runner"

	// PlanHashAnnotation carries the compiled plan hash on the generated template.
	// The plan-provenance invariant asserts it exists and matches the plan.
	PlanHashAnnotation = "massive.dev/plan-hash"
)

// buildWorkflowTemplate materializes the Argo WorkflowTemplate tree from the
// plan. The DAG mirrors the plan topology exactly: one task per step node with
// dependencies equal to the plan's step-to-step edges (the __start/__end
// sentinels are skipped). Each step template runs the step runner in the
// container-env image against a descriptor delivered as a raw input artifact.
func buildWorkflowTemplate(index planIndex, input compileContext) (*workflowTemplate, error) {
	graph := input.plan.GetGraph()

	name := input.target.WorkflowTemplateName
	if name == "" {
		name = graph.GetWorkflowName()
	}
	if name == "" {
		return nil, fmt.Errorf("plan has no workflow name and target set no workflowTemplateName")
	}
	if err := validateNames(name, index); err != nil {
		return nil, err
	}

	dag := &dagTemplate{Tasks: make([]dagTask, 0, len(index.stepOrder))}
	templates := make([]template, 0, len(index.stepOrder)+1)

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

	allTemplates := append([]template{{Name: entrypointTemplateName, DAG: dag}}, templates...)

	return &workflowTemplate{
		APIVersion: apiVersion,
		Kind:       workflowTemplateKind,
		Metadata: objectMeta{
			Name:        name,
			Namespace:   input.target.Namespace,
			Annotations: map[string]string{PlanHashAnnotation: input.plan.GetPlanHash()},
		},
		Spec: workflowTemplateSpec{
			Entrypoint:         entrypointTemplateName,
			ServiceAccountName: input.target.ServiceAccountName,
			Templates:          allTemplates,
		},
	}, nil
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

	descriptor, err := buildStepDescriptor(index, planHash, node)
	if err != nil {
		return template{}, err
	}
	descriptorJSON, err := canonicalDescriptorJSON(descriptor)
	if err != nil {
		return template{}, err
	}

	return template{
		Name: stepTemplateName(node.GetId()),
		Inputs: &inputs{
			Artifacts: []inputArtifact{{
				Name: descriptorArtifactName,
				Path: descriptorMountPath,
				Raw:  rawArtifact{Data: descriptorJSON},
			}},
		},
		Container: &container{
			Image:     environment.GetContainer().GetImage(),
			Command:   []string{stepRunnerCommand},
			Args:      []string{descriptorMountPath},
			Resources: containerResources(contract.GetResources()),
		},
	}, nil
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
	for _, edge := range graph.GetEdges() {
		if isStep[edge.GetFrom()] && isStep[edge.GetTo()] {
			index.stepDependencies[edge.GetTo()] = append(index.stepDependencies[edge.GetTo()], edge.GetFrom())
		}
	}
	for stepID := range index.stepDependencies {
		deps := index.stepDependencies[stepID]
		sortStrings(deps)
	}

	return index
}
