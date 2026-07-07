package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	NodeKindStart = "start"
	NodeKindStep  = "step"
	NodeKindEnd   = "end"
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
	Targets        []Target                     `json:"targets"`
}

type Workflow struct {
	Name         string `json:"name"`
	InputSchema  string `json:"inputSchema"`
	OutputSchema string `json:"outputSchema"`
}

type Graph struct {
	Start string      `json:"start"`
	End   string      `json:"end"`
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type GraphNode struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	InputSchema  string   `json:"inputSchema,omitempty"`
	OutputSchema string   `json:"outputSchema,omitempty"`
	SymbolRef    string   `json:"symbolRef,omitempty"`
	ContractRef  string   `json:"contractRef,omitempty"`
	MergeInputs  []string `json:"mergeInputs,omitempty"`
}

type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
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
	Root        string              `json:"root"`
	Include     []string            `json:"include"`
	Files       []SourcePackageFile `json:"files"`
	Artifact    string              `json:"artifact,omitempty"`
}

type SourcePackageFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

type Environment struct {
	Kind             string   `json:"kind"`
	Image            string   `json:"image,omitempty"`
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

type Target struct {
	Kind                 string `json:"kind"`
	Namespace            string `json:"namespace,omitempty"`
	ServiceAccountName   string `json:"serviceAccountName,omitempty"`
	WorkflowTemplateName string `json:"workflowTemplateName,omitempty"`
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

	nodeByID := make(map[string]GraphNode, len(parsed.Graph.Nodes))
	startCount := 0
	endCount := 0
	for index, node := range parsed.Graph.Nodes {
		path := fmt.Sprintf("$.graph.nodes[%d]", index)
		if _, exists := nodeByID[node.ID]; exists {
			diagnostics = append(diagnostics, Diagnostic{Path: path + ".id", Ref: node.ID, Message: "duplicate graph node id"})
			continue
		}
		nodeByID[node.ID] = node
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
	adjacency := make(map[string][]string, len(parsed.Graph.Nodes))
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
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		if upstream[edge.To] == nil {
			upstream[edge.To] = make(map[string]bool)
		}
		upstream[edge.To][edge.From] = true
	}
	for nodeID := range adjacency {
		sort.Slice(adjacency[nodeID], func(i, j int) bool { return canonical.LessUTF16(adjacency[nodeID][i], adjacency[nodeID][j]) })
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
	}

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
	for symbolRef, symbol := range parsed.Symbols {
		if _, exists := parsed.SourcePackages[symbol.PackageID]; !exists {
			diagnostics = append(diagnostics, Diagnostic{Path: "$.symbols." + symbolRef + ".packageId", Ref: symbol.PackageID, Message: "symbol package reference does not exist"})
		}
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
