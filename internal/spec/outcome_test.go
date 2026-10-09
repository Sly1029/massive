package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapOutcomeSchemaVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "map-outcomes", "outcome-schemas.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Name             string          `json:"name"`
			Accepted         bool            `json:"accepted"`
			ItemOutputSchema json.RawMessage `json:"itemOutputSchema"`
			OutputSchema     json.RawMessage `json:"outputSchema"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatal("no outcome schema vectors")
	}
	for _, vector := range vectors.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			if got := MapOutcomeListSchemaMatches(vector.OutputSchema, vector.ItemOutputSchema); got != vector.Accepted {
				t.Fatalf("MapOutcomeListSchemaMatches = %t, want %t", got, vector.Accepted)
			}
		})
	}
}

func TestParseAcceptsPythonMapOutcomesFixture(t *testing.T) {
	data, err := os.ReadFile(fixturePath("python-map-outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range parsed.Graph.Nodes {
		if node.Kind == NodeKindMap && node.ItemFailures != MapItemFailuresCollect {
			t.Fatalf("map %s itemFailures = %q", node.ID, node.ItemFailures)
		}
	}
}

func TestParseRelatesMapOutputToItemFailurePolicy(t *testing.T) {
	setPolicy := func(fixture string, policy any) []byte {
		return mutateValidFixture(t, fixture, func(root map[string]any) {
			for _, node := range root["graph"].(map[string]any)["nodes"].([]any) {
				if node := node.(map[string]any); node["kind"] == "map" {
					if policy == nil {
						delete(node, "itemFailures")
					} else {
						node["itemFailures"] = policy
					}
				}
			}
		})
	}
	for _, test := range []struct {
		name, fixture string
		policy        any
		want          string
	}{
		{name: "outcomes without the policy", fixture: "python-map-outcomes", want: "items exactly equal itemOutputSchema"},
		{name: "items with the policy", fixture: "finite-map", policy: "collect", want: "array of item outcomes wrapping itemOutputSchema"},
		{name: "explicit default", fixture: "python-map-outcomes", policy: "fail", want: "validation failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(setPolicy(test.fixture, test.policy))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateControlFlowChecksMapItemFailurePolicy(t *testing.T) {
	data, err := os.ReadFile(fixturePath("python-map-outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateControlFlow(parsed.Graph, parsed.Schemas); err != nil {
		t.Fatalf("valid collecting map rejected: %v", err)
	}
	for index, node := range parsed.Graph.Nodes {
		if node.Kind == NodeKindMap {
			parsed.Graph.Nodes[index].ItemFailures = ""
		}
	}
	if err := ValidateControlFlow(parsed.Graph, parsed.Schemas); err == nil || !strings.Contains(err.Error(), "items exactly equal itemOutputSchema") {
		t.Fatalf("ValidateControlFlow without the policy = %v", err)
	}
}
