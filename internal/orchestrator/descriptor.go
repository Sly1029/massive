package orchestrator

type StepInvocationDescriptor struct {
	Kind           string                       `json:"kind"`
	SchemaVersion  uint32                       `json:"schemaVersion"`
	Encoding       string                       `json:"encoding"`
	PlanHash       string                       `json:"planHash"`
	RunID          string                       `json:"runId"`
	NodeID         string                       `json:"nodeId"`
	Attempt        int                          `json:"attempt"`
	Symbol         StepSymbolRef                `json:"symbol"`
	SourcePackage  SourcePackageRef             `json:"sourcePackage"`
	EnvironmentRef string                       `json:"environmentRef"`
	Input          DataArtifactRef              `json:"input"`
	Output         DataArtifactDestination      `json:"output"`
	ChannelReads   []ChannelArtifactRef         `json:"channelReads"`
	ChannelWrites  []ChannelArtifactDestination `json:"channelWrites"`
	Datastore      DatastoreDescriptor          `json:"datastore"`
}

type StepSymbolRef struct {
	PackageID string `json:"packageId"`
	Language  string `json:"language"`
	Module    string `json:"module"`
	Export    string `json:"export"`
}

type SourcePackageRef struct {
	PackageID     string       `json:"packageId"`
	Language      string       `json:"language"`
	PackageHash   string       `json:"packageHash"`
	SourceArchive ArtifactRef  `json:"sourceArchive"`
	Manifest      *ArtifactRef `json:"manifest,omitempty"`
}

type ArtifactRef struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	ContentType string `json:"contentType"`
}

type ArtifactDestination struct {
	Key         string `json:"key"`
	ContentType string `json:"contentType"`
}

type DataArtifactRef struct {
	Artifact ArtifactRef `json:"artifact"`
	Schema   string      `json:"schema"`
}

type DataArtifactDestination struct {
	Artifact ArtifactDestination `json:"artifact"`
	Schema   string              `json:"schema"`
}

type ChannelArtifactRef struct {
	ChannelName string      `json:"channelName"`
	Artifact    ArtifactRef `json:"artifact"`
	Schema      string      `json:"schema"`
}

type ChannelArtifactDestination struct {
	ChannelName string              `json:"channelName"`
	Artifact    ArtifactDestination `json:"artifact"`
	Schema      string              `json:"schema"`
}

type DatastoreDescriptor struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}
