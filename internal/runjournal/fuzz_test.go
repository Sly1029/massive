package runjournal

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// journalWithPartialMap constructs real journal values at an invocation boundary.
// The byte stream chooses independently completed, failed, cancelled, running, and queued items.
func journalWithPartialMap(states []byte) Manifest {
	input := DataArtifact{Key: "inputs/map", Hash: "sha256:input", ContentType: "application/json", Schema: "sha256:schema"}
	output := PublishedArtifact{Manifest: ArtifactRef{Key: "outputs/manifest", Hash: "sha256:manifest", Size: 1, ContentType: "application/json"}, Body: ArtifactRef{Key: "outputs/body", Hash: "sha256:body", Size: 1, ContentType: "application/json"}, Schema: "sha256:schema"}
	items := make([]MapItem, len(states))
	for index, state := range states {
		item := MapItem{Index: index, Status: "pending", Attempts: []Attempt{}}
		if state%5 != 0 {
			item.Status = "running"
			attempt := Attempt{Attempt: 1, Status: "running", Input: input}
			if state%5 == 2 {
				item.Status = "succeeded"
				attempt.Status = "succeeded"
				attempt.Output = &output
			}
			if state%5 == 3 || state%5 == 4 {
				item.Status = "failed"
				if state%5 == 4 {
					item.Status = "cancelled"
				}
				attempt.Status = item.Status
				attempt.Diagnostic = "invocation terminated"
			}
			item.Attempts = append(item.Attempts, attempt)
		}
		items[index] = item
	}
	return Manifest{Kind: "RunManifest", SchemaVersion: 4, Encoding: "json-v4", PlanHash: "sha256:plan", ProjectKey: "project", RunID: "run", Status: "running", Decisions: []Decision{{NodeID: "route", Status: "selected", SelectedCase: "active"}}, Steps: []Step{
		{NodeID: "map", Status: "running", Attempts: []Attempt{{Attempt: 1, Status: "running", Input: input}}, Items: &items},
		{NodeID: "next", Status: "pending", Attempts: []Attempt{}},
		{NodeID: "complete", Status: "succeeded", Attempts: []Attempt{{Attempt: 1, Status: "succeeded", Input: input, Output: &output}}},
		{NodeID: "inactive", Status: "skipped", Attempts: []Attempt{}, SkipReason: &SkipReason{Kind: "decision-not-selected", DecisionID: "route", Case: "inactive"}},
		{NodeID: "queued-map", Status: "pending", Attempts: []Attempt{}, Items: &[]MapItem{}},
		{NodeID: "started", Status: "running", Attempts: []Attempt{{Attempt: 1, Status: "running", Input: input}}},
	}}
}

func FuzzJournalParsing(f *testing.F) {
	for _, status := range []string{"running", "failed", "cancelled"} {
		m := journalWithPartialMap([]byte{0, 1, 2})
		if status != "running" {
			m.Terminate(status, "stopped")
		}
		body, err := json.Marshal(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body)
	}
	f.Add([]byte(`{"schemaVersion":3,"encoding":"json-v3"}`))
	f.Add([]byte(`null`))
	succeeded := journalWithPartialMap(nil)
	succeeded.Status = "succeeded"
	succeeded.Steps = succeeded.Steps[2:4]
	succeeded.Result = &succeeded.Steps[0].Attempts[0].Input
	body, err := json.Marshal(succeeded)
	if err != nil {
		f.Fatal(err)
	}
	if _, err := Parse(body); err != nil {
		f.Fatal(err)
	}
	f.Add(body)
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 65536 {
			t.Skip()
		}
		parsed, err := Parse(body)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(parsed)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Parse(encoded)
		if err != nil {
			t.Fatalf("accepted journal does not round trip: %v", err)
		}
		if !reflect.DeepEqual(parsed, again) {
			t.Fatal("journal changed during round trip")
		}
	})
}

func FuzzJournalTermination(f *testing.F) {
	f.Add([]byte{0, 1, 2}, false)
	f.Add([]byte{}, true)
	f.Add([]byte{2, 2, 1, 0, 0}, true)
	f.Fuzz(func(t *testing.T, states []byte, cancelled bool) {
		if len(states) > 64 {
			t.Skip()
		}
		m := journalWithPartialMap(states)
		before, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(before); err != nil {
			t.Fatalf("invalid running fixture: %v", err)
		}
		status := "failed"
		if cancelled {
			status = "cancelled"
		}
		m.Terminate(status, "execution stopped")
		body, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(body)
		if err != nil {
			t.Fatalf("invalid terminal journal: %v", err)
		}
		if parsed.Result != nil || parsed.Status != status || parsed.Steps[1].Status != "not-started" {
			t.Fatal("incorrect terminal run")
		}
		original := journalWithPartialMap(states)
		if !reflect.DeepEqual(parsed.Decisions, original.Decisions) || !reflect.DeepEqual(parsed.Steps[2:4], original.Steps[2:4]) {
			t.Fatal("completed work or routing record changed")
		}
		if parsed.Steps[4].Status != "not-started" || len(parsed.Steps[4].Attempts) != 0 || len(*parsed.Steps[4].Items) != 0 {
			t.Fatal("queued map acquired work")
		}
		if parsed.Steps[5].Status != status || parsed.Steps[5].Attempts[0].Status != status {
			t.Fatal("started step lost terminal status")
		}
		for index, item := range *parsed.Steps[0].Items {
			initial := (*original.Steps[0].Items)[index]
			switch initial.Status {
			case "succeeded", "failed", "cancelled":
				if !reflect.DeepEqual(item, initial) {
					t.Fatal("completed artifact changed")
				}
			case "pending":
				if item.Status != "not-started" || len(item.Attempts) != 0 {
					t.Fatal("undispatched work acquired an attempt")
				}
			case "running":
				if item.Status != status || len(item.Attempts) != 1 || item.Attempts[0].Status != status {
					t.Fatal("started work lost its terminal attempt")
				}
			}
		}
		m.Terminate(status, "execution stopped")
		repeated, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, repeated) {
			t.Fatal("termination is not idempotent")
		}
	})
}
