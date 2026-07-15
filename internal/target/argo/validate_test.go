package argo

import (
	"encoding/json"
	"testing"
)

// WS-7.3: valid generated YAML passes offline schema validation; invalid is
// caught. We validate the canonical JSON form (same field tree as the emitted
// YAML) against the vendored Argo CRD schema.
func TestStructureValidationAcceptsGeneratedTemplate(t *testing.T) {
	index := buildPlanIndex(compileFixturePlan(t, "diamond").Plan)
	tmpl, err := buildWorkflowTemplate(index, compileContext{
		plan:     compileFixturePlan(t, "diamond").Plan,
		planHash: "sha256:" + strings40(),
		config:   argoConfigValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	templateJSON, err := marshalCanonicalTemplateJSON(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStructure(templateJSON); err != nil {
		t.Fatalf("valid template should pass Argo schema validation: %v", err)
	}
}

func TestStructureValidationRejectsInvalidTemplate(t *testing.T) {
	index := buildPlanIndex(compileFixturePlan(t, "diamond").Plan)
	tmpl, err := buildWorkflowTemplate(index, compileContext{
		plan:     compileFixturePlan(t, "diamond").Plan,
		planHash: "sha256:" + strings40(),
		config:   argoConfigValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	templateJSON, err := marshalCanonicalTemplateJSON(tmpl)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(map[string]any){
		"missing spec":     func(m map[string]any) { delete(m, "spec") },
		"entrypoint typed": func(m map[string]any) { m["spec"].(map[string]any)["entrypoint"] = 42 },
		"templates typed":  func(m map[string]any) { m["spec"].(map[string]any)["templates"] = "not-a-list" },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			var tree map[string]any
			if err := json.Unmarshal(templateJSON, &tree); err != nil {
				t.Fatal(err)
			}
			tamper(tree)
			broken, err := json.Marshal(tree)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateStructure(broken); err == nil {
				t.Fatalf("tampered template (%s) should fail schema validation", name)
			}
		})
	}
}

func strings40() string {
	return "3b404991915f640e1f596518c3cd04ce5653df3960aa9b3af9a224226ec960de"
}
