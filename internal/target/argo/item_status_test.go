package argo

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// This shape follows Argo v3.7.16 workflowpod.go and kubectl pod status:
// node-id is an annotation, workflow/completed are labels, and main/wait
// have separate termination statuses. Only main's evidence classifies an item.
const terminalItemPodJSON = `{
 "apiVersion":"v1","kind":"Pod",
 "metadata":{
  "name":"workflow-map-item-123","namespace":"argo",
  "labels":{"workflows.argoproj.io/workflow":"workflow","workflows.argoproj.io/completed":"true"},
  "annotations":{"workflows.argoproj.io/node-id":"attempt","workflows.argoproj.io/node-name":"workflow.map-item(0)"},
  "ownerReferences":[{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","name":"workflow","uid":"workflow-uid","controller":true}]
 },
 "spec":{"containers":[{"name":"main","image":"runner","terminationMessagePath":"/dev/termination-log","terminationMessagePolicy":"File"}]},
 "status":{"phase":"Failed","containerStatuses":[
  {"name":"wait","state":{"terminated":{"exitCode":0,"reason":"Completed"}}},
  {"name":"main","state":{"terminated":{"exitCode":137,"reason":"OOMKilled","message":"{\"phase\":\"author\"}"}}}
 ]}
}`

func TestMissingOutcomeRequiresTerminalAuthorProcessDeathEvidence(t *testing.T) {
	for _, test := range []struct {
		name           string
		reason         string
		signal         int32
		exit           int32
		message        string
		podReason      string
		wantDiagnostic string
		wantKilled     bool
	}{
		{name: "OOM in author", reason: "OOMKilled", exit: 137, message: `{"phase":"author"}`, wantKilled: true},
		{name: "signal in author", reason: "Error", signal: 9, exit: 137, message: `{"phase":"author"}`, wantKilled: true},
		{name: "OOM hydrating", reason: "OOMKilled", exit: 137, message: `{"phase":"hydrate"}`, wantDiagnostic: "runtime phase hydrate"},
		{name: "OOM publishing", reason: "OOMKilled", exit: 137, message: `{"phase":"publish"}`, wantDiagnostic: "runtime phase publish"},
		{name: "OOM without evidence", reason: "OOMKilled", exit: 137, wantDiagnostic: "absent or invalid"},
		{name: "signal with malformed evidence", reason: "Error", signal: 9, message: `{"phase":`, wantDiagnostic: "absent or invalid"},
		{name: "unknown phase", reason: "OOMKilled", exit: 137, message: `{"phase":"setup"}`, wantDiagnostic: "absent or invalid"},
		{name: "null phase", reason: "OOMKilled", exit: 137, message: `{"phase":null}`, wantDiagnostic: "absent or invalid"},
		{name: "mixed evidence", reason: "OOMKilled", exit: 137, message: `{"phase":"author","fatal":true}`, wantDiagnostic: "absent or invalid"},
		{name: "oversized evidence", reason: "OOMKilled", exit: 137, message: `{"phase":"author"}` + strings.Repeat(" ", 33), wantDiagnostic: "exceeds 32 bytes"},
		{name: "ordinary exit 200", reason: "Error", exit: 200, message: `{"phase":"author"}`, wantDiagnostic: "without OOM or signal"},
		{name: "ordinary exit 137", reason: "Error", exit: 137, message: `{"phase":"author"}`, wantDiagnostic: "without OOM or signal"},
		{name: "contract exit", reason: "Error", exit: 64, message: `{"phase":"hydrate"}`, wantDiagnostic: "runtime phase hydrate"},
		{name: "executor output extraction", reason: "Completed", exit: 0, message: `{"phase":"publish"}`, wantDiagnostic: "runtime phase publish"},
		{name: "fatal before signal", reason: "Error", signal: 9, message: `{"fatal":true}`, wantDiagnostic: "fatal infrastructure or contract"},
		{name: "eviction", reason: "Error", signal: 9, podReason: "Evicted", message: `{"phase":"author"}`, wantDiagnostic: "pod failed without evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow := itemSchedulerWorkflow{}
			workflow.Metadata.UID = "workflow-uid"
			workflow.Status.Nodes = map[string]itemSchedulerNode{
				"attempt": {Type: "Pod", Phase: "Error"},
				"retry":   {Type: "Retry", Phase: "Error", Children: []string{"attempt"}},
			}
			var pod corev1.Pod
			if err := json.Unmarshal([]byte(terminalItemPodJSON), &pod); err != nil {
				t.Fatal(err)
			}
			termination := pod.Status.ContainerStatuses[1].State.Terminated
			termination.Reason, termination.Signal, termination.ExitCode, termination.Message = test.reason, test.signal, test.exit, test.message
			pod.Status.Reason = test.podReason
			failure, err := missingItemFailure(workflow, []corev1.Pod{pod}, "attempt")
			if test.wantKilled {
				if err != nil || failure.Kind != "killed" || failure.Attempts != 1 || !strings.Contains(failure.Diagnostic, "Argo author-phase reconciliation") {
					t.Fatalf("failure = %+v, error = %v", failure, err)
				}
				workflow.Status.Nodes["earlier"] = itemSchedulerNode{Type: "Pod", Phase: "Failed"}
				workflow.Status.Nodes["retry"] = itemSchedulerNode{Type: "Retry", Phase: "Failed", Children: []string{"earlier", "attempt"}}
				failure, err = missingItemFailure(workflow, []corev1.Pod{pod}, "attempt")
				if err != nil || failure.Attempts != 2 {
					t.Fatalf("retry attempts = %d, error = %v", failure.Attempts, err)
				}
				pod.Annotations = nil
				pod.Labels["workflows.argoproj.io/node-id"] = "attempt"
				if _, err := missingItemFailure(workflow, []corev1.Pod{pod}, "attempt"); err == nil || !strings.Contains(err.Error(), "terminal pod is unavailable") {
					t.Fatalf("fabricated node-id label accepted: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantDiagnostic) {
				t.Fatalf("failure = %+v, error = %v, want %q", failure, err, test.wantDiagnostic)
			}
		})
	}
}

func TestMissingOutcomeRejectsTerminationLogFallback(t *testing.T) {
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(terminalItemPodJSON), &pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	workflow := itemSchedulerWorkflow{}
	workflow.Metadata.UID = "workflow-uid"
	workflow.Status.Nodes = map[string]itemSchedulerNode{"attempt": {Type: "Pod", Phase: "Failed"}}
	if _, err := missingItemFailure(workflow, []corev1.Pod{pod}, "attempt"); err == nil || !strings.Contains(err.Error(), "terminationMessagePolicy File") {
		t.Fatalf("log fallback accepted as execution evidence: %v", err)
	}
}
