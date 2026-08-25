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

type indexedValue struct {
	Index int             `json:"index"`
	Value json.RawMessage `json:"value"`
}

type emptyMarker struct {
	Empty bool `json:"empty"`
}

// ArgoItems projects a finite map input into indexed loop values. Argo does
// not expose a stable source index for withParam items, so the index travels
// with each value. The singleton empty marker avoids Argo's unresolved
// aggregate-output behavior for a zero-iteration loop; it never invokes user
// code and CollectArgoResults removes it again.
func ArgoItems(body []byte) ([]byte, error) {
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Argo map input: %w", err)
	}
	items, err := Expand(canonicalBody)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return canonical.Marshal([]emptyMarker{{Empty: true}})
	}
	values := make([]indexedValue, len(items))
	for index, item := range items {
		values[index] = indexedValue{Index: item.Index, Value: item.Body}
	}
	return canonical.Marshal(values)
}

// ParseArgoItem validates one indexed loop value or the singleton empty-map
// marker. Values are canonicalized again after Argo templating before they are
// passed to a language runner.
func ParseArgoItem(body []byte) (Item, bool, error) {
	index, value, empty, err := parseArgoEnvelope(body)
	if err != nil {
		return Item{}, false, fmt.Errorf("parse Argo map item: %w", err)
	}
	return Item{Index: index, Body: value}, empty, nil
}

// ArgoResult binds one mapper result back to its source index before Argo
// aggregates parallel task outputs.
func ArgoResult(index int, body []byte) ([]byte, error) {
	if index < 0 {
		return nil, fmt.Errorf("map result index %d must be nonnegative", index)
	}
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil || !bytes.Equal(canonicalBody, body) {
		return nil, fmt.Errorf("map item %d output must be canonical JSON", index)
	}
	return canonical.Marshal(indexedValue{Index: index, Value: body})
}

// CollectArgoResults unwraps Argo's aggregate output, accepting both raw JSON
// objects and the JSON-string form used by some Argo releases, then delegates
// dense ordered collection to Collect.
func CollectArgoResults(body []byte) ([]byte, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, fmt.Errorf("decode Argo map results: %w", err)
	}
	results := make([]Result, 0, len(values))
	emptyMarkers := 0
	for position, value := range values {
		envelope := []byte(value)
		if len(envelope) > 0 && envelope[0] == '"' {
			var encoded string
			if err := json.Unmarshal(envelope, &encoded); err != nil {
				return nil, fmt.Errorf("decode Argo map result %d string: %w", position, err)
			}
			envelope = []byte(encoded)
		}
		canonicalEnvelope, err := canonical.CanonicalizeJSON(envelope)
		if err != nil {
			return nil, fmt.Errorf("canonicalize Argo map result %d: %w", position, err)
		}
		index, result, empty, err := parseArgoEnvelope(canonicalEnvelope)
		if err != nil {
			return nil, fmt.Errorf("parse Argo map result %d: %w", position, err)
		}
		if empty {
			emptyMarkers++
			continue
		}
		results = append(results, Result{Index: index, Body: result})
	}
	if emptyMarkers > 0 {
		if emptyMarkers != 1 || len(values) != 1 {
			return nil, fmt.Errorf("empty-map marker cannot be combined with map results")
		}
		return []byte(`[]`), nil
	}
	return Collect(len(results), results)
}

func parseArgoEnvelope(body []byte) (int, []byte, bool, error) {
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil {
		return 0, nil, false, fmt.Errorf("map envelope must be JSON: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonicalBody, &fields); err != nil {
		return 0, nil, false, fmt.Errorf("decode map envelope: %w", err)
	}
	if len(fields) == 1 && bytes.Equal(fields["empty"], []byte("true")) {
		return 0, nil, true, nil
	}
	if len(fields) != 2 || fields["index"] == nil || fields["value"] == nil {
		return 0, nil, false, fmt.Errorf("map envelope must contain exactly index and value")
	}
	var index int
	if err := json.Unmarshal(fields["index"], &index); err != nil || index < 0 {
		return 0, nil, false, fmt.Errorf("map envelope index must be a nonnegative integer")
	}
	value, err := canonical.CanonicalizeJSON(fields["value"])
	if err != nil {
		return 0, nil, false, fmt.Errorf("canonicalize map envelope value: %w", err)
	}
	return index, value, false, nil
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
