package argo

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"google.golang.org/protobuf/proto"
)

func TestRetryPolicyLowersToRunnerTemplatesOnly(t *testing.T) {
	result := fixturePlan(t, "finite-map")
	for _, contract := range result.Plan.Contracts {
		contract.Retry = &planpb.RetryPolicy{
			MaxAttempts: proto.Uint32((4)), DelaySeconds: proto.Uint32((10)),
			BackoffFactor: proto.Uint32((3)), MaxDelaySeconds: proto.Uint32((120)),
		}
		contract.TimeoutSeconds = proto.Uint32((900))
	}
	templates := compiledTemplates(t, result.Plan)

	item := templateByName(t, templates, "map-item-map-items")
	want := map[string]any{
		"limit":       "3",
		"retryPolicy": "Always",
		"expression":  "!(lastRetry.exitCode in ['64', '65', '67'])",
		"backoff":     map[string]any{"duration": "10s", "factor": "3", "cap": "120s"},
	}
	if got := item["retryStrategy"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("map item retryStrategy = %#v, want %#v", got, want)
	}
	if args := item["container"].(map[string]any)["args"].([]any); !containsArgs(args, "--retry-count={{retries}}") {
		t.Fatalf("map item args = %v, want retry count", args)
	}
	if _, ok := item["activeDeadlineSeconds"]; ok {
		t.Fatal("per-attempt timeout must stay inside the runtime, not the pod deadline")
	}
	for _, name := range []string{"map-expand-map-items", "map-collect-map-items", "map-map-items", "main"} {
		if _, ok := templateByName(t, templates, name)["retryStrategy"]; ok {
			t.Fatalf("control template %s must not retry", name)
		}
	}
}

func TestZeroDelayRetryOmitsBackoffAndSingleAttemptOmitsStrategy(t *testing.T) {
	result := fixturePlan(t, "linear-chain")
	for _, contract := range result.Plan.Contracts {
		contract.Retry = &planpb.RetryPolicy{
			MaxAttempts: proto.Uint32((2)), DelaySeconds: proto.Uint32((0)),
			BackoffFactor: proto.Uint32((1)), MaxDelaySeconds: proto.Uint32((0)),
		}
	}
	step := stepTemplateWithArgs(t, compiledTemplates(t, result.Plan))
	strategy, ok := step["retryStrategy"].(map[string]any)
	if !ok || strategy["limit"] != "1" {
		t.Fatalf("retryStrategy = %#v, want one retry", step["retryStrategy"])
	}
	if _, ok := strategy["backoff"]; ok {
		t.Fatalf("zero-delay retry emitted backoff: %#v", strategy)
	}

	single := stepTemplateWithArgs(t, compiledTemplates(t, fixturePlan(t, "linear-chain").Plan))
	if _, ok := single["retryStrategy"]; ok {
		t.Fatal("contract without retry emitted a retryStrategy")
	}
	if containsArgs(single["container"].(map[string]any)["args"].([]any), "--retry-count={{retries}}") {
		t.Fatal("contract without retry passes a retry count")
	}
}

func compiledTemplates(t *testing.T, workflowPlan *planpb.WorkflowPlan) []any {
	t.Helper()
	canonicalPlan, _ := rehashPlan(t, workflowPlan)
	bundle, err := Compile(canonicalPlan, deploymentForPlan(t, canonicalPlan), runtimeAssetsForPlan(t, workflowPlan))
	if err != nil {
		t.Fatal(err)
	}
	var template map[string]any
	if err := json.Unmarshal(fileByPath(t, bundle, "workflow-template.json").Bytes, &template); err != nil {
		t.Fatal(err)
	}
	return template["spec"].(map[string]any)["templates"].([]any)
}

func stepTemplateWithArgs(t *testing.T, templates []any) map[string]any {
	t.Helper()
	for _, value := range templates {
		template := value.(map[string]any)
		container, ok := template["container"].(map[string]any)
		if ok && containsArgs(container["args"].([]any), "--node=double") {
			return template
		}
	}
	t.Fatal("missing runtime step template for double")
	return nil
}
