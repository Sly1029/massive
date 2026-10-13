// Package executionphase records which runtime phase survived container death.
package executionphase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Phase string

const (
	Hydrate Phase = "hydrate"
	Author  Phase = "author"
	Publish Phase = "publish"
	// RecordBytes keeps every overwrite the same size, below kubelet's limits.
	RecordBytes = 32
)

type Evidence struct {
	Phase Phase `json:"phase,omitempty"`
	Fatal bool  `json:"fatal,omitempty"`
}

var evidenceSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.ExecutionEvidenceSchemaJSON))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("execution-evidence.schema.json", document); err != nil {
		return nil, err
	}
	return compiler.Compile("execution-evidence.schema.json")
})

func Parse(message string) (Evidence, error) {
	if len(message) > RecordBytes {
		return Evidence{}, fmt.Errorf("execution evidence exceeds %d bytes", RecordBytes)
	}
	validator, err := evidenceSchema()
	if err != nil {
		return Evidence{}, err
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(message))
	if err != nil {
		return Evidence{}, err
	}
	if err := validator.Validate(instance); err != nil {
		return Evidence{}, err
	}
	var evidence Evidence
	err = json.Unmarshal([]byte(message), &evidence)
	return evidence, err
}

// Write updates the kubelet-mounted inode in a single fixed-size write. Renaming
// over the mount point is unavailable, and truncating would erase prior evidence
// if the process died before its next write. Callers serialize phase transitions.
func Write(path string, evidence Evidence) error {
	if path == "" {
		return nil
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	if _, err := Parse(string(body)); err != nil {
		return fmt.Errorf("invalid execution evidence: %w", err)
	}
	body = append(body, bytes.Repeat([]byte(" "), RecordBytes-len(body))...)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open execution evidence: %w", err)
	}
	defer file.Close()
	written, err := file.Write(body)
	if err != nil {
		return fmt.Errorf("write execution evidence: %w", err)
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	return nil
}
