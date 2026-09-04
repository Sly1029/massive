package plan

import (
	"bytes"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ParseCanonicalJSON decodes the canonical JSON projection of a WorkflowPlan.
func ParseCanonicalJSON(data []byte) (*planpb.WorkflowPlan, error) {
	var parsed planpb.WorkflowPlan
	if err := protojson.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse workflow plan schema v1 JSON (rebuild older plans): %w", err)
	}
	return &parsed, nil
}

// VerifyCanonicalJSON verifies the byte representation, the caller's expected
// identity, and the identity embedded in the plan before returning typed data.
// Target compilers and remote drivers should use this at their trust boundary.
func VerifyCanonicalJSON(planJSON []byte, expectedHash string) (*planpb.WorkflowPlan, error) {
	canonicalJSON, err := canonical.CanonicalizeJSON(planJSON)
	if err != nil {
		return nil, fmt.Errorf("plan JSON is not valid JSON: %w", err)
	}
	if !bytes.Equal(canonicalJSON, planJSON) {
		return nil, fmt.Errorf("plan JSON is not canonical")
	}

	parsed, err := ParseCanonicalJSON(planJSON)
	if err != nil {
		return nil, err
	}
	if err := validateVersionedIdentity(parsed); err != nil {
		return nil, err
	}
	recomputed, err := hashPlan(parsed)
	if err != nil {
		return nil, fmt.Errorf("recompute workflow plan hash: %w", err)
	}
	if recomputed != expectedHash {
		return nil, fmt.Errorf("expected plan hash %q does not match canonical plan content %q", expectedHash, recomputed)
	}
	if parsed.GetPlanHash() != expectedHash {
		return nil, fmt.Errorf("embedded plan hash %q does not match expected plan hash %q", parsed.GetPlanHash(), expectedHash)
	}
	return parsed, nil
}

func validateVersionedIdentity(plan *planpb.WorkflowPlan) error {
	if plan.SchemaVersion == nil || plan.GetSchemaVersion() != 1 {
		return fmt.Errorf("workflow plan schemaVersion must be present and equal 1; rebuild older plans")
	}
	if plan.SpecHash == nil {
		return fmt.Errorf("workflow plan specHash must be present")
	}
	if !canonical.IsSHA256Ref(plan.GetSpecHash()) {
		return fmt.Errorf("workflow plan specHash must be sha256:<64 lowercase hex>")
	}
	if err := validateHashingSpec(plan.GetHashing(), "workflow-plan", "workflow plan"); err != nil {
		return err
	}
	if err := validateHashingSpec(plan.GetSpecHashing(), "workflow-spec", "workflow plan specHash"); err != nil {
		return err
	}
	if plan.Graph == nil || plan.Graph.IrVersion == nil || plan.Graph.GetIrVersion() == "" {
		return fmt.Errorf("workflow plan graph.irVersion must be present")
	}
	if plan.Provenance == nil || plan.Provenance.CompilerName == nil || plan.Provenance.GetCompilerName() == "" || plan.Provenance.CompilerVersion == nil || plan.Provenance.GetCompilerVersion() == "" || plan.Provenance.SourceSpecHash == nil {
		return fmt.Errorf("workflow plan provenance compiler name, version, and source spec hash must be present")
	}
	if plan.Provenance.GetSourceSpecHash() != plan.GetSpecHash() {
		return fmt.Errorf("workflow plan provenance sourceSpecHash must equal specHash")
	}
	for index, sourcePackage := range plan.SourcePackages {
		if err := validateHashingSpec(sourcePackage.GetHashing(), "source-package", fmt.Sprintf("workflow plan sourcePackages[%d]", index)); err != nil {
			return err
		}
		if sourcePackage.PackageHash == nil || !canonical.IsSHA256Ref(sourcePackage.GetPackageHash()) {
			return fmt.Errorf("workflow plan sourcePackages[%d].packageHash must be sha256:<64 lowercase hex>", index)
		}
	}
	environments := map[string]bool{}
	for _, requirement := range plan.GetEnvironments() {
		ref := requirement.GetEnvRef()
		if requirement.Runtime == nil || !canonical.IsSHA256Ref(ref) || environments[ref] {
			return fmt.Errorf("workflow plan requires unique, typed environment requirements with sha256 references")
		}
		environments[ref] = true
		input := proto.Clone(requirement).(*planpb.EnvironmentRequirement)
		input.EnvRef = nil
		hash, err := hashPlanMessage(input)
		if err != nil {
			return err
		}
		if hash != ref {
			return fmt.Errorf("workflow plan environment reference %s does not match requirement %s", ref, hash)
		}
	}
	return nil
}

func validateHashingSpec(hashing *planpb.HashingSpec, recipe string, subject string) error {
	if hashing == nil || hashing.Algorithm == nil || hashing.Canonicalization == nil || hashing.Recipe == nil || hashing.RecipeVersion == nil {
		return fmt.Errorf("%s hashing descriptor must be present and complete", subject)
	}
	if hashing.GetAlgorithm() != "sha256" || hashing.GetCanonicalization() != "canonical-json-v0" || hashing.GetRecipe() != recipe || hashing.GetRecipeVersion() != 1 {
		return fmt.Errorf("%s hashing descriptor must be sha256/canonical-json-v0/%s@1", subject, recipe)
	}
	return nil
}
