package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/irversion"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var immutableContainerImage = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

const (
	NodeKindStart    = "start"
	NodeKindStep     = "step"
	NodeKindDecision = "decision"
	NodeKindSelect   = "select"
	NodeKindMap      = "map"
	NodeKindEnd      = "end"
)

type WorkflowSpec struct {
	Kind           string                       `json:"kind"`
	SchemaVersion  uint32                       `json:"schemaVersion"`
	Encoding       string                       `json:"encoding"`
	SpecHash       string                       `json:"specHash"`
	Workflow       Workflow                     `json:"workflow"`
	Graph          Graph                        `json:"graph"`
	Schemas        map[string]json.RawMessage   `json:"schemas"`
	Symbols        map[string]Symbol            `json:"symbols"`
	SourcePackages map[string]SourcePackage     `json:"sourcePackages"`
	Environments   map[string]Environment       `json:"environments"`
	Contracts      map[string]ExecutionContract `json:"contracts"`
}

type Workflow struct {
	Name         string `json:"name"`
	InputSchema  string `json:"inputSchema"`
	OutputSchema string `json:"outputSchema"`
}

type Graph struct {
	IRVersion string      `json:"irVersion"`
	Start     string      `json:"start"`
	End       string      `json:"end"`
	Nodes     []GraphNode `json:"nodes"`
	Edges     []GraphEdge `json:"edges"`
}

type GraphNode struct {
	ID               string         `json:"id"`
	Kind             string         `json:"kind"`
	InputSchema      string         `json:"inputSchema,omitempty"`
	OutputSchema     string         `json:"outputSchema,omitempty"`
	SymbolRef        string         `json:"symbolRef,omitempty"`
	ContractRef      string         `json:"contractRef,omitempty"`
	MergeInputs      []string       `json:"mergeInputs,omitempty"`
	Selector         string         `json:"selector,omitempty"`
	Cases            []DecisionCase `json:"cases,omitempty"`
	DecisionRef      string         `json:"decisionRef,omitempty"`
	SelectInputs     []SelectInput  `json:"selectInputs,omitempty"`
	ItemInputSchema  string         `json:"itemInputSchema,omitempty"`
	ItemOutputSchema string         `json:"itemOutputSchema,omitempty"`
	MaxConcurrency   uint32         `json:"maxConcurrency,omitempty"`
}

type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Case string `json:"case,omitempty"`
}

// DecisionCase is a data-only route declaration. Tags are deliberately strings
// so every frontend can serialize and compare them identically.
type DecisionCase struct {
	Tag    string `json:"tag"`
	Schema string `json:"schema"`
}

// SelectInput states which branch output supplies one exhaustive case.
type SelectInput struct {
	Case   string `json:"case"`
	Source string `json:"source"`
}

type Symbol struct {
	PackageID string `json:"packageId"`
	Language  string `json:"language"`
	Module    string `json:"module"`
	Export    string `json:"export"`
}

type SourcePackage struct {
	PackageID   string              `json:"packageId"`
	Language    string              `json:"language"`
	PackageHash string              `json:"packageHash"`
	Files       []SourcePackageFile `json:"files"`
}

type SourcePackageFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

type Environment struct {
	Kind             string   `json:"kind"`
	Image            string   `json:"image,omitempty"`
	Platform         string   `json:"platform,omitempty"`
	Command          []string `json:"command,omitempty"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	Version          string   `json:"version,omitempty"`
	PackageManager   string   `json:"packageManager,omitempty"`
	Lockfile         string   `json:"lockfile,omitempty"`
}

type ExecutionContract struct {
	EnvironmentRef string                `json:"environmentRef"`
	Resources      *ResourceRequirements `json:"resources,omitempty"`
	Secrets        []SecretRef           `json:"secrets,omitempty"`
	Network        *NetworkPolicy        `json:"network,omitempty"`
}

type ResourceRequirements struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type SecretRef struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

type NetworkPolicy struct {
	Egress string   `json:"egress"`
	Hosts  []string `json:"hosts,omitempty"`
}

type Diagnostic struct {
	Path    string
	Ref     string
	Message string
}

type DiagnosticsError struct {
	Diagnostics []Diagnostic
}

func (e *DiagnosticsError) Error() string {
	if len(e.Diagnostics) == 0 {
		return "workflow spec diagnostics"
	}
	return e.Diagnostics[0].String()
}

func (d Diagnostic) String() string {
	if d.Ref == "" {
		return fmt.Sprintf("%s: %s", d.Path, d.Message)
	}
	return fmt.Sprintf("%s (%s): %s", d.Path, d.Ref, d.Message)
}

func ReadFile(path string) (*WorkflowSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow spec %q: %w", path, err)
	}

	parsed, err := Parse(data)
	if err != nil {
		return nil, err
	}

	return parsed, nil
}

func Parse(data []byte) (*WorkflowSpec, error) {
	if err := validateSchema(data); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var parsed WorkflowSpec
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode workflow spec: %w", err)
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return nil, fmt.Errorf("decode workflow spec: trailing JSON content")
	}

	diagnostics := validateSemantics(&parsed)
	if len(diagnostics) > 0 {
		return nil, &DiagnosticsError{Diagnostics: diagnostics}
	}

	return &parsed, nil
}

func RecomputedSpecHash(data []byte) (string, error) {
	hash, err := canonical.DigestJSONWithRootMemberExcluded(data, "specHash")
	if err != nil {
		return "", fmt.Errorf("compute spec hash: %w", err)
	}
	return hash, nil
}

func validateSchema(data []byte) error {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode workflow spec for schema validation: %w", err)
	}

	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.WorkflowSpecSchemaJSON))
	if err != nil {
		return fmt.Errorf("decode embedded workflow spec schema: %w", err)
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("workflow-spec.schema.json", schemaDocument); err != nil {
		return fmt.Errorf("register workflow spec schema: %w", err)
	}
	schema, err := compiler.Compile("workflow-spec.schema.json")
	if err != nil {
		return fmt.Errorf("compile workflow spec schema: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			if diagnostics := missingRequiredContractRefDiagnostics(data); len(diagnostics) > 0 {
				return &DiagnosticsError{Diagnostics: diagnostics}
			}
			return &DiagnosticsError{Diagnostics: schemaDiagnostics(validation)}
		}
		return fmt.Errorf("validate workflow spec schema: %w", err)
	}

	return nil
}

func missingRequiredContractRefDiagnostics(data []byte) []Diagnostic {
	var raw struct {
		Graph struct {
			Nodes []json.RawMessage `json:"nodes"`
		} `json:"graph"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	var diagnostics []Diagnostic
	for index, nodeData := range raw.Graph.Nodes {
		var node struct {
			Kind        string           `json:"kind"`
			ContractRef *json.RawMessage `json:"contractRef"`
		}
		if err := json.Unmarshal(nodeData, &node); err != nil {
			continue
		}
		if node.Kind != NodeKindStep {
			continue
		}
		if node.ContractRef != nil {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Path:    fmt.Sprintf("$.graph.nodes[%d].contractRef", index),
			Message: "step node requires contractRef",
		})
	}

	return diagnostics
}

func schemaDiagnostics(validation *jsonschema.ValidationError) []Diagnostic {
	basic := validation.BasicOutput()
	var diagnostics []Diagnostic
	collectSchemaDiagnostics(basic, &diagnostics)
	if len(diagnostics) == 0 {
		return []Diagnostic{{Path: "$", Ref: "workflow-spec.schema.json", Message: validation.Error()}}
	}
	return diagnostics
}

func collectSchemaDiagnostics(unit *jsonschema.OutputUnit, diagnostics *[]Diagnostic) {
	if unit == nil {
		return
	}
	if unit.Error != nil {
		path := unit.InstanceLocation
		if path == "" {
			path = "$"
		}
		*diagnostics = append(*diagnostics, Diagnostic{
			Path:    path,
			Ref:     unit.KeywordLocation,
			Message: unit.Error.String(),
		})
	}
	for index := range unit.Errors {
		collectSchemaDiagnostics(&unit.Errors[index], diagnostics)
	}
}

func validateSemantics(parsed *WorkflowSpec) []Diagnostic {
	var diagnostics []Diagnostic
	version, err := irversion.Parse(parsed.Graph.IRVersion)
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.irVersion", Ref: parsed.Graph.IRVersion, Message: err.Error()})
	} else if !irversion.CompilerSupports(version) {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.irVersion", Ref: parsed.Graph.IRVersion, Message: fmt.Sprintf("unsupported graph IR version; compiler supports %s", irversion.CompilerRange)})
	}

	nodeByID := make(map[string]GraphNode, len(parsed.Graph.Nodes))
	nodeIndexes := make(map[string]int, len(parsed.Graph.Nodes))
	startCount := 0
	endCount := 0
	for index, node := range parsed.Graph.Nodes {
		path := fmt.Sprintf("$.graph.nodes[%d]", index)
		if _, exists := nodeByID[node.ID]; exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".id", Ref: node.ID, Message: "duplicate graph node id"})
			continue
		}
		nodeByID[node.ID] = node
		nodeIndexes[node.ID] = index
		if node.Kind == NodeKindStart {
			startCount++
		}
		if node.Kind == NodeKindEnd {
			endCount++
		}
	}

	if startCount != 1 {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.nodes", Message: fmt.Sprintf("expected exactly one start node, found %d", startCount)})
	}
	if endCount != 1 {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.nodes", Message: fmt.Sprintf("expected exactly one end node, found %d", endCount)})
	}
	if node, exists := nodeByID[parsed.Graph.Start]; !exists || node.Kind != NodeKindStart {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.start", Ref: parsed.Graph.Start, Message: "start must reference the start node"})
	}
	if node, exists := nodeByID[parsed.Graph.End]; !exists || node.Kind != NodeKindEnd {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.end", Ref: parsed.Graph.End, Message: "end must reference the end node"})
	}

	upstream := make(map[string]map[string]bool, len(parsed.Graph.Nodes))
	inbound := make(map[string][]string, len(parsed.Graph.Nodes))
	outbound := make(map[string][]string, len(parsed.Graph.Nodes))
	adjacency := make(map[string][]string, len(parsed.Graph.Nodes))
	edgeIndexes := make(map[string]int, len(parsed.Graph.Edges))
	for index, edge := range parsed.Graph.Edges {
		path := fmt.Sprintf("$.graph.edges[%d]", index)
		if _, exists := nodeByID[edge.From]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".from", Ref: edge.From, Message: "edge source node does not exist"})
		}
		if _, exists := nodeByID[edge.To]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".to", Ref: edge.To, Message: "edge target node does not exist"})
		}
		if _, exists := nodeByID[edge.From]; !exists {
			continue
		}
		if _, exists := nodeByID[edge.To]; !exists {
			continue
		}
		edgeKey := edge.From + "\x00" + edge.To + "\x00" + edge.Case
		if firstIndex, exists := edgeIndexes[edgeKey]; exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path, Ref: edge.From + " -> " + edge.To, Message: fmt.Sprintf("duplicate graph edge; first declared at $.graph.edges[%d]", firstIndex)})
			continue
		}
		edgeIndexes[edgeKey] = index
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		outbound[edge.From] = append(outbound[edge.From], edge.To)
		inbound[edge.To] = append(inbound[edge.To], edge.From)
		if upstream[edge.To] == nil {
			upstream[edge.To] = make(map[string]bool)
		}
		upstream[edge.To][edge.From] = true
	}
	for nodeID := range adjacency {
		sort.Slice(adjacency[nodeID], func(i, j int) bool { return canonical.LessUTF16(adjacency[nodeID][i], adjacency[nodeID][j]) })
	}
	if len(inbound[parsed.Graph.Start]) != 0 {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.edges", Ref: parsed.Graph.Start, Message: "start node must not have inbound edges"})
	}
	if len(outbound[parsed.Graph.Start]) != 1 {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.start", Ref: parsed.Graph.Start, Message: fmt.Sprintf("start node must have exactly one outbound edge, found %d", len(outbound[parsed.Graph.Start]))})
	}
	if len(outbound[parsed.Graph.End]) != 0 {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.edges", Ref: parsed.Graph.End, Message: "end node must not have outbound edges"})
	}
	if len(inbound[parsed.Graph.End]) != 1 {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.end", Ref: parsed.Graph.End, Message: fmt.Sprintf("end node must have exactly one inbound edge, found %d", len(inbound[parsed.Graph.End]))})
	}

	if len(diagnostics) == 0 {
		if cycle := findCycle(parsed.Graph.Nodes, adjacency); len(cycle) > 0 {
			diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.edges", Ref: strings.Join(cycle, " -> "), Message: "graph contains a directed cycle"})
		}
	}
	if len(diagnostics) == 0 {
		diagnostics = append(diagnostics, unreachableDiagnostics(parsed, adjacency, nodeByID)...)
	}

	for index, node := range parsed.Graph.Nodes {
		if node.Kind != NodeKindStep {
			continue
		}
		path := fmt.Sprintf("$.graph.nodes[%d]", index)
		if _, exists := parsed.Schemas[node.InputSchema]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: node.InputSchema, Message: "input schema reference does not exist"})
		}
		if _, exists := parsed.Schemas[node.OutputSchema]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".outputSchema", Ref: node.OutputSchema, Message: "output schema reference does not exist"})
		}
		if _, exists := parsed.Symbols[node.SymbolRef]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".symbolRef", Ref: node.SymbolRef, Message: "symbol reference does not exist"})
		}
		if _, exists := parsed.Contracts[node.ContractRef]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".contractRef", Ref: node.ContractRef, Message: "contract reference does not exist"})
		}
		for mergeIndex, sourceID := range node.MergeInputs {
			if source, exists := nodeByID[sourceID]; !exists || source.Kind != NodeKindStep {
				diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("%s.mergeInputs[%d]", path, mergeIndex), Ref: sourceID, Message: "merge input step does not exist"})
				continue
			}
			if !upstream[node.ID][sourceID] {
				diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("%s.mergeInputs[%d]", path, mergeIndex), Ref: sourceID, Message: "merge input is not an upstream step"})
			}
		}
		if len(node.MergeInputs) == 0 && len(inbound[node.ID]) > 1 {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".mergeInputs", Ref: node.ID, Message: "step with multiple inbound edges must declare mergeInputs"})
		}
		if len(node.MergeInputs) > 0 {
			if len(node.MergeInputs) != len(inbound[node.ID]) {
				diagnostics = append(diagnostics, Diagnostic{Path: path + ".mergeInputs", Ref: node.ID, Message: "mergeInputs must exactly match inbound edges"})
				continue
			}
			declared := make(map[string]bool, len(node.MergeInputs))
			for _, sourceID := range node.MergeInputs {
				if declared[sourceID] {
					diagnostics = append(diagnostics, Diagnostic{Path: path + ".mergeInputs", Ref: sourceID, Message: "mergeInputs must not contain duplicate source ids"})
					continue
				}
				declared[sourceID] = true
			}
			for _, sourceID := range inbound[node.ID] {
				if !declared[sourceID] {
					diagnostics = append(diagnostics, Diagnostic{Path: path + ".mergeInputs", Ref: sourceID, Message: "mergeInputs must exactly match inbound edges"})
				}
			}
		}
	}

	diagnostics = append(diagnostics, validateMapSemantics(parsed, nodeByID, inbound, outbound)...)
	diagnostics = append(diagnostics, validateDecisionAndSelectSemantics(parsed, nodeByID, nodeIndexes)...)

	if _, exists := parsed.Schemas[parsed.Workflow.InputSchema]; !exists {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.workflow.inputSchema", Ref: parsed.Workflow.InputSchema, Message: "workflow input schema reference does not exist"})
	}
	if _, exists := parsed.Schemas[parsed.Workflow.OutputSchema]; !exists {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.workflow.outputSchema", Ref: parsed.Workflow.OutputSchema, Message: "workflow output schema reference does not exist"})
	}

	for contractRef, contract := range parsed.Contracts {
		if _, exists := parsed.Environments[contract.EnvironmentRef]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: "$.contracts." + contractRef + ".environmentRef", Ref: contract.EnvironmentRef, Message: "contract environment reference does not exist"})
		}
	}
	for environmentRef, environment := range parsed.Environments {
		diagnostics = append(diagnostics, validateContainerRecipe(environmentRef, environment)...)
	}
	packageRefs := make([]string, 0, len(parsed.SourcePackages))
	for packageRef := range parsed.SourcePackages {
		packageRefs = append(packageRefs, packageRef)
	}
	sort.Slice(packageRefs, func(i, j int) bool { return canonical.LessUTF16(packageRefs[i], packageRefs[j]) })
	for _, packageRef := range packageRefs {
		sourcePackage := parsed.SourcePackages[packageRef]
		if sourcePackage.PackageID != packageRef {
			diagnostics = append(diagnostics, Diagnostic{Path: "$.sourcePackages." + packageRef + ".packageId", Ref: sourcePackage.PackageID, Message: "source package id must match map key"})
		}
	}
	for symbolRef, symbol := range parsed.Symbols {
		if _, exists := parsed.SourcePackages[symbol.PackageID]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: "$.symbols." + symbolRef + ".packageId", Ref: symbol.PackageID, Message: "symbol package reference does not exist"})
		}
	}

	return diagnostics
}

func validateDecisionAndSelectSemantics(parsed *WorkflowSpec, nodeByID map[string]GraphNode, nodeIndexes map[string]int) []Diagnostic {
	var diagnostics []Diagnostic
	if parsed.Graph.IRVersion == "0.1" {
		for index, node := range parsed.Graph.Nodes {
			if node.Kind == NodeKindDecision || node.Kind == NodeKindSelect || node.Kind == NodeKindMap {
				diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.nodes[%d].kind", index), Ref: node.Kind, Message: "graph IR 0.1 permits only static start, step, and end nodes"})
			}
		}
		for index, edge := range parsed.Graph.Edges {
			if edge.Case != "" {
				diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.edges[%d].case", index), Ref: edge.Case, Message: "graph IR 0.1 does not permit conditional edges"})
			}
		}
	}

	decisionCases := make(map[string]map[string]bool)
	decisionCaseSchemas := make(map[string]map[string]string)
	for index, node := range parsed.Graph.Nodes {
		if node.Kind != NodeKindDecision {
			continue
		}
		path := fmt.Sprintf("$.graph.nodes[%d]", index)
		if _, exists := parsed.Schemas[node.InputSchema]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: node.InputSchema, Message: "decision input schema reference does not exist"})
		}
		tags := make(map[string]bool, len(node.Cases))
		caseSchemas := make(map[string]string, len(node.Cases))
		for caseIndex, decisionCase := range node.Cases {
			casePath := fmt.Sprintf("%s.cases[%d]", path, caseIndex)
			if tags[decisionCase.Tag] {
				diagnostics = append(diagnostics, Diagnostic{Path: casePath + ".tag", Ref: decisionCase.Tag, Message: "decision case tags must be unique"})
				continue
			}
			tags[decisionCase.Tag] = true
			caseSchemas[decisionCase.Tag] = decisionCase.Schema
			if _, exists := parsed.Schemas[decisionCase.Schema]; !exists {
				diagnostics = append(diagnostics, Diagnostic{Path: casePath + ".schema", Ref: decisionCase.Schema, Message: "decision case schema reference does not exist"})
			}
		}
		decisionCases[node.ID] = tags
		decisionCaseSchemas[node.ID] = caseSchemas

		inboundEdges := make([]GraphEdge, 0, 1)
		for _, edge := range parsed.Graph.Edges {
			if edge.To == node.ID {
				inboundEdges = append(inboundEdges, edge)
			}
		}
		if len(inboundEdges) != 1 {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: node.ID, Message: "decision requires exactly one inbound value producer"})
			continue
		}
		producer, exists := nodeByID[inboundEdges[0].From]
		producerSchema, valueProducer := outputSchemaOfValueProducer(producer)
		if !exists || !valueProducer || inboundEdges[0].Case != "" {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: inboundEdges[0].From, Message: "decision requires exactly one inbound value producer (step or select)"})
			continue
		}
		if producerSchema != node.InputSchema {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: inboundEdges[0].From, Message: "decision input schema must equal its sole producer output schema"})
		}
	}

	conditionalCounts := make(map[string]map[string]int)
	branchTargets := make(map[string]map[string]string)
	conditionalTargets := make(map[string][]int)
	for edgeIndex, edge := range parsed.Graph.Edges {
		path := fmt.Sprintf("$.graph.edges[%d]", edgeIndex)
		source, sourceExists := nodeByID[edge.From]
		if edge.Case == "" {
			if sourceExists && source.Kind == NodeKindDecision {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Ref: edge.From + " -> " + edge.To, Message: "decision outputs require a conditional edge case"})
			}
			continue
		}
		if !sourceExists || source.Kind != NodeKindDecision {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".case", Ref: edge.Case, Message: "conditional edge source must be a decision node"})
			continue
		}
		if !decisionCases[edge.From][edge.Case] {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".case", Ref: edge.Case, Message: "conditional edge case is not declared by its decision"})
			continue
		}
		target, targetExists := nodeByID[edge.To]
		if targetExists && target.Kind != NodeKindStep && target.Kind != NodeKindMap {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".to", Ref: edge.To, Message: "conditional edge target must be a step node or map node"})
		}
		if targetExists && (target.Kind == NodeKindStep || target.Kind == NodeKindMap) && target.InputSchema != decisionCaseSchemas[edge.From][edge.Case] {
			targetIndex := nodeIndexes[edge.To]
			diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.nodes[%d].inputSchema", targetIndex), Ref: edge.To, Message: "conditional target input schema must equal decision case schema"})
		}
		conditionalTargets[edge.To] = append(conditionalTargets[edge.To], edgeIndex)
		if conditionalCounts[edge.From] == nil {
			conditionalCounts[edge.From] = make(map[string]int)
			branchTargets[edge.From] = make(map[string]string)
		}
		conditionalCounts[edge.From][edge.Case]++
		if branchTargets[edge.From][edge.Case] == "" {
			branchTargets[edge.From][edge.Case] = edge.To
		}
	}
	for _, target := range sortedKeys(conditionalTargets) {
		if len(conditionalTargets[target]) < 2 {
			continue
		}
		for _, edgeIndex := range conditionalTargets[target] {
			diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.edges[%d].to", edgeIndex), Ref: target, Message: "conditional branches may not share a target in graph IR 0.2"})
		}
	}
	for _, decisionID := range sortedKeys(decisionCases) {
		tags := decisionCases[decisionID]
		for _, tag := range sortedKeys(tags) {
			if conditionalCounts[decisionID][tag] != 1 {
				index := nodeIndexes[decisionID]
				diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.nodes[%d].cases", index), Ref: tag, Message: "each decision case requires exactly one conditional outgoing edge"})
			}
		}
	}

	for index, node := range parsed.Graph.Nodes {
		if node.Kind != NodeKindSelect {
			continue
		}
		path := fmt.Sprintf("$.graph.nodes[%d]", index)
		decision, exists := nodeByID[node.DecisionRef]
		if !exists || decision.Kind != NodeKindDecision {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".decisionRef", Ref: node.DecisionRef, Message: "select decisionRef must reference a decision node"})
		}
		if _, exists := parsed.Schemas[node.OutputSchema]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".outputSchema", Ref: node.OutputSchema, Message: "select output schema reference does not exist"})
		}

		tags := decisionCases[node.DecisionRef]
		selectedCases := make(map[string]bool, len(node.SelectInputs))
		selectedSources := make(map[string]bool, len(node.SelectInputs))
		declaredSources := make(map[string]bool, len(node.SelectInputs))
		for inputIndex, input := range node.SelectInputs {
			inputPath := fmt.Sprintf("%s.selectInputs[%d]", path, inputIndex)
			if selectedCases[input.Case] {
				diagnostics = append(diagnostics, Diagnostic{Path: inputPath + ".case", Ref: input.Case, Message: "select input cases must be unique"})
			}
			selectedCases[input.Case] = true
			if exists && !tags[input.Case] {
				diagnostics = append(diagnostics, Diagnostic{Path: inputPath + ".case", Ref: input.Case, Message: "select input case is not declared by its decision"})
			}
			if selectedSources[input.Source] {
				diagnostics = append(diagnostics, Diagnostic{Path: inputPath + ".source", Ref: input.Source, Message: "select input sources must be unique"})
			}
			selectedSources[input.Source] = true
			source, sourceExists := nodeByID[input.Source]
			if !sourceExists || (source.Kind != NodeKindStep && source.Kind != NodeKindSelect && source.Kind != NodeKindMap) {
				diagnostics = append(diagnostics, Diagnostic{Path: inputPath + ".source", Ref: input.Source, Message: "select input source must reference a value-producing step, select, or map node"})
			} else if source.OutputSchema != node.OutputSchema {
				diagnostics = append(diagnostics, Diagnostic{Path: inputPath + ".source", Ref: input.Source, Message: "select source output schema must equal select output schema"})
			}
			if branchTarget := branchTargets[node.DecisionRef][input.Case]; branchTarget != "" && !graphReachable(branchTarget, input.Source, parsed.Graph.Edges) {
				diagnostics = append(diagnostics, Diagnostic{Path: inputPath + ".source", Ref: input.Source, Message: "select input source must be reachable from its decision case branch"})
			}
			declaredSources[input.Source] = true
		}
		if exists {
			for _, tag := range sortedKeys(tags) {
				if !selectedCases[tag] {
					diagnostics = append(diagnostics, Diagnostic{Path: path + ".selectInputs", Ref: tag, Message: "select inputs must exactly cover decision case tags"})
				}
			}
			for _, tag := range sortedKeys(selectedCases) {
				if !tags[tag] {
					diagnostics = append(diagnostics, Diagnostic{Path: path + ".selectInputs", Ref: tag, Message: "select inputs must exactly cover decision case tags"})
				}
			}
		}
		actualSources := make(map[string]bool)
		for edgeIndex, edge := range parsed.Graph.Edges {
			if edge.To != node.ID {
				continue
			}
			if edge.Case != "" {
				diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.edges[%d].case", edgeIndex), Ref: edge.Case, Message: "select inputs require ordinary source edges"})
				continue
			}
			actualSources[edge.From] = true
		}
		for _, source := range sortedKeys(declaredSources) {
			if !actualSources[source] {
				diagnostics = append(diagnostics, Diagnostic{Path: path + ".selectInputs", Ref: source, Message: "select input sources must exactly match inbound edges"})
			}
		}
		for _, source := range sortedKeys(actualSources) {
			if !declaredSources[source] {
				diagnostics = append(diagnostics, Diagnostic{Path: path + ".selectInputs", Ref: source, Message: "select input sources must exactly match inbound edges"})
			}
		}
	}

	diagnostics = append(diagnostics, validateExclusiveDecisionBranches(parsed, decisionCases, branchTargets, nodeIndexes)...)
	diagnostics = append(diagnostics, validateDecisionActivationLineage(parsed, nodeByID, nodeIndexes)...)

	return diagnostics
}

func outputSchemaOfValueProducer(node GraphNode) (string, bool) {
	if node.Kind != NodeKindStep && node.Kind != NodeKindSelect && node.Kind != NodeKindMap {
		return "", false
	}
	return node.OutputSchema, true
}

func validateMapSemantics(parsed *WorkflowSpec, nodeByID map[string]GraphNode, inbound, outbound map[string][]string) []Diagnostic {
	var diagnostics []Diagnostic
	for index, node := range parsed.Graph.Nodes {
		if node.Kind != NodeKindMap {
			continue
		}
		path := fmt.Sprintf("$.graph.nodes[%d]", index)
		for field, schemaRef := range map[string]string{
			"inputSchema": node.InputSchema, "itemInputSchema": node.ItemInputSchema,
			"itemOutputSchema": node.ItemOutputSchema, "outputSchema": node.OutputSchema,
		} {
			if _, exists := parsed.Schemas[schemaRef]; !exists {
				diagnostics = append(diagnostics, Diagnostic{Path: path + "." + field, Ref: schemaRef, Message: "map schema reference does not exist"})
			}
		}
		if _, exists := parsed.Symbols[node.SymbolRef]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".symbolRef", Ref: node.SymbolRef, Message: "symbol reference does not exist"})
		}
		if _, exists := parsed.Contracts[node.ContractRef]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".contractRef", Ref: node.ContractRef, Message: "contract reference does not exist"})
		}
		if node.MaxConcurrency == 0 {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".maxConcurrency", Ref: node.ID, Message: "map maxConcurrency must be positive"})
		}
		if !arraySchemaItemsExactlyMatch(parsed.Schemas[node.InputSchema], parsed.Schemas[node.ItemInputSchema]) {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: node.InputSchema, Message: "map inputSchema must be an array whose items exactly equal itemInputSchema"})
		}
		if !arraySchemaItemsExactlyMatch(parsed.Schemas[node.OutputSchema], parsed.Schemas[node.ItemOutputSchema]) {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".outputSchema", Ref: node.OutputSchema, Message: "map outputSchema must be an array whose items exactly equal itemOutputSchema"})
		}
		if len(inbound[node.ID]) != 1 {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: node.ID, Message: "map requires exactly one predecessor"})
		} else if source, exists := nodeByID[inbound[node.ID][0]]; !exists {
			continue
		} else if source.Kind == NodeKindDecision && conditionalEdgeCase(parsed.Graph.Edges, source.ID, node.ID) != "" {
			// Decision validation proves the selected case schema exactly matches
			// this map's inputSchema; maps may therefore live in a branch.
		} else if sourceSchema, producesValue := graphValueOutputSchema(source, parsed.Workflow.InputSchema); !producesValue {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: source.ID, Message: "map predecessor must produce a value"})
		} else if sourceSchema != node.InputSchema {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".inputSchema", Ref: source.ID, Message: "map inputSchema must exactly equal its predecessor output schema"})
		}
		for _, targetID := range outbound[node.ID] {
			target := nodeByID[targetID]
			if target.Kind == NodeKindSelect {
				continue // selectInputs validates each declared source contract.
			}
			targetSchema, consumesValue := graphValueInputSchema(target, parsed.Workflow.OutputSchema)
			if !consumesValue {
				diagnostics = append(diagnostics, Diagnostic{Path: path + ".outputSchema", Ref: targetID, Message: "map output must target a value consumer"})
				continue
			}
			if targetSchema != node.OutputSchema {
				diagnostics = append(diagnostics, Diagnostic{Path: path + ".outputSchema", Ref: targetID, Message: "map outputSchema must exactly equal each downstream input schema"})
			}
		}
	}
	return diagnostics
}

func conditionalEdgeCase(edges []GraphEdge, from, to string) string {
	for _, edge := range edges {
		if edge.From == from && edge.To == to {
			return edge.Case
		}
	}
	return ""
}

func graphValueOutputSchema(node GraphNode, workflowInputSchema string) (string, bool) {
	if node.Kind == NodeKindStart {
		return workflowInputSchema, true
	}
	return outputSchemaOfValueProducer(node)
}

func graphValueInputSchema(node GraphNode, workflowOutputSchema string) (string, bool) {
	if node.Kind == NodeKindEnd {
		return workflowOutputSchema, true
	}
	if node.Kind == NodeKindStep || node.Kind == NodeKindDecision || node.Kind == NodeKindMap {
		return node.InputSchema, true
	}
	return "", false
}

func arraySchemaItemsExactlyMatch(arraySchema, itemSchema json.RawMessage) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(arraySchema, &object); err != nil {
		return false
	}
	var kind string
	if err := json.Unmarshal(object["type"], &kind); err != nil || kind != "array" || len(object["items"]) == 0 {
		return false
	}
	items, ok := resolveLocalJSONReference(arraySchema, object["items"])
	if !ok {
		return false
	}
	canonicalItems, err := canonical.CanonicalizeJSON(items)
	if err != nil {
		return false
	}
	canonicalItemSchema, err := canonical.CanonicalizeJSON(itemSchema)
	if err != nil {
		return false
	}
	return bytes.Equal(canonicalItems, canonicalItemSchema)
}

// resolveLocalJSONReference follows reference-only local JSON Pointer values
// within one top-level array schema. It intentionally does not try to prove
// arbitrary JSON Schema equivalence: after dereferencing, callers still
// compare the canonical JSON fragments exactly.
func resolveLocalJSONReference(document, value json.RawMessage) (json.RawMessage, bool) {
	resolved := value
	seen := make(map[string]bool)
	for {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(resolved, &object); err != nil {
			return resolved, true
		}
		referenceValue, hasReference := object["$ref"]
		if !hasReference || len(object) != 1 {
			return resolved, true
		}
		var reference string
		if err := json.Unmarshal(referenceValue, &reference); err != nil || seen[reference] {
			return nil, false
		}
		seen[reference] = true
		target, ok := resolveLocalJSONPointer(document, reference)
		if !ok {
			return nil, false
		}
		resolved = target
	}
}

func resolveLocalJSONPointer(document json.RawMessage, reference string) (json.RawMessage, bool) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Path != "" || parsed.RawQuery != "" {
		return nil, false
	}
	current := document
	if parsed.Fragment == "" {
		return current, true
	}
	if !strings.HasPrefix(parsed.Fragment, "/") {
		return nil, false
	}
	for _, encodedToken := range strings.Split(parsed.Fragment[1:], "/") {
		token, ok := decodeJSONPointerToken(encodedToken)
		if !ok {
			return nil, false
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err == nil {
			next, exists := object[token]
			if !exists {
				return nil, false
			}
			current = next
			continue
		}
		var array []json.RawMessage
		if err := json.Unmarshal(current, &array); err != nil || (len(token) > 1 && token[0] == '0') {
			return nil, false
		}
		index, err := strconv.Atoi(token)
		if err != nil || index < 0 || index >= len(array) {
			return nil, false
		}
		current = array[index]
	}
	return current, true
}

func decodeJSONPointerToken(encoded string) (string, bool) {
	var decoded strings.Builder
	for index := 0; index < len(encoded); index++ {
		if encoded[index] != '~' {
			decoded.WriteByte(encoded[index])
			continue
		}
		if index+1 >= len(encoded) {
			return "", false
		}
		index++
		switch encoded[index] {
		case '0':
			decoded.WriteByte('~')
		case '1':
			decoded.WriteByte('/')
		default:
			return "", false
		}
	}
	return decoded.String(), true
}

// validateExclusiveDecisionBranches keeps 0.2's activation model local: a
// decision's cases may meet only at that decision's select. Without this
// restriction a node can be reachable through incompatible case paths, which
// would require a lineage lattice rather than the single durable selection the
// runtime records. Nested decisions remain valid because their complete region
// belongs to exactly one case of every enclosing decision.
func validateExclusiveDecisionBranches(parsed *WorkflowSpec, decisionCases map[string]map[string]bool, branchTargets map[string]map[string]string, nodeIndexes map[string]int) []Diagnostic {
	selectsByDecision := make(map[string][]string)
	for _, node := range parsed.Graph.Nodes {
		if node.Kind == NodeKindSelect {
			selectsByDecision[node.DecisionRef] = append(selectsByDecision[node.DecisionRef], node.ID)
		}
	}

	var diagnostics []Diagnostic
	for _, decisionID := range sortedKeys(decisionCases) {
		selects := selectsByDecision[decisionID]
		if len(selects) != 1 {
			index := nodeIndexes[decisionID]
			diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.nodes[%d]", index), Ref: decisionID, Message: "decision requires exactly one select node in graph IR 0.2"})
			continue
		}
		selectID := selects[0]
		owners := make(map[string][]string)
		for _, tag := range sortedKeys(decisionCases[decisionID]) {
			root := branchTargets[decisionID][tag]
			if root == "" {
				continue
			}
			for nodeID := range graphReachableBefore(root, selectID, parsed.Graph.Edges) {
				owners[nodeID] = append(owners[nodeID], tag)
			}
		}
		for _, nodeID := range sortedKeys(owners) {
			if len(owners[nodeID]) < 2 {
				continue
			}
			index := nodeIndexes[nodeID]
			diagnostics = append(diagnostics, Diagnostic{Path: fmt.Sprintf("$.graph.nodes[%d]", index), Ref: nodeID, Message: fmt.Sprintf("decision %q cases may converge only through select %q", decisionID, selectID)})
		}
	}
	return diagnostics
}

func graphReachableBefore(start string, stop string, edges []GraphEdge) map[string]bool {
	adjacency := make(map[string][]string)
	for _, edge := range edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}
	seen := make(map[string]bool)
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == stop || seen[current] {
			continue
		}
		seen[current] = true
		for _, next := range adjacency[current] {
			if !seen[next] {
				queue = append(queue, next)
			}
		}
	}
	return seen
}

// validateDecisionActivationLineage proves that every select source exists for
// the whole case it represents. A conditional edge adds a decision=case
// requirement. That requirement flows through ordinary data dependencies. A
// select removes its own decision requirement while retaining requirements
// imposed by enclosing decisions.
func validateDecisionActivationLineage(parsed *WorkflowSpec, nodeByID map[string]GraphNode, nodeIndexes map[string]int) []Diagnostic {
	order, complete := graphTopologicalOrder(parsed.Graph.Nodes, parsed.Graph.Edges)
	if !complete {
		return nil
	}

	inbound := make(map[string][]GraphEdge, len(parsed.Graph.Nodes))
	for _, edge := range parsed.Graph.Edges {
		inbound[edge.To] = append(inbound[edge.To], edge)
	}
	lineages := make(map[string]map[string]string, len(parsed.Graph.Nodes))
	var diagnostics []Diagnostic
	for _, nodeID := range order {
		node, exists := nodeByID[nodeID]
		if !exists {
			continue
		}
		if node.Kind == NodeKindSelect {
			enclosing, exists := lineages[node.DecisionRef]
			if !exists {
				continue
			}
			lineages[nodeID] = cloneActivationLineage(enclosing)
			for inputIndex, input := range node.SelectInputs {
				actual, exists := lineages[input.Source]
				if !exists {
					continue
				}
				expected := cloneActivationLineage(enclosing)
				expected[node.DecisionRef] = input.Case
				if activationLineagesEqual(actual, expected) {
					continue
				}
				path := fmt.Sprintf("$.graph.nodes[%d].selectInputs[%d].source", nodeIndexes[nodeID], inputIndex)
				diagnostics = append(diagnostics, Diagnostic{
					Path:    path,
					Ref:     input.Source,
					Message: activationLineageDiagnostic(actual, expected),
				})
			}
			continue
		}

		lineage := make(map[string]string)
		for _, edge := range inbound[nodeID] {
			for decisionID, tag := range lineages[edge.From] {
				if existing, conflict := lineage[decisionID]; conflict && existing != tag {
					continue
				}
				lineage[decisionID] = tag
			}
			if edge.Case != "" {
				lineage[edge.From] = edge.Case
			}
		}
		lineages[nodeID] = lineage
	}
	return diagnostics
}

func cloneActivationLineage(lineage map[string]string) map[string]string {
	cloned := make(map[string]string, len(lineage)+1)
	for decisionID, tag := range lineage {
		cloned[decisionID] = tag
	}
	return cloned
}

func activationLineagesEqual(actual map[string]string, expected map[string]string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for decisionID, tag := range expected {
		if actual[decisionID] != tag {
			return false
		}
	}
	return true
}

func activationLineageDiagnostic(actual map[string]string, expected map[string]string) string {
	var extras []string
	for _, decisionID := range sortedKeys(actual) {
		if expected[decisionID] == actual[decisionID] {
			continue
		}
		extras = append(extras, fmt.Sprintf("%s=%q", decisionID, actual[decisionID]))
	}
	if len(extras) > 0 {
		return "select input source has unresolved nested decision requirement " + strings.Join(extras, ", ") + "; select nested decision outputs before using them in an enclosing select"
	}
	return "select input source activation lineage must exactly match its decision case lineage"
}

func graphTopologicalOrder(nodes []GraphNode, edges []GraphEdge) ([]string, bool) {
	indegree := make(map[string]int, len(nodes))
	adjacency := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		indegree[node.ID] = 0
	}
	for _, edge := range edges {
		if _, exists := indegree[edge.From]; !exists {
			continue
		}
		if _, exists := indegree[edge.To]; !exists {
			continue
		}
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		indegree[edge.To]++
	}
	ready := make([]string, 0, len(nodes))
	for nodeID, degree := range indegree {
		if degree == 0 {
			ready = append(ready, nodeID)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return canonical.LessUTF16(ready[i], ready[j]) })
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		order = append(order, current)
		for _, next := range adjacency[current] {
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return canonical.LessUTF16(ready[i], ready[j]) })
	}
	return order, len(order) == len(nodes)
}

func graphReachable(start string, target string, edges []GraphEdge) bool {
	if start == target {
		return true
	}
	adjacency := make(map[string][]string)
	for _, edge := range edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if next == target {
				return true
			}
			if seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	return false
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return canonical.LessUTF16(keys[i], keys[j]) })
	return keys
}

func validateContainerRecipe(environmentRef string, environment Environment) []Diagnostic {
	if environment.Kind != "container" {
		return nil
	}
	path := "$.environments." + environmentRef
	var diagnostics []Diagnostic
	if environment.Platform == "" {
		diagnostics = append(diagnostics, Diagnostic{Path: path + ".platform", Message: "container recipe requires platform"})
	}
	if !immutableContainerImage.MatchString(environment.Image) {
		diagnostics = append(diagnostics, Diagnostic{Path: path + ".image", Ref: environment.Image, Message: "container recipe requires an immutable image digest reference"})
	}
	return diagnostics
}

func findCycle(nodes []GraphNode, adjacency map[string][]string) []string {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)

	orderedIDs := make([]string, 0, len(nodes))
	for _, node := range nodes {
		orderedIDs = append(orderedIDs, node.ID)
	}
	sort.Slice(orderedIDs, func(i, j int) bool { return canonical.LessUTF16(orderedIDs[i], orderedIDs[j]) })

	state := make(map[string]int, len(nodes))
	stack := make([]string, 0, len(nodes))
	stackIndex := make(map[string]int, len(nodes))
	var cycle []string

	var visit func(string) bool
	visit = func(nodeID string) bool {
		state[nodeID] = visiting
		stackIndex[nodeID] = len(stack)
		stack = append(stack, nodeID)

		for _, next := range adjacency[nodeID] {
			if state[next] == unvisited {
				if visit(next) {
					return true
				}
				continue
			}
			if state[next] != visiting {
				continue
			}
			start := stackIndex[next]
			cycle = append([]string{}, stack[start:]...)
			cycle = append(cycle, next)
			return true
		}

		stack = stack[:len(stack)-1]
		delete(stackIndex, nodeID)
		state[nodeID] = visited
		return false
	}

	for _, nodeID := range orderedIDs {
		if state[nodeID] != unvisited {
			continue
		}
		if visit(nodeID) {
			return cycle
		}
	}

	return nil
}

func unreachableDiagnostics(parsed *WorkflowSpec, adjacency map[string][]string, nodeByID map[string]GraphNode) []Diagnostic {
	if _, exists := nodeByID[parsed.Graph.Start]; !exists {
		return nil
	}

	reachable := map[string]bool{parsed.Graph.Start: true}
	queue := []string{parsed.Graph.Start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if reachable[next] {
				continue
			}
			reachable[next] = true
			queue = append(queue, next)
		}
	}

	var unreachable []string
	for _, node := range parsed.Graph.Nodes {
		if reachable[node.ID] {
			continue
		}
		unreachable = append(unreachable, node.ID)
	}
	sort.Slice(unreachable, func(i, j int) bool { return canonical.LessUTF16(unreachable[i], unreachable[j]) })

	diagnostics := make([]Diagnostic, 0, len(unreachable))
	for _, nodeID := range unreachable {
		diagnostics = append(diagnostics, Diagnostic{Path: "$.graph.nodes", Ref: nodeID, Message: "node is not reachable from start"})
	}
	return diagnostics
}
