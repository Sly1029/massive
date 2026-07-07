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
	Attempt    int                   `json:"attempt"`
	Status     string                `json:"status"`
	Input      manifestDataArtifact  `json:"input"`
	Output     *manifestDataArtifact `json:"output,omitempty"`
	Diagnostic string                `json:"diagnostic,omitempty"`
}

type manifestDataArtifact struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	ContentType string `json:"contentType"`
	Schema      string `json:"schema"`
}
