package argo

import (
	"encoding/json"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"sigs.k8s.io/yaml"
)

func yamlToJSON(data []byte) ([]byte, error) {
	return yaml.YAMLToJSON(data)
}

// argoConfigJSON renders an argoConfig as the canonical config bytes a neutral
// CompileInput carries (the target request minus its kind), so tests exercise
// the same decode path the CLI feeds from a parsed spec.
func argoConfigJSON(t *testing.T, config argoConfig) []byte {
	t.Helper()
	members := map[string]any{}
	if config.Namespace != "" {
		members["namespace"] = config.Namespace
	}
	if config.ServiceAccountName != "" {
		members["serviceAccountName"] = config.ServiceAccountName
	}
	if config.WorkflowTemplateName != "" {
		members["workflowTemplateName"] = config.WorkflowTemplateName
	}
	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	canonicalConfig, err := canonical.CanonicalizeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return canonicalConfig
}

func decodeTemplate(jsonData []byte) (*workflowTemplate, error) {
	var tmpl workflowTemplate
	if err := json.Unmarshal(jsonData, &tmpl); err != nil {
		return nil, err
	}
	return &tmpl, nil
}
