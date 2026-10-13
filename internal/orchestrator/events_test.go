package orchestrator

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/runevent"
	"github.com/Sly1029/massive/internal/runjournal"
)

// This subprocess completes successfully without publishing an output. The
// parent tests observation and rejected return-value handling, not settlement.
func TestRunEventsProcess(t *testing.T) {
	if os.Getenv("MASSIVE_EVENT_PROCESS") == "1" {
		os.Exit(0)
	}
}

func TestRunEventsCloseStartedAttemptsAfterRejectedBatch(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MASSIVE_EVENT_PROCESS", "1")
	for _, stepStatus := range []string{"", StatusFailed, StatusNotStarted, StatusCancelled} {
		t.Run("step status="+stepStatus, func(t *testing.T) {
			queue := make(chan runevent.Event, 32)
			planHash := processMapDescriptor("rejected", 0).PlanHash
			events := newRunEvents(RunConfig{Plan: &planpb.WorkflowPlan{PlanHash: &planHash}, Hooks: RunHooks{OnRunEvent: func(event runevent.Event) { queue <- event }}}, "project", "rejected", time.Now(), nil)
			steps := []StepInvocation{{Descriptor: processMapDescriptor("rejected", 0)}, {Descriptor: processMapDescriptor("rejected", 1)}}
			outcomes, err := (ProcessStepInvoker{CommandTemplate: []string{executable, "-test.run=^TestRunEventsProcess$", "--", descriptorPathToken}}).InvokeSteps(t.Context(), StepInvocationBatch{Steps: steps, OnStarted: events.attemptStarted})
			if err != nil {
				t.Fatal(err)
			}
			// Corrupt real returned identities, exercising rejection before any
			// journal items can be reconciled. Start observations remain open.
			outcomes[1] = outcomes[0]
			if _, err := mapOutcomesByIndex("map-items", 1, outcomes, []int{0, 1}, true); err == nil {
				t.Fatal("duplicate outcome accepted")
			}
			if stepStatus != "" {
				events.stepFinished(&runjournal.Step{NodeID: "map-items", Status: stepStatus})
			}
			wantAttemptStatus := StatusFailed
			if stepStatus == StatusCancelled {
				wantAttemptStatus = StatusCancelled
			}
			events.runFinished(StatusFailed)
			close(queue)
			starts, finishes := map[int]bool{}, map[int]bool{}
			terminal := false
			for event := range queue {
				body, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := runevent.Parse(body); err != nil {
					t.Fatalf("invalid event: %s: %v", body, err)
				}
				if terminal {
					t.Fatal("event emitted after run finish")
				}
				switch event.Type {
				case "step.attempt.started":
					starts[*event.ItemIndex] = true
				case "step.attempt.finished":
					index := *event.ItemIndex
					if !starts[index] || finishes[index] || event.Status != wantAttemptStatus {
						t.Fatalf("incorrect cleanup event: %+v", event)
					}
					finishes[index] = true
				case "step.finished", "run.finished":
					if len(finishes) != len(starts) {
						t.Fatal("terminal event preceded open attempt cleanup")
					}
					terminal = event.Type == "run.finished"
				}
			}
			if !terminal || len(starts) != 2 || len(finishes) != 2 {
				t.Fatalf("incomplete stream: starts=%v finishes=%v", starts, finishes)
			}
		})
	}
}
