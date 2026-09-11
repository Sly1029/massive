package runjournal

type Manifest struct {
	Kind          string        `json:"kind"`
	SchemaVersion uint32        `json:"schemaVersion"`
	Encoding      string        `json:"encoding"`
	PlanHash      string        `json:"planHash"`
	ProjectKey    string        `json:"projectKey"`
	RunID         string        `json:"runId"`
	Status        string        `json:"status"`
	Steps         []Step        `json:"steps"`
	Decisions     []Decision    `json:"decisions"`
	Result        *DataArtifact `json:"result,omitempty"`
}

// The run-manifest transport is intentionally versioned independently of the
// graph IR. Schema v3/json-v3 adds source-indexed finite-map item records to
// the v2 manifest-last output and durable routing journal. The local
// orchestrator currently executes one attempt per step (attempt 1); target
// retry scheduling and later attempt records are a subsequent slice.

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
