package argo

import (
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
)

// StepInvocationDescriptor mirrors internal/orchestrator's descriptor shape and
// the conformance step-invocation-descriptor schema, so the pod-side runner
// invocation is identical to a local run: fetch the source package, resolve the
// symbol, read the input artifact, execute, write the output artifact.
//
// Run-scoped fields use Argo's runtime template variables. Argo substitutes
// {{workflow.uid}} into the descriptor (delivered as a raw input artifact) when
// the pod starts, so one reusable WorkflowTemplate serves every run. Fields that
// depend on a live run — the input artifact digest and the pod-reachable
// datastore endpoint — are finalized by the cluster harness (WS-8); the wedge
// records documented placeholders for them.
type StepInvocationDescriptor struct {
	Kind           string                       `json:"kind"`
	SchemaVersion  int                          `json:"schemaVersion"`
	Encoding       string                       `json:"encoding"`
	PlanHash       string                       `json:"planHash"`
	RunID          string                       `json:"runId"`
	NodeID         string                       `json:"nodeId"`
	Attempt        int                          `json:"attempt"`
	Symbol         stepSymbolRef                `json:"symbol"`
	SourcePackage  sourcePackageRef             `json:"sourcePackage"`
	EnvironmentRef string                       `json:"environmentRef"`
	Input          dataArtifactRef              `json:"input"`
	Output         dataArtifactDestination      `json:"output"`
	ChannelReads   []channelArtifactRef         `json:"channelReads"`
	ChannelWrites  []channelArtifactDestination `json:"channelWrites"`
	Datastore      datastoreDescriptor          `json:"datastore"`
}

type stepSymbolRef struct {
	PackageID string `json:"packageId"`
	Language  string `json:"language"`
	Module    string `json:"module"`
	Export    string `json:"export"`
}

type sourcePackageRef struct {
	PackageID     string      `json:"packageId"`
	Language      string      `json:"language"`
	PackageHash   string      `json:"packageHash"`
	SourceArchive artifactRef `json:"sourceArchive"`
}

type artifactRef struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	ContentType string `json:"contentType"`
}

type artifactDestination struct {
	Key         string `json:"key"`
	ContentType string `json:"contentType"`
}

type dataArtifactRef struct {
	Artifact artifactRef `json:"artifact"`
	Schema   string      `json:"schema"`
}

type dataArtifactDestination struct {
	Artifact artifactDestination `json:"artifact"`
	Schema   string              `json:"schema"`
}

type channelArtifactRef struct {
	ChannelName string      `json:"channelName"`
	Artifact    artifactRef `json:"artifact"`
	Schema      string      `json:"schema"`
}

type channelArtifactDestination struct {
	ChannelName string              `json:"channelName"`
	Artifact    artifactDestination `json:"artifact"`
	Schema      string              `json:"schema"`
}

type datastoreDescriptor struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

const (
	// argoRunIDVariable is Argo's per-run unique id, substituted at pod start.
	argoRunIDVariable = "{{workflow.uid}}"
	// descriptorMountPath is where the raw descriptor artifact is written in the
	// step container and where the runner is told to read it.
	descriptorMountPath = "/massive/descriptor.json"
	// wedgeDatastorePath is a documented placeholder; WS-8 replaces it with the
	// pod-reachable datastore (MinIO / S3-compatible endpoint).
	wedgeDatastorePath = "/massive/store"
	jsonContentType    = "application/json"
)

// buildStepDescriptor assembles the descriptor for one step node from the plan.
// Static identity (symbol, source package, schemas, plan hash) comes straight
// from the plan and is byte-stable; run-scoped keys use argoRunIDVariable.
func buildStepDescriptor(index planIndex, planHash string, node *planpb.GraphNode) (StepInvocationDescriptor, error) {
	symbol, ok := index.symbolsByRef[node.GetSymbolRef()]
	if !ok {
		return StepInvocationDescriptor{}, fmt.Errorf("step %q references unknown symbol %q", node.GetId(), node.GetSymbolRef())
	}
	sourcePackage, ok := index.packagesByID[symbol.GetPackageId()]
	if !ok {
		return StepInvocationDescriptor{}, fmt.Errorf("step %q references unknown source package %q", node.GetId(), symbol.GetPackageId())
	}
	contract, ok := index.contractsByRef[node.GetContractRef()]
	if !ok {
		return StepInvocationDescriptor{}, fmt.Errorf("step %q references unknown contract %q", node.GetId(), node.GetContractRef())
	}

	return StepInvocationDescriptor{
		Kind:          "StepInvocationDescriptor",
		SchemaVersion: 0,
		Encoding:      "json-v0",
		PlanHash:      planHash,
		RunID:         argoRunIDVariable,
		NodeID:        node.GetId(),
		Attempt:       1,
		Symbol: stepSymbolRef{
			PackageID: symbol.GetPackageId(),
			Language:  symbol.GetLanguage(),
			Module:    symbol.GetModule(),
			Export:    symbol.GetExport(),
		},
		SourcePackage: sourcePackageRef{
			PackageID:   sourcePackage.GetPackageId(),
			Language:    sourcePackage.GetLanguage(),
			PackageHash: sourcePackage.GetPackageHash(),
			SourceArchive: artifactRef{
				Key:         sourcePackage.GetSourceArchive().GetKey(),
				Hash:        sourcePackage.GetSourceArchive().GetHash(),
				ContentType: sourcePackage.GetSourceArchive().GetContentType(),
			},
		},
		EnvironmentRef: contract.GetEnvironmentRef(),
		Input: dataArtifactRef{
			// The upstream output digest is only known at run time; WS-8 wires the
			// live hash. The key follows the datastore run layout, run-scoped by uid.
			Artifact: artifactRef{
				Key:         fmt.Sprintf("runs/%s/inputs/%s.json", argoRunIDVariable, node.GetId()),
				Hash:        "",
				ContentType: jsonContentType,
			},
			Schema: node.GetInputSchema(),
		},
		Output: dataArtifactDestination{
			Artifact: artifactDestination{
				Key:         fmt.Sprintf("runs/%s/steps/%s/1/output.json", argoRunIDVariable, node.GetId()),
				ContentType: jsonContentType,
			},
			Schema: node.GetOutputSchema(),
		},
		ChannelReads:  []channelArtifactRef{},
		ChannelWrites: []channelArtifactDestination{},
		Datastore: datastoreDescriptor{
			Kind: "local",
			Path: wedgeDatastorePath,
		},
	}, nil
}

// canonicalDescriptorJSON renders the descriptor as canonical JSON for embedding
// as the step's raw input artifact.
func canonicalDescriptorJSON(descriptor StepInvocationDescriptor) (string, error) {
	raw, err := marshalJSON(descriptor)
	if err != nil {
		return "", err
	}
	canonicalJSON, err := canonical.CanonicalizeJSON(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize step descriptor: %w", err)
	}
	return string(canonicalJSON), nil
}
