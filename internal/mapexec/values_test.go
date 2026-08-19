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
	collected, err := mapexec.Collect(completed)
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
	collected, err := mapexec.Collect(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(collected) != "[]" {
		t.Fatalf("collected %s, want []", collected)
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
	if _, err := mapexec.Collect([]mapexec.Result{{Index: 0, Body: []byte(`{"value": 1}`)}}); err == nil {
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
			if _, err := mapexec.Collect(results); err == nil {
				t.Fatal("expected result identity error")
			}
		})
	}
}
