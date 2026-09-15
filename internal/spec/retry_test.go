package spec

import (
	"strings"
	"testing"
)

func TestParseAcceptsRetryAndTimeoutContract(t *testing.T) {
	data := mutateValidFixture(t, "linear-chain", func(root map[string]any) {
		for _, contract := range root["contracts"].(map[string]any) {
			contract.(map[string]any)["retry"] = map[string]any{"maxAttempts": 3, "delaySeconds": 5, "backoffFactor": 2, "maxDelaySeconds": 60}
			contract.(map[string]any)["timeoutSeconds"] = 900
		}
	})

	workflowSpec, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range workflowSpec.Contracts {
		if contract.Retry == nil || contract.Retry.MaxAttempts != 3 || contract.Retry.MaxDelaySeconds != 60 || contract.TimeoutSeconds != 900 {
			t.Fatalf("contract = %#v, want parsed retry and timeout", contract)
		}
	}
}

func TestParseRejectsRetryDelayAboveCap(t *testing.T) {
	data := mutateValidFixture(t, "linear-chain", func(root map[string]any) {
		for _, contract := range root["contracts"].(map[string]any) {
			contract.(map[string]any)["retry"] = map[string]any{"maxAttempts": 3, "delaySeconds": 30, "backoffFactor": 2, "maxDelaySeconds": 10}
		}
	})

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected retry delay above its cap to be rejected")
	}
	diagnostics := diagnosticsFromError(t, err)
	if !containsDiagnostic(diagnostics, "retry maxDelaySeconds must be at least delaySeconds") || !strings.HasSuffix(diagnostics[0].Path, ".retry.maxDelaySeconds") {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
}

func TestParseRejectsRetryOutsideSchemaBounds(t *testing.T) {
	for name, retry := range map[string]map[string]any{
		"zero attempts":   {"maxAttempts": 0, "delaySeconds": 0, "backoffFactor": 1, "maxDelaySeconds": 0},
		"huge attempts":   {"maxAttempts": 101, "delaySeconds": 0, "backoffFactor": 1, "maxDelaySeconds": 0},
		"missing backoff": {"maxAttempts": 2, "delaySeconds": 0, "maxDelaySeconds": 0},
		"unknown field":   {"maxAttempts": 2, "delaySeconds": 0, "backoffFactor": 1, "maxDelaySeconds": 0, "jitter": true},
	} {
		t.Run(name, func(t *testing.T) {
			data := mutateValidFixture(t, "linear-chain", func(root map[string]any) {
				for _, contract := range root["contracts"].(map[string]any) {
					contract.(map[string]any)["retry"] = retry
				}
			})
			if _, err := Parse(data); err == nil {
				t.Fatal("invalid retry policy accepted")
			}
		})
	}
	data := mutateValidFixture(t, "linear-chain", func(root map[string]any) {
		for _, contract := range root["contracts"].(map[string]any) {
			contract.(map[string]any)["timeoutSeconds"] = 0
		}
	})
	if _, err := Parse(data); err == nil {
		t.Fatal("zero timeoutSeconds accepted")
	}
}
