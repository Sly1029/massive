package runjournal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	contract "github.com/Sly1029/massive/conformance/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var manifestSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(contract.RunManifestSchemaJSON))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("run-manifest.json", document); err != nil {
		return nil, err
	}
	return compiler.Compile("run-manifest.json")
})

// Parse reads only the current journal. Historical transports require a new run,
// never an inferred migration or a partially decoded view.
func Parse(data []byte) (*Manifest, error) {
	schema, err := manifestSchema()
	if err != nil {
		return nil, err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid run journal JSON: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return nil, fmt.Errorf("unsupported or malformed run journal; create a new run with the current SDK: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	for _, step := range manifest.Steps {
		if manifest.Status != "running" && (step.Status == "pending" || step.Status == "running") {
			return nil, fmt.Errorf("terminal run has unfinished step %q", step.NodeID)
		}
		if manifest.Status == "succeeded" && step.Status != "succeeded" && step.Status != "skipped" {
			return nil, fmt.Errorf("successful run has unsuccessful step %q", step.NodeID)
		}
		if len(step.Attempts) > 0 && step.Attempts[0].Status != step.Status {
			return nil, fmt.Errorf("step %q attempt status differs from step status", step.NodeID)
		}
		if step.Items == nil {
			continue
		}
		if (step.Status == "pending" || step.Status == "skipped" || step.Status == "not-started") && len(*step.Items) != 0 {
			return nil, fmt.Errorf("inactive map %q contains item records", step.NodeID)
		}
		for index, item := range *step.Items {
			if item.Index != index {
				return nil, fmt.Errorf("map %q item indexes must be dense and source ordered", step.NodeID)
			}
			if len(item.Attempts) > 0 && item.Attempts[0].Status != item.Status {
				return nil, fmt.Errorf("map %q item %d attempt status differs", step.NodeID, index)
			}
			if item.Status == "not-started" && step.Status != "failed" && step.Status != "cancelled" {
				return nil, fmt.Errorf("map %q has not-started items without termination", step.NodeID)
			}
			if step.Status == "succeeded" && item.Status != "succeeded" {
				return nil, fmt.Errorf("successful map %q has unsuccessful items", step.NodeID)
			}
			if (step.Status == "failed" || step.Status == "cancelled") && (item.Status == "pending" || item.Status == "running") {
				return nil, fmt.Errorf("terminal map %q has unfinished items", step.NodeID)
			}
		}
	}
	return &manifest, nil
}
