package plan

import (
	"bytes"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"google.golang.org/protobuf/encoding/protojson"
)

// ParseCanonicalJSON decodes the canonical JSON projection of a WorkflowPlan.
func ParseCanonicalJSON(data []byte) (*planpb.WorkflowPlan, error) {
	var parsed planpb.WorkflowPlan
	if err := protojson.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse workflow plan JSON: %w", err)
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
	if plan.SchemaVersion == nil || plan.GetSchemaVersion() != 0 {
		return fmt.Errorf("workflow plan schemaVersion must be present and equal 0")
	}
	if err := validateHashingSpec(plan.GetHashing(), "workflow-plan", "workflow plan"); err != nil {
		return err
	}
	if plan.Graph == nil || plan.Graph.IrVersion == nil || plan.Graph.GetIrVersion() == "" {
		return fmt.Errorf("workflow plan graph.irVersion must be present")
	}
	if plan.Provenance == nil || plan.Provenance.CompilerVersion == nil || plan.Provenance.GetCompilerVersion() == "" {
		return fmt.Errorf("workflow plan provenance.compilerVersion must be present")
	}
	for index, sourcePackage := range plan.SourcePackages {
		if err := validateHashingSpec(sourcePackage.GetHashing(), "source-package", fmt.Sprintf("workflow plan sourcePackages[%d]", index)); err != nil {
			return err
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
