package runjournal

import (
	"encoding/json"
	"testing"
)

func TestTerminatePreservesEarlierFailedAttempts(t *testing.T) {
	input := DataArtifact{Key: "inputs/step", Hash: "sha256:input", ContentType: "application/json", Schema: "sha256:schema"}
	items := []MapItem{{Index: 0, Status: "running", Attempts: []Attempt{
		{Attempt: 1, Status: "failed", Input: input, Diagnostic: "item flake"},
		{Attempt: 2, Status: "running", Input: input},
	}}}
	manifest := Manifest{Kind: "RunManifest", SchemaVersion: 4, Encoding: "json-v4", PlanHash: "sha256:plan", ProjectKey: "project", RunID: "run", Status: "running", Decisions: []Decision{}, Steps: []Step{
		{NodeID: "step", Status: "running", Attempts: []Attempt{
			{Attempt: 1, Status: "failed", Input: input, Diagnostic: "step flake"},
			{Attempt: 2, Status: "running", Input: input},
		}},
		{NodeID: "map", Status: "running", Attempts: []Attempt{{Attempt: 1, Status: "running", Input: input}}, Items: &items},
	}}

	manifest.Terminate("cancelled", "run cancelled")

	for _, attempts := range [][]Attempt{manifest.Steps[0].Attempts, (*manifest.Steps[1].Items)[0].Attempts} {
		if attempts[0].Status != "failed" || attempts[0].Diagnostic == "run cancelled" {
			t.Fatalf("earlier attempt overwritten: %#v", attempts[0])
		}
		if attempts[1].Status != "cancelled" || attempts[1].Diagnostic != "run cancelled" {
			t.Fatalf("running attempt = %#v, want cancelled", attempts[1])
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(body); err != nil {
		t.Fatalf("terminated multi-attempt journal rejected: %v", err)
	}

	for name, change := range map[string]func(*Manifest){
		"gap in attempt numbers":     func(m *Manifest) { m.Steps[0].Attempts[1].Attempt = 3 },
		"earlier attempt not failed": func(m *Manifest) { m.Steps[0].Attempts[0].Status = "cancelled" },
		"step disagrees with last":   func(m *Manifest) { m.Steps[0].Attempts[1].Status = "failed" },
		"item gap in attempts":       func(m *Manifest) { (*m.Steps[1].Items)[0].Attempts[0].Attempt = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			var changed Manifest
			if err := json.Unmarshal(body, &changed); err != nil {
				t.Fatal(err)
			}
			change(&changed)
			encoded, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(encoded); err == nil {
				t.Fatal("invalid attempt history accepted")
			}
		})
	}
}
