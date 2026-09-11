package runjournal

type Manifest struct {
	Kind          string        `json:"kind"`
	SchemaVersion uint32        `json:"schemaVersion"`
	Encoding      string        `json:"encoding"`
	PlanHash      string        `json:"planHash"`
	ProjectKey    string        `json:"projectKey"`
	RunID         string        `json:"runId"`
	Status        string        `json:"status"`
	Diagnostic    string        `json:"diagnostic,omitempty"`
	Steps         []Step        `json:"steps"`
	Decisions     []Decision    `json:"decisions"`
	Result        *DataArtifact `json:"result,omitempty"`
}

// The run journal is versioned independently of graph IR. Only v4/json-v4 is
// accepted. Terminal runs distinguish cancelled attempts from undispatched work;
// every planned step has a terminal status. Execution still uses one attempt.

type Step struct {
	NodeID     string      `json:"nodeId"`
	Status     string      `json:"status"`
	Attempts   []Attempt   `json:"attempts"`
	Items      *[]MapItem  `json:"items,omitempty"`
	SkipReason *SkipReason `json:"skipReason,omitempty"`
}

// MapItem keeps each source-indexed invocation observable even when a
// sibling fails. A terminal not-started item has no attempts; otherwise its
// attempts describe the item runner. The containing map step's attempt
// describes collection into the static map-node output slot.
type MapItem struct {
	Index      int       `json:"index"`
	Status     string    `json:"status"`
	Attempts   []Attempt `json:"attempts"`
	Diagnostic string    `json:"diagnostic,omitempty"`
}

// Decision is the durable decision record. Replays must use this
// selection rather than evaluate the classifier body a second time.
type Decision struct {
	NodeID       string      `json:"nodeId"`
	Status       string      `json:"status"`
	SelectedCase string      `json:"selectedCase,omitempty"`
	Diagnostic   string      `json:"diagnostic,omitempty"`
	SkipReason   *SkipReason `json:"skipReason,omitempty"`
}

// SkipReason makes an inactive branch observable rather than leaving
// it indistinguishable from a scheduler omission.
type SkipReason struct {
	Kind       string `json:"kind"`
	DecisionID string `json:"decisionId"`
	Case       string `json:"case"`
}

type Attempt struct {
	Attempt    int                `json:"attempt"`
	Status     string             `json:"status"`
	Input      DataArtifact       `json:"input"`
	Output     *PublishedArtifact `json:"output,omitempty"`
	Diagnostic string             `json:"diagnostic,omitempty"`
}

// PublishedArtifact records both legs of manifest-last publication.
// The manifest is the logical attempt output; Body is its independently
// content-addressed canonical JSON value.
type PublishedArtifact struct {
	Manifest ArtifactRef `json:"manifest"`
	Body     ArtifactRef `json:"body"`
	Schema   string      `json:"schema"`
}

type ArtifactRef struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	Size        int    `json:"size"`
	ContentType string `json:"contentType"`
}

type DataArtifact struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	ContentType string `json:"contentType"`
	Schema      string `json:"schema"`
}

// Terminate preserves completed work and closes every unfinished journal entry.
// Callers reconcile actual invocation outcomes before terminating the run.
func (manifest *Manifest) Terminate(status, diagnostic string) {
	manifest.Status = status
	manifest.Diagnostic = diagnostic
	manifest.Result = nil
	for index := range manifest.Steps {
		step := &manifest.Steps[index]
		switch step.Status {
		case "pending":
			step.Status = "not-started"
		case "running":
			step.Status = status
			for attempt := range step.Attempts {
				step.Attempts[attempt].Status = status
				step.Attempts[attempt].Diagnostic = diagnostic
			}
		}
		if step.Items == nil {
			continue
		}
		for itemIndex := range *step.Items {
			item := &(*step.Items)[itemIndex]
			switch item.Status {
			case "pending":
				item.Status = "not-started"
				item.Diagnostic = diagnostic
			case "running":
				item.Status = status
				for attempt := range item.Attempts {
					item.Attempts[attempt].Status = status
					item.Attempts[attempt].Diagnostic = diagnostic
				}
			}
		}
	}
}
