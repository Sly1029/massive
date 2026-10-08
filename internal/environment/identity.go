package environment

import (
	"fmt"

	pb "github.com/Sly1029/massive/conformance/schema/materializationpb"
	"github.com/Sly1029/massive/internal/canonical"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// MaterializerName identifies preflight of an existing Python environment.
	// Its version is the control plane release.
	MaterializerName = "massive-existing-python"
	// RecordContentType is the media type of a stored RealizedEnvironment.
	RecordContentType   = "application/vnd.massive.realized-environment+json"
	recordSchemaVersion = 1
)

// RequirementHash identifies dependency inputs: recipe "python-requirement" v1.
// A lock defines the dependency set, so direct dependencies are hashed only
// without one.
func RequirementHash(requirement *pb.PythonRequirement) (string, error) {
	dependencies := requirement.GetDependencies()
	if dependencies == nil {
		dependencies = []string{}
	}
	return digest(map[string]any{
		"recipe": "python-requirement", "recipeVersion": 1,
		"requiresPython": optional(requirement.RequiresPython),
		"lockHash":       optional(requirement.LockHash),
		"dependencies":   dependencies,
	})
}

// RealizationHash identifies what is installed: recipe "existing-python" v1.
// It excludes requirement inputs so equivalent environments share an identity.
func RealizationHash(realization *pb.ExistingPythonRealization) (string, error) {
	distributions := make([]any, 0, len(realization.GetDistributions()))
	for _, distribution := range realization.GetDistributions() {
		distributions = append(distributions, map[string]any{
			"name": distribution.GetName(), "version": distribution.GetVersion(),
			"direct": distribution.GetDirect(), "editable": distribution.GetEditable(),
		})
	}
	return digest(map[string]any{
		"recipe": "existing-python", "recipeVersion": 1,
		"implementation":      realization.GetImplementation(),
		"pythonVersion":       realization.GetPythonVersion(),
		"cacheTag":            optional(realization.CacheTag),
		"sysconfigPlatform":   realization.GetSysconfigPlatform(),
		"os":                  realization.GetOs(),
		"arch":                realization.GetArch(),
		"distributions":       distributions,
		"materializerName":    realization.GetMaterializerName(),
		"materializerVersion": realization.GetMaterializerVersion(),
		"verification":        realization.GetVerification().String(),
	})
}

func newRecord(report *Report, lockHash *string, version string) (*pb.RealizedEnvironment, error) {
	requirement := &pb.PythonRequirement{LockHash: lockHash}
	if report.Project != nil {
		requirement.RequiresPython = report.Project.RequiresPython
		if lockHash == nil {
			requirement.Dependencies = report.Project.Dependencies
		}
	}
	verification := pb.PythonVerification(pb.PythonVerification_value[string(report.Verification)])
	interpreter := report.Interpreter
	realization := &pb.ExistingPythonRealization{
		Implementation: proto.String(interpreter.Implementation), PythonVersion: proto.String(interpreter.Version),
		CacheTag: interpreter.CacheTag, SysconfigPlatform: proto.String(interpreter.Platform),
		Os: proto.String(interpreter.OS), Arch: proto.String(interpreter.Arch),
		MaterializerName: proto.String(MaterializerName), MaterializerVersion: proto.String(version),
		Verification: verification.Enum(),
	}
	for _, distribution := range report.Distributions {
		realization.Distributions = append(realization.Distributions, &pb.InstalledDistribution{
			Name: proto.String(distribution.Name), Version: proto.String(distribution.Version),
			Direct: proto.Bool(distribution.Direct), Editable: proto.Bool(distribution.Editable),
		})
	}
	requirementHash, err := RequirementHash(requirement)
	if err != nil {
		return nil, err
	}
	realizationHash, err := RealizationHash(realization)
	if err != nil {
		return nil, err
	}
	return &pb.RealizedEnvironment{
		SchemaVersion: proto.Uint32(recordSchemaVersion), RequirementHash: proto.String(requirementHash),
		RealizationHash: proto.String(realizationHash), Requirement: requirement, Realization: realization,
	}, nil
}

// MarshalRecord returns the canonical protobuf JSON bytes that are stored and
// content-addressed.
func MarshalRecord(record *pb.RealizedEnvironment) ([]byte, error) {
	data, err := protojson.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode realized environment: %w", err)
	}
	return canonical.CanonicalizeJSON(data)
}

// ParseRecord accepts only a current record whose identities match its contents.
func ParseRecord(data []byte) (*pb.RealizedEnvironment, error) {
	var record pb.RealizedEnvironment
	if err := protojson.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("invalid realized environment record: %w", err)
	}
	if record.GetSchemaVersion() != recordSchemaVersion || record.Requirement == nil || record.Realization == nil ||
		record.GetRealization().GetVerification() == pb.PythonVerification_PYTHON_VERIFICATION_UNSPECIFIED {
		return nil, fmt.Errorf("unsupported realized environment record; create a new run with the current massive release")
	}
	requirementHash, err := RequirementHash(record.Requirement)
	if err != nil {
		return nil, err
	}
	realizationHash, err := RealizationHash(record.Realization)
	if err != nil {
		return nil, err
	}
	if requirementHash != record.GetRequirementHash() || realizationHash != record.GetRealizationHash() {
		return nil, fmt.Errorf("realized environment record identities do not match its contents")
	}
	return &record, nil
}

func digest(fields map[string]any) (string, error) {
	data, err := canonical.Marshal(fields)
	if err != nil {
		return "", err
	}
	return canonical.DigestBytes(data), nil
}

func optional(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
