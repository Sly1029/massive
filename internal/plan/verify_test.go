package plan

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/spec"
)

func TestVerifyCanonicalJSONAcceptsCompilerOutput(t *testing.T) {
	data := readFixture(t, "specs", "diamond", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}

	verified, err := VerifyCanonicalJSON(compiled.CanonicalJSON, compiled.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if verified.GetPlanHash() != compiled.PlanHash {
		t.Fatalf("verified plan hash = %q, want %q", verified.GetPlanHash(), compiled.PlanHash)
	}
}

func TestVerifyCanonicalJSONRejectsNonCanonicalBytes(t *testing.T) {
	data := readFixture(t, "specs", "passthrough", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonical := append([]byte(" \n"), compiled.CanonicalJSON...)

	_, err = VerifyCanonicalJSON(nonCanonical, compiled.PlanHash)
	if err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("VerifyCanonicalJSON() error = %v, want non-canonical rejection", err)
	}
}

func TestVerifyCanonicalJSONRejectsTamperingAndWrongExpectedIdentity(t *testing.T) {
	data := readFixture(t, "specs", "linear-chain", "workflow-spec.json")
	workflowSpec, err := spec.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(workflowSpec, data)
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Replace(compiled.CanonicalJSON, []byte(`"workflowName":"linear-chain"`), []byte(`"workflowName":"linear-chains"`), 1)
	if bytes.Equal(tampered, compiled.CanonicalJSON) {
		t.Fatal("test did not alter plan bytes")
	}
	if _, err := VerifyCanonicalJSON(tampered, compiled.PlanHash); err == nil || !strings.Contains(err.Error(), "does not match canonical plan content") {
		t.Fatalf("tampered plan error = %v, want hash mismatch", err)
	}

	wrongHash := "sha256:" + strings.Repeat("0", 64)
	if _, err := VerifyCanonicalJSON(compiled.CanonicalJSON, wrongHash); err == nil || !strings.Contains(err.Error(), "does not match canonical plan content") {
		t.Fatalf("wrong identity error = %v, want hash mismatch", err)
	}
}
