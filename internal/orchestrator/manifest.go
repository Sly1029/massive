package orchestrator

type runManifest struct {
	Kind          string                `json:"kind"`
	SchemaVersion uint32                `json:"schemaVersion"`
	Encoding      string                `json:"encoding"`
	PlanHash      string                `json:"planHash"`
	ProjectKey    string                `json:"projectKey"`
	RunID         string                `json:"runId"`
	Status        string                `json:"status"`
	Steps         []manifestStep        `json:"steps"`
	Decisions     []manifestDecision    `json:"decisions"`
	Result        *manifestDataArtifact `json:"result,omitempty"`
}

// The run-manifest transport is intentionally versioned independently of the
// graph IR. Schema v2/json-v2 records manifest-last outputs together with
// durable data-only routing. The local orchestrator currently executes one
// attempt per step (attempt 1); target retry scheduling and later attempt
// records are a subsequent slice.

type manifestStep struct {
	NodeID     string              `json:"nodeId"`
	Status     string              `json:"status"`
	Attempts   []manifestAttempt   `json:"attempts"`
	Items      *[]manifestMapItem  `json:"items,omitempty"`
	SkipReason *manifestSkipReason `json:"skipReason,omitempty"`
}

// manifestMapItem keeps each source-indexed invocation observable even when a
// sibling fails. Its attempts describe the item runner; the containing map
// step's attempts describe collection into the static map-node output slot.
type manifestMapItem struct {
	Index    int               `json:"index"`
	Status   string            `json:"status"`
	Attempts []manifestAttempt `json:"attempts"`
}

// manifestDecision is the durable decision record. Replays must use this
// selection rather than evaluate the classifier body a second time.
type manifestDecision struct {
	NodeID       string              `json:"nodeId"`
	Status       string              `json:"status"`
	SelectedCase string              `json:"selectedCase,omitempty"`
	Diagnostic   string              `json:"diagnostic,omitempty"`
	SkipReason   *manifestSkipReason `json:"skipReason,omitempty"`
}

// manifestSkipReason makes an inactive branch observable rather than leaving
// it indistinguishable from a scheduler omission.
type manifestSkipReason struct {
	Kind       string `json:"kind"`
	DecisionID string `json:"decisionId"`
	Case       string `json:"case"`
}

type manifestAttempt struct {
	Attempt    int                        `json:"attempt"`
	Status     string                     `json:"status"`
	Input      manifestDataArtifact       `json:"input"`
	Output     *manifestPublishedArtifact `json:"output,omitempty"`
	Diagnostic string                     `json:"diagnostic,omitempty"`
}

// manifestPublishedArtifact records both legs of manifest-last publication.
// The manifest is the logical attempt output; Body is its independently
// content-addressed canonical JSON value.
type manifestPublishedArtifact struct {
	Manifest manifestArtifactRef `json:"manifest"`
	Body     manifestArtifactRef `json:"body"`
	Schema   string              `json:"schema"`
}

type manifestArtifactRef struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	Size        int    `json:"size"`
	ContentType string `json:"contentType"`
}

type manifestDataArtifact struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	ContentType string `json:"contentType"`
	Schema      string `json:"schema"`
}
