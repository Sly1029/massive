package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapArrayItemSchemaVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "array-items", "schemas.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Name        string          `json:"name"`
			Accepted    bool            `json:"accepted"`
			ItemSchema  json.RawMessage `json:"itemSchema"`
			ArraySchema json.RawMessage `json:"arraySchema"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatal("no array item schema vectors")
	}
	for _, vector := range vectors.Cases {
		for _, boundary := range []string{"input", "output"} {
			t.Run(vector.Name+"/"+boundary, func(t *testing.T) {
				data := mutateValidFixture(t, "finite-map", func(root map[string]any) {
					node := root["graph"].(map[string]any)["nodes"].([]any)[1].(map[string]any)
					schemas := root["schemas"].(map[string]any)
					itemField := "itemInputSchema"
					if boundary == "output" {
						itemField = "itemOutputSchema"
					}
					schemas[node[boundary+"Schema"].(string)] = vector.ArraySchema
					schemas[node[itemField].(string)] = vector.ItemSchema
				})
				parsed, err := Parse(data)
				if vector.Accepted {
					if err != nil {
						t.Fatal(err)
					}
					if err := ValidateControlFlow(parsed.Graph, parsed.Schemas); err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "items exactly equal "+map[string]string{"input": "itemInputSchema", "output": "itemOutputSchema"}[boundary]) {
					t.Fatalf("Parse error = %v, want exact item schema mismatch", err)
				}
			})
		}
	}
}

func TestMapArrayItemLocalPointerCycles(t *testing.T) {
	for _, test := range []struct {
		name, schema string
		accepted     bool
	}{
		{"self", `{"type":"array","items":{"$ref":"#/items"}}`, false},
		{"mutual", `{"type":"array","items":{"$ref":"#/additionalProperties"},"additionalProperties":{"$ref":"#/items"}}`, false},
		{"chain", `{"type":"array","items":{"$ref":"#/additionalProperties"},"additionalProperties":{"type":"integer"}}`, true},
	} {
		for _, boundary := range []string{"input", "output"} {
			t.Run(test.name+"/"+boundary, func(t *testing.T) {
				data := mutateValidFixture(t, "finite-map", func(root map[string]any) {
					node := root["graph"].(map[string]any)["nodes"].([]any)[1].(map[string]any)
					schemas := root["schemas"].(map[string]any)
					schemas[node[boundary+"Schema"].(string)] = json.RawMessage(test.schema)
					schemas[node["item"+strings.ToUpper(boundary[:1])+boundary[1:]+"Schema"].(string)] = json.RawMessage(`{"type":"integer"}`)
				})
				parsed, err := Parse(data)
				if test.accepted {
					if err != nil {
						t.Fatal(err)
					}
					if err := ValidateControlFlow(parsed.Graph, parsed.Schemas); err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "items exactly equal item"+strings.ToUpper(boundary[:1])+boundary[1:]+"Schema") {
					t.Fatalf("Parse error = %v, want map item schema validation error", err)
				}
			})
		}
	}
}
