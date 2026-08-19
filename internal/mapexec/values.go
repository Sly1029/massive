// Package mapexec owns the value semantics of a finite map scope. It expands
// one already-crystallized canonical JSON array into stable source-indexed
// items and assembles completed item values in that same order.
package mapexec

import (
	"bytes"
	"encoding/json"
	"fmt"

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
	ordered := make([][]byte, expectedCount)
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

	var output bytes.Buffer
	output.WriteByte('[')
	for index, body := range ordered {
		if index > 0 {
			output.WriteByte(',')
		}
		output.Write(body)
	}
	output.WriteByte(']')
	return output.Bytes(), nil
}
