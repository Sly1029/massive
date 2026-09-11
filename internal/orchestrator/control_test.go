package orchestrator

import (
	"bytes"
	"testing"

	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func TestRemoteControlValuesShareDecisionValidation(t *testing.T) {
	data := readRepoFile(t, "conformance", "fixtures", "specs", "exhaustive-decision", "workflow-spec.json")
	parsed, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(parsed, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value string
		valid bool
		index int
	}{
		{`{"kind":"accepted","value":7}`, true, 0},
		{`{"kind":"rejected","reason":"no"}`, true, 1},
		{`{"kind":"unknown"}`, false, -1},
		{`{"kind":"accepted","value":"invalid"}`, false, -1},
		{`{"value":7}`, false, -1},
		{`null`, false, -1},
	} {
		t.Run(tc.value, func(t *testing.T) {
			result, err := ResolveControlValue(compiled.Plan, "route", []byte(tc.value))
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid route accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.CaseIndex == nil || *result.CaseIndex != tc.index || !bytes.Equal(result.Value, []byte(tc.value)) {
				t.Fatalf("route result: %#v", result)
			}
		})
	}
	if _, err := ResolveControlValue(compiled.Plan, "choose", []byte(`"wrong"`)); err == nil {
		t.Fatal("select accepted incompatible value")
	}
}
