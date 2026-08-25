package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/artifact"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type nodeOutput struct {
	Artifact  manifestDataArtifact
	Published manifestPublishedArtifact
	Body      []byte
}

// executionResolver owns the mutable graph-resolution state for a run. Keeping
// outputs, branch selections, and inactive regions together prevents callers
// from threading several related maps through every resolution operation.
type executionResolver struct {
	index         executionIndex
	outputs       map[string]nodeOutput
	selectedCases map[string]string
	inactive      map[string]manifestSkipReason
}

func newExecutionResolver(index executionIndex, graph *planpb.GraphIR, input []byte) *executionResolver {
	return &executionResolver{
		index: index,
		outputs: map[string]nodeOutput{
			graph.GetStartNode(): {
				Artifact: manifestDataArtifact{
					Hash:        canonical.DigestBytes(input),
					ContentType: jsonContentType,
					Schema:      graph.GetInputSchema(),
				},
				Body: input,
			},
		},
		selectedCases: make(map[string]string),
		inactive:      make(map[string]manifestSkipReason),
	}
}

func (r *executionResolver) setOutput(nodeID string, output nodeOutput) {
	r.outputs[nodeID] = output
}

func (r *executionResolver) markInactive(nodeID string, reason manifestSkipReason) {
	r.inactive[nodeID] = reason
}

func (r *executionResolver) inputForNode(node *planpb.GraphNode) ([]byte, error) {
	inbound := r.index.inboundByTarget[node.GetId()]
	if len(node.GetMergeInputs()) == 0 {
		if len(inbound) != 1 {
			return nil, fmt.Errorf("local runner v0 requires exactly one input edge for %q", node.GetId())
		}
		output, ok := r.outputs[inbound[0].GetFrom()]
		if !ok {
			return nil, fmt.Errorf("missing output from %q for %q", inbound[0].GetFrom(), node.GetId())
		}
		return output.Body, nil
	}

	inboundSources := make(map[string]bool, len(inbound))
	for _, edge := range inbound {
		inboundSources[edge.GetFrom()] = true
	}
	for _, source := range node.GetMergeInputs() {
		if !inboundSources[source] {
			return nil, fmt.Errorf("merge step %q is missing edge from %q", node.GetId(), source)
		}
	}
	if len(inbound) != len(node.GetMergeInputs()) {
		return nil, fmt.Errorf("merge step %q has edges that are not declared merge inputs", node.GetId())
	}

	values := make([]json.RawMessage, 0, len(node.GetMergeInputs()))
	for _, source := range node.GetMergeInputs() {
		output, ok := r.outputs[source]
		if !ok {
			return nil, fmt.Errorf("missing output from %q for %q", source, node.GetId())
		}
		values = append(values, output.Body)
	}
	encoded, err := canonical.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode merge input for %q: %w", node.GetId(), err)
	}
	return encoded, nil
}

// routeDecision is deliberately data-only: it reads the already-published
// classifier value, validates the selected branch schema, and forwards the
// same immutable artifact to the conditional edge. It never invokes user code.
func (r *executionResolver) routeDecision(node *planpb.GraphNode) (string, error) {
	inbound := r.index.inboundByTarget[node.GetId()]
	if len(inbound) != 1 {
		return "", fmt.Errorf("decision %q requires exactly one input edge", node.GetId())
	}
	inputBytes, err := r.inputForNode(node)
	if err != nil {
		return "", fmt.Errorf("decision %q input: %w", node.GetId(), err)
	}
	input, ok := r.outputs[inbound[0].GetFrom()]
	if !ok {
		return "", fmt.Errorf("decision %q input source %q has no output", node.GetId(), inbound[0].GetFrom())
	}
	if input.Artifact.Schema != node.GetInputSchema() {
		return "", fmt.Errorf("decision %q input schema %q does not match declared schema %q", node.GetId(), input.Artifact.Schema, node.GetInputSchema())
	}

	var value map[string]json.RawMessage
	if err := json.Unmarshal(inputBytes, &value); err != nil {
		return "", fmt.Errorf("decision %q selector %q requires a JSON object: %w", node.GetId(), node.GetSelector(), err)
	}
	rawTag, exists := value[node.GetSelector()]
	if !exists {
		return "", fmt.Errorf("decision %q selector %q is missing", node.GetId(), node.GetSelector())
	}
	var tag string
	if err := json.Unmarshal(rawTag, &tag); err != nil {
		return "", fmt.Errorf("decision %q selector %q must be a string", node.GetId(), node.GetSelector())
	}
	for _, decisionCase := range node.GetCases() {
		if decisionCase.GetTag() != tag {
			continue
		}
		schema := r.index.schemaJSON[decisionCase.GetSchema()]
		if schema == "" {
			return "", fmt.Errorf("decision %q case %q references unavailable schema %q", node.GetId(), tag, decisionCase.GetSchema())
		}
		if err := validateJSONAgainstSchema(schema, inputBytes); err != nil {
			// Schema-library errors can include a rendered instance. The manifest is
			// durable user-visible control-plane state, so retain the actionable
			// route identity without copying classified data into its diagnostic.
			return "", fmt.Errorf("decision %q selected case %q does not satisfy its schema", node.GetId(), tag)
		}
		r.selectedCases[node.GetId()] = tag
		r.outputs[node.GetId()] = input
		return tag, nil
	}
	return "", fmt.Errorf("decision %q selector %q selected an undeclared case", node.GetId(), node.GetSelector())
}

// selectOutput aliases the chosen branch's DataArtifactRef and body. It must
// not republish the data: the source attempt already committed it manifest-last.
func (r *executionResolver) selectOutput(node *planpb.GraphNode) error {
	tag, exists := r.selectedCases[node.GetDecisionRef()]
	if !exists {
		return fmt.Errorf("select %q has no durable selection for decision %q", node.GetId(), node.GetDecisionRef())
	}
	for _, input := range node.GetSelectInputs() {
		if input.GetCase() != tag {
			continue
		}
		output, exists := r.outputs[input.GetSource()]
		if !exists {
			return fmt.Errorf("select %q selected source %q has no output", node.GetId(), input.GetSource())
		}
		if output.Artifact.Schema != node.GetOutputSchema() {
			return fmt.Errorf("select %q output schema %q does not match selected source schema %q", node.GetId(), node.GetOutputSchema(), output.Artifact.Schema)
		}
		r.outputs[node.GetId()] = output
		return nil
	}
	return fmt.Errorf("select %q has no input for selected case %q", node.GetId(), tag)
}

// activationSkipReason is the scheduler's control-region gate. Every node
// kind uses it before execution: an inactive outer branch suppresses nested
// decisions and selects as well as ordinary steps. Selects deliberately read
// only their chosen input; the other select inputs are inactive by design.
func (r *executionResolver) activationSkipReason(node *planpb.GraphNode) (*manifestSkipReason, error) {
	if node.GetKind() == "select" {
		if reason, exists := r.inactive[node.GetDecisionRef()]; exists {
			return &reason, nil
		}
		selectedCase, exists := r.selectedCases[node.GetDecisionRef()]
		if !exists {
			return nil, fmt.Errorf("select %q depends on unresolved decision %q", node.GetId(), node.GetDecisionRef())
		}
		for _, input := range node.GetSelectInputs() {
			if input.GetCase() != selectedCase {
				continue
			}
			if reason, exists := r.inactive[input.GetSource()]; exists {
				return nil, fmt.Errorf(
					"select %q selected source %q is inactive because decision %q did not select case %q",
					node.GetId(), input.GetSource(), reason.DecisionID, reason.Case,
				)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("select %q has no input for selected case %q", node.GetId(), selectedCase)
	}

	for _, edge := range r.index.inboundByTarget[node.GetId()] {
		if reason, exists := r.inactive[edge.GetFrom()]; exists {
			return &reason, nil
		}
		if edge.GetCase() != "" {
			selectedCase, exists := r.selectedCases[edge.GetFrom()]
			if !exists {
				return nil, fmt.Errorf("conditional node %q depends on unresolved decision %q", node.GetId(), edge.GetFrom())
			}
			if selectedCase != edge.GetCase() {
				return &manifestSkipReason{Kind: "decision-not-selected", DecisionID: edge.GetFrom(), Case: edge.GetCase()}, nil
			}
		}
	}
	return nil, nil
}

func validateJSONAgainstSchema(schemaJSON string, valueJSON []byte) error {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(schemaJSON)))
	if err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(valueJSON))
	if err != nil {
		return fmt.Errorf("decode value: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("decision-case.schema.json", document); err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	schema, err := compiler.Compile("decision-case.schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	return schema.Validate(instance)
}

func nodeOutputFromPublished(published artifact.PublishedJSON, body []byte) nodeOutput {
	return nodeOutput{
		Artifact: manifestDataArtifact{Key: published.Body.Key, Hash: published.Body.Hash, ContentType: published.Body.ContentType, Schema: published.Schema},
		Published: manifestPublishedArtifact{
			Manifest: manifestArtifactRef{Key: published.Manifest.Key, Hash: published.Manifest.Hash, Size: published.Manifest.Size, ContentType: published.Manifest.ContentType},
			Body:     manifestArtifactRef{Key: published.Body.Key, Hash: published.Body.Hash, Size: published.Body.Size, ContentType: published.Body.ContentType},
			Schema:   published.Schema,
		},
		Body: body,
	}
}
