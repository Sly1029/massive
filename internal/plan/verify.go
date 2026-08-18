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
