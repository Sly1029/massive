package argo

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestMissingOutcomeRequiresTerminalProcessDeathEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		reason     string
		signal     int32
		exit       int32
		message    string
		podReason  string
		wantKilled bool
	}{
		{name: "OOM", reason: "OOMKilled", exit: 137, wantKilled: true},
		{name: "signal", reason: "Error", signal: 9, exit: 137, wantKilled: true},
		{name: "ordinary exit 200", reason: "Error", exit: 200},
		{name: "ordinary exit 137", reason: "Error", exit: 137},
		{name: "contract exit", reason: "Error", exit: 64},
		{name: "executor output extraction", reason: "Completed", exit: 0},
		{name: "fatal before signal", reason: "Error", signal: 9, message: `{"fatal":true}`},
		{name: "eviction", reason: "Error", signal: 9, podReason: "Evicted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow := itemSchedulerWorkflow{}
			workflow.Metadata.UID = "workflow-uid"
			workflow.Status.Nodes = map[string]itemSchedulerNode{
				"attempt": {Type: "Pod", Phase: "Error"},
				"retry":   {Type: "Retry", Phase: "Error", Children: []string{"attempt"}},
			}
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels:          map[string]string{"workflows.argoproj.io/node-id": "attempt"},
					OwnerReferences: []metav1.OwnerReference{{Kind: "Workflow", UID: types.UID("workflow-uid")}},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed, Reason: test.podReason,
					ContainerStatuses: []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: test.reason, Signal: test.signal, ExitCode: test.exit, Message: test.message,
					}}}},
				},
			}
			failure, err := missingItemFailure(workflow, []corev1.Pod{pod}, "attempt")
			if test.wantKilled {
				if err != nil || failure.Kind != "killed" || failure.Attempts != 1 {
					t.Fatalf("failure = %+v, error = %v", failure, err)
				}
				// The contract may allow more attempts; only executed children count.
				workflow.Status.Nodes["earlier"] = itemSchedulerNode{Type: "Pod", Phase: "Failed"}
				workflow.Status.Nodes["retry"] = itemSchedulerNode{Type: "Retry", Phase: "Failed", Children: []string{"earlier", "attempt"}}
				failure, err = missingItemFailure(workflow, []corev1.Pod{pod}, "attempt")
				if err != nil || failure.Attempts != 2 {
					t.Fatalf("retry attempts = %d, error = %v", failure.Attempts, err)
				}
			} else if err == nil {
				t.Fatalf("collected infrastructure/ordinary exit as %+v", failure)
			}
		})
	}
}
