package argo

import (
	"encoding/json"

	"sigs.k8s.io/yaml"
)

func yamlToJSON(data []byte) ([]byte, error) {
	return yaml.YAMLToJSON(data)
}

func decodeTemplate(jsonData []byte) (*workflowTemplate, error) {
	var tmpl workflowTemplate
	if err := json.Unmarshal(jsonData, &tmpl); err != nil {
		return nil, err
	}
	return &tmpl, nil
}
