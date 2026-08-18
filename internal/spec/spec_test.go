package spec

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptsValidFixtures(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "passthrough", path: fixturePath("passthrough")},
		{name: "linear-chain", path: fixturePath("linear-chain")},
		{name: "diamond", path: fixturePath("diamond")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
		})
	}
}

func TestParseReportsMissingContractRefFixture(t *testing.T) {
	data, err := os.ReadFile(fixturePath("invalid-missing-contract-ref"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.nodes[2].contractRef" {
		t.Fatalf("unexpected diagnostic path: %#v", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Message, "contractRef") {
		t.Fatalf("unexpected diagnostic message: %#v", diagnostics)
	}
}

func TestParseReportsDanglingContractRef(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		graph := root["graph"].(map[string]any)
		nodes := graph["nodes"].([]any)
		step := nodes[2].(map[string]any)
		step["contractRef"] = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.nodes[2].contractRef" {
		t.Fatalf("unexpected diagnostic path: %#v", diagnostics)
	}
	if diagnostics[0].Ref != "sha256:9999999999999999999999999999999999999999999999999999999999999999" {
		t.Fatalf("unexpected diagnostic ref: %#v", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Message, "contract reference") {
		t.Fatalf("unexpected diagnostic message: %#v", diagnostics)
	}
}

func TestParseReportsCycle(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		graph := root["graph"].(map[string]any)
		graph["edges"] = append(graph["edges"].([]any), map[string]any{"from": "label", "to": "double"})
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.edges" {
		t.Fatalf("unexpected diagnostic path: %#v", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Message, "cycle") {
		t.Fatalf("unexpected diagnostic message: %#v", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Ref, "double -> increment -> label -> double") {
		t.Fatalf("unexpected cycle: %#v", diagnostics)
	}
}

func TestParseReportsUnsupportedGraphIRVersion(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		root["graph"].(map[string]any)["irVersion"] = "0.2"
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.irVersion" || diagnostics[0].Ref != "0.2" || !strings.Contains(diagnostics[0].Message, "compiler supports >=0.1 <0.2") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func TestParseRejectsDuplicateEdge(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		graph := root["graph"].(map[string]any)
		edges := graph["edges"].([]any)
		graph["edges"] = append(edges, edges[0])
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.edges[4]" || !strings.Contains(diagnostics[0].Message, "duplicate") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func TestParseRejectsStartWithInboundEdge(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		graph := root["graph"].(map[string]any)
		graph["edges"] = append(graph["edges"].([]any), map[string]any{"from": "__end", "to": "__start"})
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.edges" || !strings.Contains(diagnostics[0].Message, "start node must not have inbound") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func TestParseRejectsEndWithMultipleInboundEdges(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		graph := root["graph"].(map[string]any)
		graph["edges"] = append(graph["edges"].([]any), map[string]any{"from": "increment", "to": "__end"})
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.end" || !strings.Contains(diagnostics[0].Message, "exactly one inbound") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func TestParseRejectsMergeInputsThatDoNotMatchInboundEdges(t *testing.T) {
	data := mutateFixture(t, "diamond", func(root map[string]any) {
		graph := root["graph"].(map[string]any)
		for _, rawNode := range graph["nodes"].([]any) {
			node := rawNode.(map[string]any)
			if node["id"] == "merge" {
				node["mergeInputs"] = []any{"left"}
			}
		}
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.nodes[5].mergeInputs" || !strings.Contains(diagnostics[0].Message, "exactly match inbound") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func TestParseRejectsSourcePackageMapKeyMismatch(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		packages := root["sourcePackages"].(map[string]any)
		packages["ts-main"].(map[string]any)["packageId"] = "other-package"
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.sourcePackages.ts-main.packageId" || !strings.Contains(diagnostics[0].Message, "must match map key") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func TestParseRejectsMutableContainerRecipeImage(t *testing.T) {
	data := mutateFixture(t, "linear-chain", func(root map[string]any) {
		environments := root["environments"].(map[string]any)
		for _, rawEnvironment := range environments {
			environment := rawEnvironment.(map[string]any)
			environment["image"] = "registry.example/python:3.12"
		}
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.environments.sha256:7777777777777777777777777777777777777777777777777777777777777777.image" || !strings.Contains(diagnostics[0].Message, "immutable image digest") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "conformance", "fixtures", "specs", name, "workflow-spec.json")
}

func mutateFixture(t *testing.T, name string, mutate func(map[string]any)) []byte {
	t.Helper()

	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}

	mutate(root)

	output, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func diagnosticsFromError(t *testing.T, err error) []Diagnostic {
	t.Helper()

	diagnostics, ok := err.(*DiagnosticsError)
	if !ok {
		t.Fatalf("expected DiagnosticsError, got %T: %v", err, err)
	}
	if len(diagnostics.Diagnostics) == 0 {
		t.Fatal("expected diagnostics")
	}
	return diagnostics.Diagnostics
}
