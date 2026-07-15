package plan

import (
	"bytes"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"google.golang.org/protobuf/encoding/protojson"
)

// ParseCanonicalJSON decodes a compiled plan's canonical JSON body (the exact
// bytes written to plans/<plan-key>/workflow.json) back into a typed plan. It is
// the inverse of MarshalCanonical for callers — the Argo step driver and target
// compilation — that receive a plan as datastore bytes rather than as a fresh
// compile.
func ParseCanonicalJSON(data []byte) (*planpb.WorkflowPlan, error) {
	var parsed planpb.WorkflowPlan
	if err := protojson.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse workflow plan JSON: %w", err)
	}
	return &parsed, nil
}

// VerifyCanonicalJSON checks that planJSON is a self-consistent compiled plan
// carrying planHash: the bytes are already canonical, their self-excluded digest
// equals planHash (the same hash rule the compiler applies in hashPlan), and the
// plan's own planHash field agrees. It returns the parsed plan so a caller that
// loads a plan from the datastore verifies and decodes in one step.
func VerifyCanonicalJSON(planJSON []byte, planHash string) (*planpb.WorkflowPlan, error) {
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
		return nil, err
	}
	if recomputed != planHash {
		return nil, fmt.Errorf("plan hash %q does not match the canonical plan JSON (recomputed %q)", planHash, recomputed)
	}
	if embedded := parsed.GetPlanHash(); embedded != planHash {
		return nil, fmt.Errorf("embedded plan.planHash %q does not match plan hash %q", embedded, planHash)
	}

	return parsed, nil
}
