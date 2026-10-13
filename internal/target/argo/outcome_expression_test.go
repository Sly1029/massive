package argo

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Sly1029/massive/internal/valueparam"
	"github.com/expr-lang/expr"
)

func TestGeneratedOutcomeExpressionResolvesTerminalPodIdentity(t *testing.T) {
	templates := compiledTemplates(t, fixturePlan(t, "python-map-outcomes").Plan)
	outcome := templateByName(t, templates, "map-outcome-scan")
	valueFrom := outcome["outputs"].(map[string]any)["parameters"].([]any)[0].(map[string]any)["valueFrom"].(map[string]any)
	const terminalID = "massive-map-outcomes-t7rq5-1718880123"
	for name, attempt := range map[string]map[string]any{
		"OOM without outputs":         {"id": terminalID},
		"executor without parameters": {"id": terminalID, "outputs": map[string]any{}},
		"reported outcome":            {"id": terminalID, "outputs": map[string]any{"parameters": map[string]any{"result": `{"index":0,"value":7}`}}},
	} {
		t.Run(name, func(t *testing.T) {
			// Argo v3.7.16 evaluates ValueFrom.Expression with Expr v1.17.7.
			// ValueFrom.Default is returned literally, without template expansion.
			expression, ok := valueFrom["expression"].(string)
			if !ok {
				t.Fatal("item outcome requires an expression evaluated in task scope")
			}
			env := map[string]any{"tasks": map[string]any{"attempt": attempt}}
			program, err := expr.Compile(expression, expr.Env(env))
			if err != nil {
				t.Fatal(err)
			}
			output, err := expr.Run(program, env)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal([]any{output})
			if err != nil {
				t.Fatal(err)
			}
			nodes, err := valueparam.MissingOutcomeNodes(body)
			if err != nil {
				t.Fatal(err)
			}
			if name == "reported outcome" {
				if len(nodes) != 0 || output != `{"index":0,"value":7}` {
					t.Fatalf("reported result changed: %v, %v", output, nodes)
				}
			} else if !reflect.DeepEqual(nodes, []string{terminalID}) {
				t.Fatalf("missing result identity = %v, want terminal pod %q", nodes, terminalID)
			}
		})
	}
}
