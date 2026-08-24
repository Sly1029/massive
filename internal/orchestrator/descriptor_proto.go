package orchestrator

import (
	"fmt"
	"math"

	"github.com/Sly1029/massive/conformance/schema/runtimepb"
	"google.golang.org/protobuf/encoding/protojson"
)

// MarshalJSON makes the generated proto contract authoritative at the runner
// seam while retaining the smaller internal structs used by orchestration.
func (descriptor StepInvocationDescriptor) MarshalJSON() ([]byte, error) {
	message, err := descriptorToProto(descriptor)
	if err != nil {
		return nil, err
	}
	return (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(message)
}

func descriptorToProto(descriptor StepInvocationDescriptor) (*runtimepb.StepInvocationDescriptor, error) {
	if descriptor.Attempt < 0 || uint64(descriptor.Attempt) > math.MaxUint32 {
		return nil, fmt.Errorf("descriptor attempt %d exceeds the proto v2 uint32 range", descriptor.Attempt)
	}
	scope, err := executionScopeToProto(descriptor.Scope)
	if err != nil {
		return nil, err
	}
	datastore, err := datastoreToProto(descriptor.Datastore)
	if err != nil {
		return nil, err
	}
	channelReads := make([]*runtimepb.ChannelArtifactRef, 0, len(descriptor.ChannelReads))
	for _, channel := range descriptor.ChannelReads {
		channelReads = append(channelReads, &runtimepb.ChannelArtifactRef{
			ChannelName: pointer(channel.ChannelName),
			Artifact:    artifactRefToProto(channel.Artifact),
			Schema:      pointer(channel.Schema),
		})
	}
	channelWrites := make([]*runtimepb.ChannelArtifactDestination, 0, len(descriptor.ChannelWrites))
	for _, channel := range descriptor.ChannelWrites {
		channelWrites = append(channelWrites, &runtimepb.ChannelArtifactDestination{
			ChannelName: pointer(channel.ChannelName),
			Artifact: &runtimepb.ArtifactDestination{
				Key: pointer(channel.Artifact.Key), ContentType: pointer(channel.Artifact.ContentType),
			},
			Schema: pointer(channel.Schema),
		})
	}
	sourcePackage := &runtimepb.SourcePackage{
		PackageId:     pointer(descriptor.SourcePackage.PackageID),
		Language:      pointer(descriptor.SourcePackage.Language),
		PackageHash:   pointer(descriptor.SourcePackage.PackageHash),
		SourceArchive: artifactRefToProto(descriptor.SourcePackage.SourceArchive),
	}
	if descriptor.SourcePackage.Manifest != nil {
		sourcePackage.Manifest = artifactRefToProto(*descriptor.SourcePackage.Manifest)
	}
	return &runtimepb.StepInvocationDescriptor{
		Kind:          pointer(descriptor.Kind),
		SchemaVersion: pointer(descriptor.SchemaVersion),
		Encoding:      pointer(descriptor.Encoding),
		PlanHash:      pointer(descriptor.PlanHash),
		ProjectKey:    pointer(descriptor.ProjectKey),
		RunId:         pointer(descriptor.RunID),
		NodeId:        pointer(descriptor.NodeID),
		Attempt:       pointer(uint32(descriptor.Attempt)),
		Scope:         scope,
		Symbol: &runtimepb.StepSymbol{
			PackageId: pointer(descriptor.Symbol.PackageID),
			Language:  pointer(descriptor.Symbol.Language),
			Module:    pointer(descriptor.Symbol.Module),
			Export:    pointer(descriptor.Symbol.Export),
		},
		SourcePackage:  sourcePackage,
		EnvironmentRef: pointer(descriptor.EnvironmentRef),
		Input: &runtimepb.DataArtifactRef{
			Artifact: artifactRefToProto(descriptor.Input.Artifact),
			Schema:   pointer(descriptor.Input.Schema),
		},
		Output: &runtimepb.DataArtifactManifestDestination{
			ManifestKey: pointer(descriptor.Output.ManifestKey),
			Schema:      pointer(descriptor.Output.Schema),
		},
		ChannelReads:  channelReads,
		ChannelWrites: channelWrites,
		Datastore:     datastore,
	}, nil
}

func executionScopeToProto(scope *ExecutionScope) (*runtimepb.ExecutionScope, error) {
	if scope == nil {
		return nil, nil
	}
	frames := make([]*runtimepb.MapItemScopeFrame, 0, len(scope.Frames))
	for _, frame := range scope.Frames {
		if frame.Index < 0 || uint64(frame.Index) > math.MaxUint32 {
			return nil, fmt.Errorf("map index %d exceeds the proto v2 uint32 range", frame.Index)
		}
		frames = append(frames, &runtimepb.MapItemScopeFrame{
			Kind: pointer(frame.Kind), MapId: pointer(frame.MapID), Index: pointer(uint32(frame.Index)),
		})
	}
	return &runtimepb.ExecutionScope{Frames: frames}, nil
}

func datastoreToProto(datastore DatastoreDescriptor) (*runtimepb.Datastore, error) {
	switch value := datastore.(type) {
	case LocalDatastoreDescriptor:
		return &runtimepb.Datastore{Kind: pointer(value.Kind), Path: pointer(value.Path)}, nil
	case S3DatastoreDescriptor:
		message := &runtimepb.Datastore{
			Kind: pointer(value.Kind), Bucket: pointer(value.Bucket), Region: pointer(value.Region),
		}
		if value.Prefix != "" {
			message.Prefix = pointer(value.Prefix)
		}
		if value.Endpoint != "" {
			message.Endpoint = pointer(value.Endpoint)
		}
		message.ForcePathStyle = value.ForcePathStyle
		return message, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported descriptor datastore type %T", datastore)
	}
}

func artifactRefToProto(artifact ArtifactRef) *runtimepb.ArtifactRef {
	return &runtimepb.ArtifactRef{
		Key: pointer(artifact.Key), Hash: pointer(artifact.Hash), ContentType: pointer(artifact.ContentType),
	}
}

func pointer[T any](value T) *T {
	return &value
}
