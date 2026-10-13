// Package runevent defines the local, versioned NDJSON observation contract.
package runevent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	contract "github.com/Sly1029/massive/conformance/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const Version = 1

// Event timestamps combine UTC wall time with milliseconds from the run's
// monotonic clock. Sequence numbers order concurrent item notifications.
// ItemIndex is the stable, zero-based source identity, never an author value.
type Event struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Type           string    `json:"type"`
	Sequence       int       `json:"sequence"`
	RunID          string    `json:"runId"`
	Time           string    `json:"time"`
	ElapsedMS      float64   `json:"elapsedMs"`
	PlanHash       string    `json:"planHash,omitempty"`
	NodeCount      *int      `json:"nodeCount,omitempty"`
	NodeID         string    `json:"nodeId,omitempty"`
	ItemIndex      *int      `json:"itemIndex,omitempty"`
	Attempt        int       `json:"attempt,omitempty"`
	MaxAttempts    int       `json:"maxAttempts,omitempty"`
	DurationMS     *float64  `json:"durationMs,omitempty"`
	Status         string    `json:"status,omitempty"`
	Classification string    `json:"classification,omitempty"`
	Diagnostic     string    `json:"diagnostic,omitempty"`
	LogTail        string    `json:"logTail,omitempty"`
	Completed      *int      `json:"completed,omitempty"`
	Failed         *int      `json:"failed,omitempty"`
	Total          *int      `json:"total,omitempty"`
	FailedNode     *string   `json:"failedNode,omitempty"`
	ExitHook       *ExitHook `json:"exitHook,omitempty"`
}

type ExitHook struct {
	NodeID                string `json:"nodeId"`
	Status                string `json:"status"`
	PersistenceDiagnostic string `json:"persistenceDiagnostic,omitempty"`
}

var eventSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(contract.RunEventSchemaJSON))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("run-event.json", document); err != nil {
		return nil, err
	}
	return compiler.Compile("run-event.json")
})

// Parse validates the contract before decoding. Unknown versions and fields
// are rejected; consumers must upgrade rather than infer an older transport.
func Parse(data []byte) (*Event, error) {
	schema, err := eventSchema()
	if err != nil {
		return nil, err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(value); err != nil {
		return nil, fmt.Errorf("unsupported or malformed run event; use the current CLI: %w", err)
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}
