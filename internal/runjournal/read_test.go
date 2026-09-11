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
	original := Manifest{Kind: "RunManifest", SchemaVersion: 4, Encoding: "json-v4", PlanHash: "sha256:plan", ProjectKey: "project", RunID: "run", Status: "failed", Diagnostic: "map failed", Decisions: []Decision{}, Steps: []Step{{NodeID: "map", Status: "failed", Attempts: []Attempt{{Attempt: 1, Status: "failed", Input: input, Diagnostic: "map failed"}}, Items: &items}}}
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
		{"obsolete transport", func(m *Manifest) { m.SchemaVersion = 3; m.Encoding = "json-v3" }},
		{"sparse source order", func(m *Manifest) { (*m.Steps[0].Items)[1].Index = 5 }},
		{"attempt disagreement", func(m *Manifest) { (*m.Steps[0].Items)[0].Status = "succeeded" }},
		{"unfinished failed map", func(m *Manifest) { (*m.Steps[0].Items)[1].Status = "pending" }},
		{"not-started without failure", func(m *Manifest) {
			m.Status = "running"
			m.Diagnostic = ""
			m.Steps[0].Status = "running"
			m.Steps[0].Attempts[0].Status = "running"
			m.Steps[0].Attempts[0].Diagnostic = ""
		}},
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

func TestTerminalJournalRejectsUnfinishedWork(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		for _, unfinished := range []string{"pending", "running"} {
			t.Run(status+"/"+unfinished, func(t *testing.T) {
				m := journalWithPartialMap([]byte{0, 1, 2})
				m.Terminate(status, "stopped")
				m.Steps[1].Status = unfinished
				if unfinished == "running" {
					m.Steps[1].Attempts = []Attempt{{Attempt: 1, Status: "running", Input: m.Steps[0].Attempts[0].Input}}
				}
				body, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Parse(body); err == nil {
					t.Fatal("terminal journal accepted an unfinished step")
				}
				m.Terminate(status, "stopped")
				item := &(*m.Steps[0].Items)[0]
				item.Status = unfinished
				item.Diagnostic = ""
				item.Attempts = []Attempt{}
				if unfinished == "running" {
					item.Attempts = []Attempt{{Attempt: 1, Status: "running", Input: m.Steps[0].Attempts[0].Input}}
				}
				body, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Parse(body); err == nil {
					t.Fatal("terminal journal accepted an unfinished item")
				}
			})
		}
	}
}
