package orchestrator

type runManifest struct {
	Kind          string                `json:"kind"`
	SchemaVersion uint32                `json:"schemaVersion"`
	PlanHash      string                `json:"planHash"`
	ProjectKey    string                `json:"projectKey"`
	RunID         string                `json:"runId"`
	Status        string                `json:"status"`
	Steps         []manifestStep        `json:"steps"`
	Result        *manifestDataArtifact `json:"result,omitempty"`
}

type manifestStep struct {
	NodeID   string            `json:"nodeId"`
	Status   string            `json:"status"`
	Attempts []manifestAttempt `json:"attempts"`
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
