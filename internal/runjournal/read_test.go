package runjournal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJournalReaderValidatesTerminalMapState(t *testing.T) {
	input := DataArtifact{Key: "inputs/map", Hash: "sha256:input", ContentType: "application/json", Schema: "sha256:schema"}
	items := []MapItem{
		{Index: 0, Status: "failed", Attempts: []Attempt{{Attempt: 1, Status: "failed", Input: input, Diagnostic: "user error"}}},
		{Index: 1, Status: "not-started", Attempts: []Attempt{}, Diagnostic: "sibling failed"},
	}
	original := Manifest{Kind: "RunManifest", SchemaVersion: 3, Encoding: "json-v3", PlanHash: "sha256:plan", ProjectKey: "project", RunID: "run", Status: "failed", Decisions: []Decision{}, Steps: []Step{{NodeID: "map", Status: "failed", Attempts: []Attempt{{Attempt: 1, Status: "failed", Input: input, Diagnostic: "map failed"}}, Items: &items}}}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(body); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Manifest)
	}{
		{"obsolete transport", func(m *Manifest) { m.SchemaVersion = 2; m.Encoding = "json-v2" }},
		{"sparse source order", func(m *Manifest) { (*m.Steps[0].Items)[1].Index = 5 }},
		{"attempt disagreement", func(m *Manifest) { (*m.Steps[0].Items)[0].Status = "succeeded" }},
		{"unfinished failed map", func(m *Manifest) { (*m.Steps[0].Items)[1].Status = "pending" }},
		{"not-started without failure", func(m *Manifest) { m.Steps[0].Status = "running" }},
		{"successful without result", func(m *Manifest) { m.Status = "succeeded" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed Manifest
			if err := json.Unmarshal(body, &changed); err != nil {
				t.Fatal(err)
			}
			tc.change(&changed)
			encoded, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(encoded)
			if err == nil {
				t.Fatal("invalid journal accepted")
			}
			if tc.name == "obsolete transport" && !strings.Contains(err.Error(), "current SDK") {
				t.Fatal(err)
			}
		})
	}
}
