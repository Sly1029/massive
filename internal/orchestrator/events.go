package orchestrator

import (
	"strings"
	"time"

	"github.com/Sly1029/massive/internal/runevent"
	"github.com/Sly1029/massive/internal/runjournal"
)

// localJournal observes the very records used for durable publication. Item
// callbacks are serialized by the invoker; publication stays once per round.
type localJournal struct {
	runjournal.Manifest
	events *runEvents
}

type eventIdentity struct {
	node    string
	item    int
	attempt int
}
type observedEntry struct {
	started    time.Time
	finished   bool
	attempts   map[int]string
	itemStatus string
}
type mapProgress struct{ completed, failed, total int }
type runEvents struct {
	hook     func(runevent.Event)
	runID    string
	started  time.Time
	sequence int
	entries  map[eventIdentity]*observedEntry
	outcomes map[eventIdentity]StepInvocationOutcome
	policies map[string]int
	maps     map[string]*mapProgress
}

func (events *runEvents) emit(event runevent.Event) {
	if events.hook == nil {
		return
	}
	events.sequence++
	event.SchemaVersion, event.Sequence, event.RunID = runevent.Version, events.sequence, events.runID
	event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	event.ElapsedMS = float64(time.Since(events.started)) / float64(time.Millisecond)
	events.hook(event)
}

func (events *runEvents) outcome(outcome StepInvocationOutcome) {
	if events.hook == nil {
		return
	}
	item := -1
	if outcome.Scope != nil {
		item = outcome.Scope.Frames[0].Index
	}
	events.outcomes[eventIdentity{outcome.NodeID, item, outcome.Attempt}] = outcome
}

func (journal *localJournal) observe(step *runjournal.Step, itemIndex *int) {
	events := journal.events
	if events.hook == nil {
		return
	}
	identity := eventIdentity{node: step.NodeID, item: -1}
	status, attempts := step.Status, step.Attempts
	if itemIndex != nil {
		identity.item = *itemIndex
		item := (*step.Items)[*itemIndex]
		status, attempts = item.Status, item.Attempts
	}
	entry := events.entries[identity]
	if entry == nil {
		entry = &observedEntry{attempts: make(map[int]string)}
		events.entries[identity] = entry
	}
	if entry.started.IsZero() && status == StatusRunning {
		entry.started = time.Now()
		if itemIndex == nil {
			events.emit(runevent.Event{Type: "step.started", NodeID: step.NodeID})
		}
	}
	maxAttempts := events.policies[step.NodeID]
	if step.Items != nil && itemIndex == nil {
		maxAttempts = 1
	}
	for _, attempt := range attempts {
		previous := entry.attempts[attempt.Attempt]
		if previous == attempt.Status {
			continue
		}
		attemptIdentity := identity
		attemptIdentity.attempt = attempt.Attempt
		// Each start has its own monotonic clock, including after retry backoff.
		clock := events.entries[attemptIdentity]
		if previous == "" {
			clock = &observedEntry{started: time.Now()}
			events.entries[attemptIdentity] = clock
			events.emit(runevent.Event{Type: "step.attempt.started", NodeID: step.NodeID, ItemIndex: itemIndex, Attempt: attempt.Attempt, MaxAttempts: maxAttempts})
		}
		entry.attempts[attempt.Attempt] = attempt.Status
		if attempt.Status == StatusRunning {
			continue
		}
		duration := float64(time.Since(clock.started)) / float64(time.Millisecond)
		classification, diagnostic, tail := attempt.Status, attempt.Diagnostic, ""
		if outcome, ok := events.outcomes[attemptIdentity]; ok {
			classification = classifyEventOutcome(outcome)
			if outcome.Status == StatusSucceeded && attempt.Status == StatusFailed {
				classification = "verification"
			}
			if outcome.Status != StatusSucceeded {
				diagnostic = withRunnerMessage(runnerFailureSummary(outcome, "runner failed"), outcome.RunnerMessage)
				if outcome.Status == StatusCancelled || outcome.Status == stepInvocationStatusInfraFailed {
					diagnostic = attempt.Diagnostic
				}
				tail = boundedLogTail(outcome.Diagnostic)
			}
			delete(events.outcomes, attemptIdentity)
		} else if attempt.Status == StatusFailed {
			classification = "verification"
		}
		events.emit(runevent.Event{Type: "step.attempt.finished", NodeID: step.NodeID, ItemIndex: itemIndex, Attempt: attempt.Attempt, MaxAttempts: maxAttempts, DurationMS: &duration, Status: attempt.Status, Classification: classification, Diagnostic: boundedEventText(diagnostic, 2048), LogTail: tail})
	}
	terminal := status != StatusPending && status != StatusRunning
	if itemIndex != nil {
		if terminal && entry.itemStatus != status {
			progress := events.maps[step.NodeID]
			if entry.itemStatus == "" || entry.itemStatus == StatusRunning {
				progress.completed++
			}
			if status == StatusFailed && entry.itemStatus != StatusFailed {
				progress.failed++
			}
			entry.itemStatus = status
			events.progress(step.NodeID)
		}
		return
	}
	if terminal && !entry.finished {
		entry.finished = true
		duration := 0.0
		if !entry.started.IsZero() {
			duration = float64(time.Since(entry.started)) / float64(time.Millisecond)
		}
		diagnostic := ""
		if last := lastAttempt(attempts); last != nil {
			diagnostic = last.Diagnostic
		}
		events.emit(runevent.Event{Type: "step.finished", NodeID: step.NodeID, Status: status, DurationMS: &duration, Diagnostic: boundedEventText(diagnostic, 2048)})
	}
}

func (events *runEvents) progress(nodeID string) {
	if events.hook == nil {
		return
	}
	progress := events.maps[nodeID]
	completed, failed, total := progress.completed, progress.failed, progress.total
	events.emit(runevent.Event{Type: "map.progress", NodeID: nodeID, Completed: &completed, Failed: &failed, Total: &total})
}

// Reconcile bulk terminalization, including queued nodes and items that never
// dispatched. These events carry no attempt for undispatched map items.
func (journal *localJournal) observeTerminal() {
	for index := range journal.Steps {
		step := &journal.Steps[index]
		if step.Items != nil {
			for itemIndex := range *step.Items {
				journal.observe(step, &itemIndex)
			}
		}
		journal.observe(step, nil)
	}
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
		return "killed"
	default:
		return "error"
	}
}

func boundedEventText(value string, limit int) string {
	text := []rune(strings.ToValidUTF8(value, "�"))
	if len(text) > limit {
		text = append(text[:limit-1], '…')
	}
	return string(text)
}

func boundedLogTail(value string) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	text := []rune(strings.ToValidUTF8(strings.Join(lines, "\n"), "�"))
	if len(text) > 8192 {
		text = text[len(text)-8192:]
	}
	return string(text)
}

func newRunEvents(config RunConfig, index executionIndex, runID string, started time.Time) *runEvents {
	events := &runEvents{hook: config.Hooks.OnRunEvent, runID: runID, started: started, entries: make(map[eventIdentity]*observedEntry), outcomes: make(map[eventIdentity]StepInvocationOutcome), policies: make(map[string]int), maps: make(map[string]*mapProgress)}
	for id, node := range index.nodesByID {
		events.policies[id] = policyForContract(index.contractsByRef[node.GetContractRef()]).maxAttempts
	}
	count := len(index.stepOrder)
	events.emit(runevent.Event{Type: "run.started", PlanHash: config.Plan.GetPlanHash(), NodeCount: &count})
	return events
}

func (journal *localJournal) finishEvents(result *RunResult) {
	journal.observeTerminal()
	duration := float64(time.Since(journal.events.started)) / float64(time.Millisecond)
	event := runevent.Event{Type: "run.finished", Status: journal.Status, DurationMS: &duration, Diagnostic: boundedEventText(journal.Diagnostic, 2048)}
	var failed string
	for _, step := range journal.Steps {
		if step.Status == StatusFailed || step.Status == StatusCancelled {
			if len(step.Attempts) > 0 {
				failed = step.NodeID
				break
			}
		}
	}
	if failed != "" {
		event.FailedNode = &failed
	}
	if result.ExitHook != nil {
		event.ExitHook = &runevent.ExitHook{NodeID: result.ExitHook.NodeID, Status: result.ExitHook.Status, PersistenceDiagnostic: boundedEventText(result.ExitHookPersistenceDiagnostic, 2048)}
	}
	journal.events.emit(event)
}
