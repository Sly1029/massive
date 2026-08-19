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
		{name: "python-linear", path: fixturePath("python-linear")},
		{name: "exhaustive-decision", path: fixturePath("exhaustive-decision")},
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
		root["graph"].(map[string]any)["irVersion"] = "0.3"
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected invalid spec")
	}

	diagnostics := diagnosticsFromError(t, err)
	if diagnostics[0].Path != "$.graph.irVersion" || diagnostics[0].Ref != "0.3" || !strings.Contains(diagnostics[0].Message, "compiler supports >=0.1 <0.3") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostics)
	}
}

func TestParseConstrainsGraphNodeIDsToSafeDatastoreSegments(t *testing.T) {
	for _, nodeID := range []string{"nested/double", `nested\\double`, ".", ".."} {
		t.Run("rejects "+nodeID, func(t *testing.T) {
			data := mutateFixture(t, "linear-chain", func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				nodes := graph["nodes"].([]any)
				nodes[2].(map[string]any)["id"] = nodeID
				for _, rawEdge := range graph["edges"].([]any) {
					edge := rawEdge.(map[string]any)
					if edge["from"] == "double" {
						edge["from"] = nodeID
					}
					if edge["to"] == "double" {
						edge["to"] = nodeID
					}
				}
			})

			if _, err := Parse(data); err == nil {
				t.Fatalf("Parse accepted unsafe graph node id %q", nodeID)
			}
		})
	}

	for _, nodeID := range []string{"_step", ".hidden"} {
		t.Run("accepts "+nodeID, func(t *testing.T) {
			data := mutateFixture(t, "linear-chain", func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				nodes := graph["nodes"].([]any)
				nodes[2].(map[string]any)["id"] = nodeID
				for _, rawEdge := range graph["edges"].([]any) {
					edge := rawEdge.(map[string]any)
					if edge["from"] == "double" {
						edge["from"] = nodeID
					}
					if edge["to"] == "double" {
						edge["to"] = nodeID
					}
				}
			})

			if _, err := Parse(data); err != nil {
				t.Fatalf("Parse rejected safe graph node id %q: %v", nodeID, err)
			}
		})
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

func TestParseRejectsInvalidExhaustiveDecisionContracts(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		message string
	}{
		{
			name: "duplicate case tag",
			mutate: func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				for _, rawNode := range graph["nodes"].([]any) {
					node := rawNode.(map[string]any)
					if node["id"] == "route" {
						node["cases"].([]any)[1].(map[string]any)["tag"] = "accepted"
					}
				}
			},
			message: "decision case tags must be unique",
		},
		{
			name: "missing branch edge",
			mutate: func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				edges := graph["edges"].([]any)
				graph["edges"] = append(edges[:3:3], edges[4:]...)
			},
			message: "each decision case requires exactly one conditional outgoing edge",
		},
		{
			name: "select source does not match inbound edge",
			mutate: func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				for _, rawNode := range graph["nodes"].([]any) {
					node := rawNode.(map[string]any)
					if node["id"] == "choose" {
						node["selectInputs"].([]any)[1].(map[string]any)["source"] = "classify"
					}
				}
			},
			message: "select input sources must exactly match inbound edges",
		},
		{
			name: "select source belongs to a different case branch",
			mutate: func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				for _, rawNode := range graph["nodes"].([]any) {
					node := rawNode.(map[string]any)
					if node["id"] == "choose" {
						inputs := node["selectInputs"].([]any)
						inputs[0].(map[string]any)["source"] = "reject"
						inputs[1].(map[string]any)["source"] = "accept"
					}
				}
			},
			message: "select input source must be reachable from its decision case branch",
		},
		{
			name: "case branches converge before their select",
			mutate: func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				graph["edges"] = append(graph["edges"].([]any), map[string]any{"from": "accept", "to": "reject"})
			},
			message: "cases may converge only through select",
		},
		{
			name: "conditional branches share a target",
			mutate: func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				for _, rawEdge := range graph["edges"].([]any) {
					edge := rawEdge.(map[string]any)
					if edge["from"] == "route" && edge["case"] == "rejected" {
						edge["to"] = "accept"
					}
				}
			},
			message: "conditional branches may not share a target",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(mutateFixture(t, "exhaustive-decision", test.mutate))
			if err == nil {
				t.Fatal("expected invalid spec")
			}
			if !containsDiagnostic(diagnosticsFromError(t, err), test.message) {
				t.Fatalf("diagnostics = %#v, want %q", diagnosticsFromError(t, err), test.message)
			}
		})
	}
}

func TestParseRejectsDecisionSchemaContractMismatches(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		path    string
		message string
	}{
		{
			name: "decision input differs from producer output",
			mutate: func(root map[string]any) {
				nodeByID(root, "route")["inputSchema"] = hashRefForTest("1")
			},
			path:    "$.graph.nodes[3].inputSchema",
			message: "decision input schema must equal its sole producer output schema",
		},
		{
			name: "decision has multiple inbound values",
			mutate: func(root map[string]any) {
				graph := root["graph"].(map[string]any)
				graph["edges"] = append(graph["edges"].([]any), map[string]any{"from": "__start", "to": "route"})
			},
			path:    "$.graph.nodes[3].inputSchema",
			message: "decision requires exactly one inbound value producer",
		},
		{
			name: "conditional target input differs from case",
			mutate: func(root map[string]any) {
				nodeByID(root, "accept")["inputSchema"] = hashRefForTest("3")
			},
			path:    "$.graph.nodes[4].inputSchema",
			message: "conditional target input schema must equal decision case schema",
		},
		{
			name: "conditional target is not executable step",
			mutate: func(root map[string]any) {
				edges := root["graph"].(map[string]any)["edges"].([]any)
				edges[2].(map[string]any)["to"] = "choose"
			},
			path:    "$.graph.edges[2].to",
			message: "conditional edge target must be a step node",
		},
		{
			name: "select source output differs from select output",
			mutate: func(root map[string]any) {
				nodeByID(root, "accept")["outputSchema"] = hashRefForTest("3")
			},
			path:    "$.graph.nodes[6].selectInputs[0].source",
			message: "select source output schema must equal select output schema",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(mutateFixture(t, "exhaustive-decision", test.mutate))
			if err == nil {
				t.Fatal("expected invalid spec")
			}
			for _, diagnostic := range diagnosticsFromError(t, err) {
				if diagnostic.Path == test.path && strings.Contains(diagnostic.Message, test.message) {
					return
				}
			}
			t.Fatalf("diagnostics = %#v, want path %q containing %q", diagnosticsFromError(t, err), test.path, test.message)
		})
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

func nodeByID(root map[string]any, id string) map[string]any {
	for _, rawNode := range root["graph"].(map[string]any)["nodes"].([]any) {
		node := rawNode.(map[string]any)
		if node["id"] == id {
			return node
		}
	}
	panic("fixture node not found: " + id)
}

func hashRefForTest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
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

func containsDiagnostic(diagnostics []Diagnostic, message string) bool {
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, message) {
			return true
		}
	}
	return false
}
