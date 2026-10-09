package spec

import (
	"encoding/json"
	"testing"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
)

// withExitHook adds an exit hook that reuses the fixture's only step symbol and
// contract, with the shared run-outcome input and a null output.
func withExitHook(t *testing.T, mutate func(root, hook map[string]any)) []byte {
	t.Helper()
	return mutateValidFixture(t, "python-linear", func(root map[string]any) {
		var outcome any
		if err := json.Unmarshal(schemacontract.RunOutcomeSchemaJSON, &outcome); err != nil {
			t.Fatal(err)
		}
		schemas := root["schemas"].(map[string]any)
		schemas[hashRefForTest("e")] = outcome
		schemas[hashRefForTest("f")] = map[string]any{"type": "null"}
		step := nodeByID(root, "add_one")
		hook := map[string]any{
			"id": "notify", "kind": "exit-hook",
			"inputSchema": hashRefForTest("e"), "outputSchema": hashRefForTest("f"),
			"symbolRef": step["symbolRef"], "contractRef": step["contractRef"],
		}
		root["graph"].(map[string]any)["exitHook"] = hook
		mutate(root, hook)
	})
}

func TestParseAcceptsAnExitHookOutsideTheDAG(t *testing.T) {
	parsed, err := Parse(withExitHook(t, func(map[string]any, map[string]any) {}))
	if err != nil {
		t.Fatal(err)
	}
	if hook := parsed.Graph.ExitHook; hook == nil || hook.ID != "notify" || hook.Kind != NodeKindExitHook {
		t.Fatalf("exit hook = %+v", parsed.Graph.ExitHook)
	}
}

func TestParseRejectsExitHooksOutsideTheirContract(t *testing.T) {
	for name, test := range map[string]struct {
		mutate     func(root, hook map[string]any)
		diagnostic string
	}{
		"node id": {func(_, hook map[string]any) { hook["id"] = "add_one" }, "exit hook id must differ from every graph node id"},
		"input schema": {func(root, hook map[string]any) {
			hook["inputSchema"] = nodeByID(root, "add_one")["inputSchema"]
		}, "exit hooks take the run-outcome schema"},
		"output schema": {func(root, hook map[string]any) {
			hook["outputSchema"] = nodeByID(root, "add_one")["outputSchema"]
		}, "exit hooks take the run-outcome schema"},
		"missing symbol": {func(_, hook map[string]any) { hook["symbolRef"] = "python-main:workflow#missing" }, "symbol reference does not exist"},
		"retry": {func(root, _ map[string]any) {
			for _, contract := range root["contracts"].(map[string]any) {
				contract.(map[string]any)["retry"] = map[string]any{"maxAttempts": 2, "delaySeconds": 1, "backoffFactor": 1, "maxDelaySeconds": 1}
			}
		}, "exit hooks run once; their contract must not retry"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(withExitHook(t, test.mutate))
			if err == nil {
				t.Fatal("invalid exit hook accepted")
			}
			if diagnostics := diagnosticsFromError(t, err); !containsDiagnostic(diagnostics, test.diagnostic) {
				t.Fatalf("diagnostics = %#v", diagnostics)
			}
		})
	}
}
