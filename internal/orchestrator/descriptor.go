package orchestrator

type StepInvocationDescriptor struct {
	Kind           string                          `json:"kind"`
	SchemaVersion  uint32                          `json:"schemaVersion"`
	Encoding       string                          `json:"encoding"`
	PlanHash       string                          `json:"planHash"`
	ProjectKey     string                          `json:"projectKey"`
	RunID          string                          `json:"runId"`
	NodeID         string                          `json:"nodeId"`
	Attempt        int                             `json:"attempt"`
	Symbol         StepSymbolRef                   `json:"symbol"`
	SourcePackage  SourcePackageRef                `json:"sourcePackage"`
	EnvironmentRef string                          `json:"environmentRef"`
	Input          DataArtifactRef                 `json:"input"`
	Output         DataArtifactManifestDestination `json:"output"`
	ChannelReads   []ChannelArtifactRef            `json:"channelReads"`
	ChannelWrites  []ChannelArtifactDestination    `json:"channelWrites"`
	Datastore      DatastoreDescriptor             `json:"datastore"`
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

// DataArtifactManifestDestination names the immutable commit point for a
// canonical JSON output. The runner does not receive a mutable body key: the
// artifact runtime derives and conditionally publishes its content-addressed
// body before publishing this manifest.
type DataArtifactManifestDestination struct {
	ManifestKey string `json:"manifestKey"`
	Schema      string `json:"schema"`
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

// DatastoreDescriptor is the portable descriptor union. Local orchestration
// currently emits LocalDatastoreDescriptor; Argo and other remote targets can
// emit S3DatastoreDescriptor without inventing a different JSON shape.
type DatastoreDescriptor interface {
	datastoreDescriptor()
}

type LocalDatastoreDescriptor struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

func (LocalDatastoreDescriptor) datastoreDescriptor() {}

type S3DatastoreDescriptor struct {
	Kind           string `json:"kind"`
	Bucket         string `json:"bucket"`
	Region         string `json:"region"`
	Prefix         string `json:"prefix,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	ForcePathStyle *bool  `json:"forcePathStyle,omitempty"`
}

func (S3DatastoreDescriptor) datastoreDescriptor() {}
