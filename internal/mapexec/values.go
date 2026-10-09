// Package mapexec owns the value semantics of a finite map scope. It expands
// one already-crystallized canonical JSON array into stable source-indexed
// items and assembles completed item values in that same order.
package mapexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Sly1029/massive/internal/canonical"
)

type Item struct {
	Index int
	Body  []byte
}

type Result struct {
	Index int
	Body  []byte
}

// Expand returns one immutable item per source-array position. Equal values
// remain separate items because index, not body hash, is the map identity.
func Expand(body []byte) ([]Item, error) {
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil || !bytes.Equal(canonicalBody, body) {
		return nil, fmt.Errorf("map input must be canonical JSON")
	}
	if len(body) == 0 || body[0] != '[' {
		return nil, fmt.Errorf("map input must be a JSON array")
	}

	var values []json.RawMessage
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, fmt.Errorf("decode map input array: %w", err)
	}
	items := make([]Item, len(values))
	for index, value := range values {
		itemBody, err := canonical.CanonicalizeJSON(value)
		if err != nil {
			return nil, fmt.Errorf("canonicalize map item %d: %w", index, err)
		}
		items[index] = Item{Index: index, Body: itemBody}
	}
	return items, nil
}

// Collect builds one canonical JSON array in source-index order, independent
// of runner completion order. A result set must contain each dense source
// index exactly once; partial, duplicate, or foreign results are rejected.
func Collect(expectedCount int, results []Result) ([]byte, error) {
	if expectedCount < 0 || len(results) != expectedCount {
		return nil, fmt.Errorf("map returned %d results, want %d", len(results), expectedCount)
	}
	ordered := make([]json.RawMessage, expectedCount)
	seen := make([]bool, expectedCount)
	for _, result := range results {
		if result.Index < 0 || result.Index >= expectedCount {
			return nil, fmt.Errorf("map result index %d is outside dense range 0..%d", result.Index, expectedCount-1)
		}
		if seen[result.Index] {
			return nil, fmt.Errorf("map result index %d is duplicated", result.Index)
		}
		canonicalBody, err := canonical.CanonicalizeJSON(result.Body)
		if err != nil || !bytes.Equal(canonicalBody, result.Body) {
			return nil, fmt.Errorf("map item %d output must be canonical JSON", result.Index)
		}
		seen[result.Index] = true
		ordered[result.Index] = result.Body
	}

	return canonical.Marshal(ordered)
}

// FailureKind classifies a terminal item failure that a map collected.
type FailureKind string

const (
	// FailureError is an author exception or a runner that exited nonzero.
	FailureError FailureKind = "error"
	// FailureKilled is a runner ended by a signal, such as an out-of-memory kill.
	FailureKilled FailureKind = "killed"
	// FailureNonRetryable is the author's NonRetryableError.
	FailureNonRetryable FailureKind = "non-retryable"
	// FailureTimeout is an attempt stopped at the contract's per-attempt deadline.
	FailureTimeout FailureKind = "timeout"
)

// DiagnosticLimit bounds a failure diagnostic in Unicode code points, the
// unit of the outcome schema's maxLength.
const DiagnosticLimit = 1024

// Failure is the closed record of an item's terminal failure. It is a
// workflow value, so it reaches downstream author code.
type Failure struct {
	Attempts   int         `json:"attempts"`
	Diagnostic string      `json:"diagnostic"`
	Kind       FailureKind `json:"kind"`
}

// NewFailure bounds the diagnostic to DiagnosticLimit code points of valid UTF-8.
func NewFailure(kind FailureKind, attempts int, diagnostic string) Failure {
	diagnostic = strings.ToValidUTF8(diagnostic, "\uFFFD")
	if runes := []rune(diagnostic); len(runes) > DiagnosticLimit {
		diagnostic = string(runes[:DiagnosticLimit])
	}
	return Failure{Attempts: attempts, Diagnostic: diagnostic, Kind: kind}
}

// Outcome is one item's result when its map collects item failures: a
// canonical value, or a failure.
type Outcome struct {
	Index   int
	Value   []byte
	Failure *Failure
}

// CollectOutcomes builds the source-ordered list of item outcomes with the
// same dense-index rules as Collect. Each entry is {"status":"succeeded",
// "value":...} or {"failure":{...},"status":"failed"}.
func CollectOutcomes(expectedCount int, outcomes []Outcome) ([]byte, error) {
	results := make([]Result, len(outcomes))
	for position, outcome := range outcomes {
		var entry any
		switch {
		case outcome.Failure == nil:
			canonicalValue, err := canonical.CanonicalizeJSON(outcome.Value)
			if err != nil || !bytes.Equal(canonicalValue, outcome.Value) {
				return nil, fmt.Errorf("map item %d output must be canonical JSON", outcome.Index)
			}
			entry = map[string]any{"status": "succeeded", "value": json.RawMessage(outcome.Value)}
		case outcome.Value == nil:
			entry = map[string]any{"failure": *outcome.Failure, "status": "failed"}
		default:
			return nil, fmt.Errorf("map item %d has both a value and a failure", outcome.Index)
		}
		body, err := canonical.Marshal(entry)
		if err != nil {
			return nil, err
		}
		results[position] = Result{Index: outcome.Index, Body: body}
	}
	return Collect(expectedCount, results)
}

// ParseFailure reads a canonical failure record that an item runtime built
// for an attempt budget of maxAttempts.
func ParseFailure(body []byte, maxAttempts int) (Failure, error) {
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil || !bytes.Equal(canonicalBody, body) {
		return Failure{}, fmt.Errorf("failure record must be canonical JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var failure Failure
	if err := decoder.Decode(&failure); err != nil {
		return Failure{}, fmt.Errorf("decode failure record: %w", err)
	}
	switch failure.Kind {
	case FailureError, FailureKilled, FailureNonRetryable, FailureTimeout:
	default:
		return Failure{}, fmt.Errorf("failure kind %q is not a collected kind", failure.Kind)
	}
	if failure.Attempts < 1 || failure.Attempts > maxAttempts {
		return Failure{}, fmt.Errorf("failure after %d attempts is outside the %d-attempt budget", failure.Attempts, maxAttempts)
	}
	if failure != NewFailure(failure.Kind, failure.Attempts, failure.Diagnostic) {
		return Failure{}, fmt.Errorf("failure diagnostic must be valid UTF-8 of at most %d code points", DiagnosticLimit)
	}
	return failure, nil
}
