package mapexec_test

import (
	"bytes"
	"testing"

	"github.com/Sly1029/massive/internal/mapexec"
)

func TestExpandAndCollectPreserveSourceIndexAndDuplicates(t *testing.T) {
	items, err := mapexec.Expand([]byte(`[{"value":3},{"value":1},{"value":3},{"value":2}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4", len(items))
	}
	for index, item := range items {
		if item.Index != index {
			t.Fatalf("item %d has index %d", index, item.Index)
		}
	}

	// Simulate completion in a different order, then let the scheduler place
	// each result back into its source-indexed slot before collection.
	completed := []mapexec.Result{
		{Index: 1, Body: []byte(`{"doubled":2}`)},
		{Index: 3, Body: []byte(`{"doubled":4}`)},
		{Index: 0, Body: []byte(`{"doubled":6}`)},
		{Index: 2, Body: []byte(`{"doubled":6}`)},
	}
	collected, err := mapexec.Collect(len(items), completed)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`[{"doubled":6},{"doubled":2},{"doubled":6},{"doubled":4}]`)
	if !bytes.Equal(collected, want) {
		t.Fatalf("collected %s, want %s", collected, want)
	}
}

func TestEmptyMapCollectsAnEmptyArray(t *testing.T) {
	items, err := mapexec.Expand([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d items, want none", len(items))
	}
	collected, err := mapexec.Collect(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(collected) != "[]" {
		t.Fatalf("collected %s, want []", collected)
	}
}

func TestArgoItemsKeepDuplicateValuesDistinctAndMakeEmptyMapsRunnable(t *testing.T) {
	items, err := mapexec.ArgoItems([]byte(`[3, 3, {"name": "x"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(items), `[{"index":0,"value":3},{"index":1,"value":3},{"index":2,"value":{"name":"x"}}]`; got != want {
		t.Fatalf("Argo items = %s, want %s", got, want)
	}

	empty, err := mapexec.ArgoItems([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(empty), `[{"empty":true}]`; got != want {
		t.Fatalf("empty Argo items = %s, want %s", got, want)
	}
}

func TestArgoItemAndResultEnvelopesPreserveCanonicalValues(t *testing.T) {
	item, empty, err := mapexec.ParseArgoItem([]byte(`{ "value": { "name": "x" }, "index": 2 }`))
	if err != nil || empty || item.Index != 2 || string(item.Body) != `{"name":"x"}` {
		t.Fatalf("parsed item = %#v, empty=%v, err=%v", item, empty, err)
	}

	result, err := mapexec.ArgoResult(item.Index, []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(result), `{"index":2,"value":{"ok":true}}`; got != want {
		t.Fatalf("Argo result = %s, want %s", got, want)
	}
}

func TestCollectArgoResultsAcceptsArgoRawAndStringAggregates(t *testing.T) {
	for name, body := range map[string][]byte{
		"raw objects":  []byte(`[{"index":1,"value":"b"},{"index":0,"value":"a"}]`),
		"JSON strings": []byte(`["{\"index\":1,\"value\":\"b\"}","{\"index\":0,\"value\":\"a\"}"]`),
	} {
		t.Run(name, func(t *testing.T) {
			collected, err := mapexec.CollectArgoResults(body)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(collected), `["a","b"]`; got != want {
				t.Fatalf("collected = %s, want %s", got, want)
			}
		})
	}

	empty, err := mapexec.CollectArgoResults([]byte(`[{"empty":true}]`))
	if err != nil || string(empty) != `[]` {
		t.Fatalf("empty collection = %s, err=%v", empty, err)
	}
}

func TestArgoEnvelopeValidationRejectsAmbiguousOrInvalidCollections(t *testing.T) {
	for name, body := range map[string][]byte{
		"negative index":          []byte(`{"index":-1,"value":1}`),
		"extra item field":        []byte(`{"extra":true,"index":0,"value":1}`),
		"empty mixed with result": []byte(`[{"empty":true},{"index":0,"value":1}]`),
		"missing dense result":    []byte(`[{"index":1,"value":1}]`),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "negative index" || name == "extra item field" {
				if _, _, err := mapexec.ParseArgoItem(body); err == nil {
					t.Fatal("expected item validation failure")
				}
				return
			}
			if _, err := mapexec.CollectArgoResults(body); err == nil {
				t.Fatal("expected collection validation failure")
			}
		})
	}
}

func TestExpandAndCollectRejectValuesOutsideCanonicalArrayContract(t *testing.T) {
	for name, body := range map[string][]byte{
		"object":       []byte(`{"value":1}`),
		"null":         []byte(`null`),
		"noncanonical": []byte(`[1, 2]`),
		"fraction":     []byte(`[1.5]`),
	} {
		t.Run("expand "+name, func(t *testing.T) {
			if _, err := mapexec.Expand(body); err == nil {
				t.Fatal("expected expansion error")
			}
		})
	}
	if _, err := mapexec.Collect(1, []mapexec.Result{{Index: 0, Body: []byte(`{"value": 1}`)}}); err == nil {
		t.Fatal("expected collection error for noncanonical item")
	}
}

func TestCollectRejectsIncompleteOrDuplicateResultIdentity(t *testing.T) {
	for name, results := range map[string][]mapexec.Result{
		"negative": {
			{Index: -1, Body: []byte(`1`)},
		},
		"outside dense range": {
			{Index: 1, Body: []byte(`1`)},
		},
		"duplicate leaves gap": {
			{Index: 0, Body: []byte(`1`)},
			{Index: 0, Body: []byte(`2`)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mapexec.Collect(len(results), results); err == nil {
				t.Fatal("expected result identity error")
			}
		})
	}
}

func TestCollectRejectsAResultSetShorterThanTheExpandedInput(t *testing.T) {
	if _, err := mapexec.Collect(4, []mapexec.Result{
		{Index: 0, Body: []byte(`1`)},
		{Index: 1, Body: []byte(`2`)},
		{Index: 2, Body: []byte(`3`)},
	}); err == nil {
		t.Fatal("expected collection error for a missing tail result")
	}
}
