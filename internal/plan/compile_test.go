package plan

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/spec"
)

func TestCompileFixturesMatchGoldenPlans(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "passthrough"},
		{name: "linear-chain"},
		{name: "diamond"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			specData := readFixture(t, "specs", test.name, "workflow-spec.json")
			workflowSpec, err := spec.Parse(specData)
			if err != nil {
				t.Fatal(err)
			}

			first, err := Compile(workflowSpec, specData)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Compile(workflowSpec, specData)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.CanonicalJSON, second.CanonicalJSON) {
				t.Fatalf("compiled plan is not byte-stable\nfirst:  %s\nsecond: %s", first.CanonicalJSON, second.CanonicalJSON)
			}

			golden := readFixture(t, "plans", test.name, "workflow-plan.json")
			if !bytes.Equal(first.CanonicalJSON, golden) {
				t.Fatalf("plan mismatch\nactual:   %s\nexpected: %s", first.CanonicalJSON, golden)
			}
		})
	}
}

func TestCompileRejectsWorkflowSpecWhoseEmbeddedHashDoesNotMatchContent(t *testing.T) {
	original := readFixture(t, "specs", "linear-chain", "workflow-spec.json")
	workflowSpec, err := spec.Parse(original)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(original, []byte(`"name": "linear-chain"`), []byte(`"name": "linear-chains"`), 1)
	if bytes.Equal(tampered, original) {
		t.Fatal("test did not alter workflow spec bytes")
	}
	_, err = Compile(workflowSpec, tampered)
	if err == nil || !strings.Contains(err.Error(), "does not match canonical content") {
		t.Fatalf("Compile() error = %v, want embedded hash mismatch", err)
	}
}

func TestCompileDoesNotInventMaterializedSourceArtifacts(t *testing.T) {
	specData := readFixture(t, "specs", "passthrough", "workflow-spec.json")
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}

	for _, sourcePackage := range compiled.Plan.GetSourcePackages() {
		if sourcePackage.GetManifest() != nil || sourcePackage.GetSourceArchive() != nil {
			t.Fatalf("unmaterialized source package contains artifact refs: %#v", sourcePackage)
		}
	}
}

func TestCompilePreservesPythonFrontendIdentity(t *testing.T) {
	specData := readFixture(t, "specs", "python-linear", "workflow-spec.json")
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}

	if got := compiled.Plan.GetGraph().GetIrVersion(); got != "0.1" {
		t.Fatalf("Graph IR version = %q, want 0.1", got)
	}
	if len(compiled.Plan.GetSymbols()) != 1 || compiled.Plan.GetSymbols()[0].GetLanguage() != "python" {
		t.Fatalf("compiled symbols = %#v, want one Python symbol", compiled.Plan.GetSymbols())
	}
	if len(compiled.Plan.GetSourcePackages()) != 1 || compiled.Plan.GetSourcePackages()[0].GetLanguage() != "python" {
		t.Fatalf("compiled source packages = %#v, want one Python package", compiled.Plan.GetSourcePackages())
	}
	if got := compiled.Plan.GetHashing(); got.GetRecipe() != "workflow-plan" || got.GetRecipeVersion() != 1 {
		t.Fatalf("compiled plan hashing = %#v, want workflow-plan@1", got)
	}
	if got := compiled.Plan.GetSpecHashing(); got.GetRecipe() != "workflow-spec" || got.GetRecipeVersion() != 1 {
		t.Fatalf("compiled spec hashing = %#v, want workflow-spec@1", got)
	}
	if got := compiled.Plan.GetSourcePackages()[0].GetHashing(); got.GetRecipe() != "source-package" || got.GetRecipeVersion() != 1 {
		t.Fatalf("compiled source hashing = %#v, want source-package@1", got)
	}
	if len(compiled.Plan.GetEnvironments()) != 1 || compiled.Plan.GetEnvironments()[0].GetContainer().GetImage() == "" {
		t.Fatalf("compiled environments = %#v, want runnable container plan", compiled.Plan.GetEnvironments())
	}
}

func TestCompileLowersDataOnlyExhaustiveDecision(t *testing.T) {
	specData := decisionSpecData(t)
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatalf("parse decision WorkflowSpec: %v", err)
	}

	compiled, err := Compile(workflowSpec, specData)
	if err != nil {
		t.Fatalf("compile decision WorkflowSpec: %v", err)
	}

	var decision, selectNode *planpb.GraphNode
	for _, node := range compiled.Plan.GetGraph().GetNodes() {
		switch node.GetKind() {
		case "decision":
			decision = node
		case "select":
			selectNode = node
		}
	}
	if decision == nil || selectNode == nil {
		t.Fatalf("compiled graph did not preserve decision/select nodes: %#v", compiled.Plan.GetGraph().GetNodes())
	}
	if decision.GetSelector() != "kind" || len(decision.GetCases()) != 2 || decision.GetCases()[0].GetTag() != "accepted" || decision.GetCases()[1].GetTag() != "rejected" {
		t.Fatalf("compiled decision = %#v", decision)
	}
	if selectNode.GetDecisionRef() != "route" || len(selectNode.GetSelectInputs()) != 2 || selectNode.GetSelectInputs()[0].GetSource() != "accept" || selectNode.GetSelectInputs()[1].GetSource() != "reject" {
		t.Fatalf("compiled select = %#v", selectNode)
	}
	var conditional int
	for _, edge := range compiled.Plan.GetGraph().GetEdges() {
		if edge.GetCase() != "" {
			conditional++
		}
	}
	if conditional != 2 {
		t.Fatalf("compiled conditional edges = %d, want 2", conditional)
	}
}

func TestCompileLowersFiniteMapWithExactSchemaContracts(t *testing.T) {
	specData := readFixture(t, "specs", "finite-map", "workflow-spec.json")
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatalf("parse finite map WorkflowSpec: %v", err)
	}

	compiled, err := Compile(workflowSpec, specData)
	if err != nil {
		t.Fatalf("compile finite map WorkflowSpec: %v", err)
	}

	var mapNode *planpb.GraphNode
	for _, node := range compiled.Plan.GetGraph().GetNodes() {
		if node.GetKind() == "map" {
			mapNode = node
			break
		}
	}
	if mapNode == nil {
		t.Fatalf("compiled graph did not preserve map node: %#v", compiled.Plan.GetGraph().GetNodes())
	}
	if mapNode.GetInputSchema() != compiled.Plan.GetGraph().GetInputSchema() || mapNode.GetOutputSchema() != compiled.Plan.GetGraph().GetOutputSchema() {
		t.Fatalf("compiled map boundary schemas = input %q output %q; want graph schemas %q %q", mapNode.GetInputSchema(), mapNode.GetOutputSchema(), compiled.Plan.GetGraph().GetInputSchema(), compiled.Plan.GetGraph().GetOutputSchema())
	}
	if mapNode.GetItemInputSchema() == "" || mapNode.GetItemOutputSchema() == "" || mapNode.GetItemInputSchema() == mapNode.GetInputSchema() || mapNode.GetItemOutputSchema() == mapNode.GetOutputSchema() {
		t.Fatalf("compiled map item schema contracts = %#v", mapNode)
	}
	if mapNode.GetMaxConcurrency() != 3 || mapNode.GetSymbolRef() == "" || mapNode.GetContractRef() == "" {
		t.Fatalf("compiled map execution contract = %#v", mapNode)
	}
}

func readFixture(t *testing.T, fixtureKind, name, file string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", fixtureKind, name, file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decisionSpecData(t *testing.T) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(readFixture(t, "specs", "linear-chain", "workflow-spec.json"), &root); err != nil {
		t.Fatal(err)
	}
	graph := root["graph"].(map[string]any)
	graph["irVersion"] = "0.2"
	graph["nodes"] = []any{
		map[string]any{"id": "__start", "kind": "start"},
		map[string]any{"id": "__end", "kind": "end"},
		map[string]any{
			"id": "classify", "kind": "step",
			"inputSchema": hashRef("1"), "outputSchema": hashRef("1"),
			"symbolRef": "linear-chain/double", "contractRef": hashRef("8"),
		},
		map[string]any{
			"id": "route", "kind": "decision", "inputSchema": hashRef("1"), "selector": "kind",
			"cases": []any{
				map[string]any{"tag": "accepted", "schema": hashRef("1")},
				map[string]any{"tag": "rejected", "schema": hashRef("1")},
			},
		},
		map[string]any{
			"id": "accept", "kind": "step",
			"inputSchema": hashRef("1"), "outputSchema": hashRef("1"),
			"symbolRef": "linear-chain/increment", "contractRef": hashRef("8"),
		},
		map[string]any{
			"id": "reject", "kind": "step",
			"inputSchema": hashRef("1"), "outputSchema": hashRef("1"),
			"symbolRef": "linear-chain/label", "contractRef": hashRef("8"),
		},
		map[string]any{
			"id": "choose", "kind": "select", "decisionRef": "route", "outputSchema": hashRef("1"),
			"selectInputs": []any{
				map[string]any{"case": "accepted", "source": "accept"},
				map[string]any{"case": "rejected", "source": "reject"},
			},
		},
	}
	graph["edges"] = []any{
		map[string]any{"from": "__start", "to": "classify"},
		map[string]any{"from": "classify", "to": "route"},
		map[string]any{"from": "route", "to": "accept", "case": "accepted"},
		map[string]any{"from": "route", "to": "reject", "case": "rejected"},
		map[string]any{"from": "accept", "to": "choose"},
		map[string]any{"from": "reject", "to": "choose"},
		map[string]any{"from": "choose", "to": "__end"},
	}
	data, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := spec.RecomputedSpecHash(data)
	if err != nil {
		t.Fatal(err)
	}
	root["specHash"] = hash
	data, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hashRef(character string) string {
	return "sha256:" + string(bytes.Repeat([]byte(character), 64))
}
