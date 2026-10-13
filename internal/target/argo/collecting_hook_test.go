package argo

import (
	"encoding/json"
	"testing"

	"github.com/Sly1029/massive/internal/deployment"
	"google.golang.org/protobuf/proto"
)

func TestCollectingMapPreservesPlacementDeadlineAndExitHook(t *testing.T) {
	compiled := exitHookPlan(t, "python-map-outcomes")
	compiled.Plan.Graph.DeadlineSeconds = proto.Uint32(30)
	data, _ := rehashPlan(t, compiled.Plan)
	binding := deploymentForPlan(t, data)
	binding.Profile.Target.Placement = &deployment.Placement{
		Defaults: &deployment.PodPlacement{PriorityClassName: "batch"},
		Nodes: map[string]deployment.PodPlacement{
			"scan":   {PriorityClassName: "items"},
			"notify": {PriorityClassName: "notifications"},
		},
	}
	binding, _, err := deployment.New(binding.PlanHash, binding.Profile, binding.MaterializationHash)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Compile(data, binding, runtimeAssetsForPlan(t, compiled.Plan))
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := json.Unmarshal(fileByPath(t, bundle, "workflow-template.json").Bytes, &workflow); err != nil {
		t.Fatal(err)
	}
	spec := workflow["spec"].(map[string]any)
	if spec["activeDeadlineSeconds"] != float64(30) || spec["onExit"] != exitHandlerName {
		t.Fatalf("workflow deadline/hook = %v", spec)
	}
	templates := spec["templates"].([]any)
	for name, priority := range map[string]string{
		"map-expand-scan": "batch", "map-item-scan": "items", "map-collect-scan": "batch", "exit-notify": "notifications",
	} {
		pod := templateByName(t, templates, name)
		if pod["priorityClassName"] != priority {
			t.Fatalf("%s placement = %v", name, pod)
		}
	}
	if templateByName(t, templates, "map-outcome-scan")["dag"] == nil {
		t.Fatal("collecting map lost its per-item DAG")
	}
	hook := templateByName(t, templates, "exit-notify")
	if hook["activeDeadlineSeconds"] != "300" || hook["retryStrategy"] != nil {
		t.Fatalf("hook lifetime = %v", hook)
	}
	// Collected failed attempts must never supply failed_node for a successful run.
	failures, _ := json.Marshal(`[{"displayName":"attempt(0)","templateName":"map-item-scan","finishedAt":"2026-01-01T00:00:01Z"}]`)
	status, node, err := ExitStatus(compiled.Plan, "Succeeded", string(failures))
	if err != nil || status != "succeeded" || node != nil {
		t.Fatalf("collected failures changed hook outcome: %s %v %v", status, node, err)
	}
}

func TestFatalWatcherUsesOnlyControlCredentialsAndGatesDispatch(t *testing.T) {
	compiled := exitHookPlan(t, "python-map-outcomes")
	templates := compiledTemplates(t, compiled.Plan)
	for _, name := range []string{"map-item-scan", "step-plan", "exit-notify"} {
		pod := templateByName(t, templates, name)
		if pod["automountServiceAccountToken"] == true {
			t.Fatalf("author pod received API token: %s", name)
		}
	}
}
