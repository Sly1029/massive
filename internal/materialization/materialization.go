// Package materialization binds portable source bytes and externally supplied
// immutable images to plan requirements. It performs no network or filesystem IO.
package materialization

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"

	pb "github.com/Sly1029/massive/conformance/schema/materializationpb"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/containerimage"
	"github.com/Sly1029/massive/internal/sourceidentity"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	Name    = "massive-existing-container"
	Version = "1"
)

// Same structural platform grammar as WorkflowSpec; platform availability is
// intentionally not inferred from a registry or a host-specific OS/arch list.
var platformPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)

// ForPlan prepares portable inputs for existing pinned container requirements.
// Never mutate or rehash a plan to attach materialization results.
func ForPlan(plan *planpb.WorkflowPlan, archives map[string][]byte) (*pb.MaterializationSpec, error) {
	if plan == nil {
		return nil, fmt.Errorf("materialization: workflow plan is required")
	}
	result := &pb.MaterializationSpec{SchemaVersion: proto.Uint32(0)}
	environments := map[string]bool{}
	for _, environment := range plan.GetEnvironments() {
		ref := environment.GetEnvRef()
		container := environment.GetContainer()
		if container == nil {
			return nil, fmt.Errorf("materialization: environment %s is not a container requirement; existing-container materialization does not support Node requirements", ref)
		}
		image := container.GetImage()
		if err := containerimage.ValidatePinned(image); err != nil {
			return nil, fmt.Errorf("materialization: environment %s: %w", ref, err)
		}
		if !platformPattern.MatchString(container.GetPlatform()) {
			return nil, fmt.Errorf("materialization: environment %s requires an os/architecture platform", ref)
		}
		if !canonical.IsSHA256Ref(ref) {
			return nil, fmt.Errorf("materialization: invalid environment reference %q", ref)
		}
		if environments[ref] {
			return nil, fmt.Errorf("materialization: duplicate environment requirement %s", ref)
		}
		environments[ref] = true
		result.Environments = append(result.Environments, &pb.EnvironmentSelection{
			EnvironmentRef: proto.String(ref),
			Mode: &pb.EnvironmentSelection_ExistingContainer{ExistingContainer: &pb.ExistingContainer{
				Image: proto.String(image), Platform: proto.String(container.GetPlatform()),
			}},
		})
	}
	sort.Slice(result.Environments, func(i, j int) bool {
		return canonical.LessUTF16(result.Environments[i].GetEnvironmentRef(), result.Environments[j].GetEnvironmentRef())
	})
	packages := map[string]bool{}
	for _, source := range plan.GetSourcePackages() {
		hash := source.GetPackageHash()
		if packages[hash] {
			continue
		}
		packages[hash] = true
		archive, exists := archives[hash]
		if !exists {
			return nil, fmt.Errorf("materialization: source archive %s is required", hash)
		}
		if err := sourceidentity.VerifyArchive(archive, hash); err != nil {
			return nil, fmt.Errorf("materialization: source %s: %w", hash, err)
		}
		result.SourceArchives = append(result.SourceArchives, &pb.SourceArchive{
			PackageHash: proto.String(hash), ArchiveHash: proto.String(canonical.DigestBytes(archive)),
		})
	}
	if len(archives) != len(packages) {
		return nil, fmt.Errorf("materialization: source archives must exactly match the plan's packages")
	}
	sort.Slice(result.SourceArchives, func(i, j int) bool {
		return canonical.LessUTF16(result.SourceArchives[i].GetPackageHash(), result.SourceArchives[j].GetPackageHash())
	})
	data, err := MarshalCanonical(result)
	if err != nil {
		return nil, err
	}
	result.SpecHash = proto.String(canonical.DigestBytes(data))
	return result, nil
}

// Resolve authenticates the supplied spec against the plan and actual archive
// bytes, then records what was checked. A client manifest is never trusted as
// proof. Existing image declarations must match the pinned plan requirements;
// replacing a workflow image requires recompiling the workflow.
func Resolve(plan *planpb.WorkflowPlan, specJSON []byte, archives map[string][]byte) (*pb.MaterializationManifest, error) {
	var supplied pb.MaterializationSpec
	if err := protojson.Unmarshal(specJSON, &supplied); err != nil {
		return nil, fmt.Errorf("materialization: parse spec: %w", err)
	}
	expected, err := ForPlan(plan, archives)
	if err != nil {
		return nil, err
	}
	// Exact message equality validates version/presence, self-hash, ordering,
	// duplicate/missing/extra entries, selection modes and archive byte digests.
	// There is currently only one permitted realization of a pinned requirement.
	if !proto.Equal(&supplied, expected) {
		return nil, fmt.Errorf("materialization: spec does not match plan requirements and source archive bytes (expected specHash %s)", expected.GetSpecHash())
	}
	projection, err := canonical.CanonicalizeJSON(specJSON)
	if err != nil {
		return nil, err
	}
	expectedJSON, err := MarshalCanonical(expected)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(projection, expectedJSON) {
		return nil, fmt.Errorf("materialization: spec must use the canonical protobuf JSON projection")
	}
	manifest := &pb.MaterializationManifest{
		SchemaVersion: proto.Uint32(0), SpecHash: proto.String(expected.GetSpecHash()),
		SourceArchives:   expected.SourceArchives,
		MaterializerName: proto.String(Name), MaterializerVersion: proto.String(Version),
	}
	for _, selection := range expected.GetEnvironments() {
		container := selection.GetExistingContainer()
		// Execution command, source, namespace and service account are not
		// environment build inputs. Keep them out of this reusable identity.
		identity, err := canonical.Marshal(map[string]any{
			"recipe": "existing-container", "recipeVersion": 1,
			"image": container.GetImage(), "platform": container.GetPlatform(),
			"materializerName": Name, "materializerVersion": Version,
			"verification": "PINNED_REFERENCE_ONLY",
		})
		if err != nil {
			return nil, err
		}
		manifest.Environments = append(manifest.Environments, &pb.EnvironmentRealization{
			EnvironmentRef:    proto.String(selection.GetEnvironmentRef()),
			RealizationHash:   proto.String(canonical.DigestBytes(identity)),
			ExistingContainer: container,
			Verification:      pb.ContainerVerification_PINNED_REFERENCE_ONLY.Enum(),
		})
	}
	data, err := MarshalCanonical(manifest)
	if err != nil {
		return nil, err
	}
	manifest.ManifestHash = proto.String(canonical.DigestBytes(data))
	return manifest, nil
}

// MarshalCanonical uses the same explicit-presence proto JSON conventions as
// plans. Schema version 0 fixes each message's self-hash recipe.
func MarshalCanonical(message proto.Message) ([]byte, error) {
	data, err := protojson.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("materialization: encode protobuf JSON: %w", err)
	}
	return canonical.CanonicalizeJSON(data)
}
