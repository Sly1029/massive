package orchestrator

import (
	"strings"
	"sync"
	"time"

	"github.com/Sly1029/massive/internal/runevent"
	"github.com/Sly1029/massive/internal/runjournal"
)

type eventIdentity struct {
	node    string
	item    int
	attempt int
}

// runEvents holds only monotonic clocks and event ordering. Execution state and
// outcomes belong to the journal and the orchestrator, never to observers.
type runEvents struct {
	mu                sync.Mutex
	hook              func(runevent.Event)
	projectKey, runID string
	started           time.Time
	sequence          int
	attemptStarts     map[eventIdentity]time.Time
	stepStarts        map[string]time.Time
}

func newRunEvents(config RunConfig, projectKey, runID string, started time.Time, totals map[string]int) *runEvents {
	if config.Hooks.OnRunEvent == nil {
		return nil
	}
	events := &runEvents{hook: config.Hooks.OnRunEvent, projectKey: projectKey, runID: runID, started: started, attemptStarts: make(map[eventIdentity]time.Time), stepStarts: make(map[string]time.Time)}
	events.emit(runevent.Event{Type: "run.started", PlanHash: config.Plan.GetPlanHash(), MapTotals: totals})
	return events
}

// emit is called with mu held after run.started. Hooks only enqueue; they must
// never do I/O or mutate execution state.
func (events *runEvents) emit(event runevent.Event) {
	events.sequence++
	event.SchemaVersion, event.Sequence = runevent.Version, events.sequence
	event.ProjectKey, event.RunID = events.projectKey, events.runID
	event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	event.ElapsedMS = float64(time.Since(events.started)) / float64(time.Millisecond)
	events.hook(event)
}

func invocationEventIdentity(descriptor StepInvocationDescriptor) (eventIdentity, *int) {
	identity := eventIdentity{node: descriptor.NodeID, item: -1, attempt: descriptor.Attempt}
	var item *int
	if descriptor.Scope != nil {
		index := descriptor.Scope.Frames[0].Index
		identity.item, item = index, &index
	}
	return identity, item
}

func (events *runEvents) attemptStarted(descriptor StepInvocationDescriptor) {
	if events == nil {
		return
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	identity, item := invocationEventIdentity(descriptor)
	events.startAttempt(identity, item)
}

func (events *runEvents) startAttempt(identity eventIdentity, item *int) {
	if _, exists := events.attemptStarts[identity]; exists {
		return
	}
	now := time.Now()
	events.attemptStarts[identity] = now
	if _, exists := events.stepStarts[identity.node]; !exists {
		events.stepStarts[identity.node] = now
	}
	events.emit(runevent.Event{Type: "step.attempt.started", NodeID: identity.node, ItemIndex: item, Attempt: identity.attempt})
}

// attemptFinished runs only after returned outcomes have been validated and
// settled. Invokers without start notifications still yield complete events.
func (events *runEvents) attemptFinished(descriptor StepInvocationDescriptor, attempt runjournal.Attempt, classification string) {
	if events == nil {
		return
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	identity, item := invocationEventIdentity(descriptor)
	events.startAttempt(identity, item)
	duration := float64(time.Since(events.attemptStarts[identity])) / float64(time.Millisecond)
	delete(events.attemptStarts, identity)
	if classification == "" {
		classification = attempt.Status
		if classification == StatusFailed {
			classification = "error"
		}
	}
	events.emit(runevent.Event{Type: "step.attempt.finished", NodeID: identity.node, ItemIndex: item, Attempt: identity.attempt, DurationMS: &duration, Status: attempt.Status, Classification: classification, Diagnostic: boundedEventText(attempt.Diagnostic)})
}

func (events *runEvents) stepFinished(step *runjournal.Step) {
	if events == nil {
		return
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	duration := 0.0
	if started, exists := events.stepStarts[step.NodeID]; exists {
		duration = float64(time.Since(started)) / float64(time.Millisecond)
	}
	delete(events.stepStarts, step.NodeID)
	events.emit(runevent.Event{Type: "step.finished", NodeID: step.NodeID, Status: step.Status, DurationMS: &duration})
}

func (events *runEvents) runFinished(status string) {
	if events == nil {
		return
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	events.emit(runevent.Event{Type: "run.finished", Status: status})
}

func classifyEventOutcome(outcome StepInvocationOutcome) string {
	switch {
	case outcome.Status == StatusSucceeded:
		return "succeeded"
	case outcome.Status == StatusCancelled:
		return "cancelled"
	case outcome.Status == stepInvocationStatusInfraFailed:
		return "infrastructure"
	case outcome.TimedOutAfter > 0:
		return "timeout"
	case outcome.ExitCode == runnerExitDescriptorResolution || outcome.ExitCode == runnerExitSchemaValidation:
		return "contract"
	case outcome.ExitCode == runnerExitNonRetryable:
		return "non-retryable"
	case outcome.ExitCode < 0:
		return "terminated"
	default:
		return "error"
	}
}

func boundedEventText(value string) string {
	text := []rune(strings.ToValidUTF8(value, "�"))
	if len(text) > 2048 {
		text = append(text[:2047], '…')
	}
	return string(text)
}

// startHook is nil when observation is disabled; invokers need no event path.
func (events *runEvents) startHook() func(StepInvocationDescriptor) {
	if events == nil {
		return nil
	}
	return events.attemptStarted
}

func (events *runEvents) attemptIsOpen(node string, item, attempt int) bool {
	if events == nil {
		return false
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	_, exists := events.attemptStarts[eventIdentity{node, item, attempt}]
	return exists
}
