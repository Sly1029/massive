package orchestrator

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/graphgen"
)

// Named seeds prove their scenario through actual descriptor execution and the
// resulting journal, so extending the byte contract cannot silently erase it.
func TestGeneratedExecutionSeedsRetainNamedScenarios(t *testing.T) {
	files, err := filepath.Glob("testdata/fuzz/FuzzGeneratedGraphExecution/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		name := filepath.Base(file)
		// Go also persists unnamed minimized reproducers in this directory.
		if !strings.Contains(name, "-") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			literal := strings.TrimSpace(strings.TrimPrefix(string(body), "go test fuzz v1\n"))
			decoded, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(literal, "[]byte("), ")"))
			if err != nil {
				t.Fatal(err)
			}
			workflow, err := graphgen.Generate([]byte(decoded))
			if err != nil {
				t.Fatal(err)
			}
			prediction, err := workflow.Predict()
			if err != nil {
				t.Fatal(err)
			}
			result, runErr, executor, storeRoot := runGeneratedWorkflow(t, workflow, newCompletionGates(prediction))
			oracle := &executionOracle{t: t, workflow: workflow, prediction: prediction, storeRoot: storeRoot, executor: executor}
			oracle.check(result, runErr)
			features := map[string]bool{}
			records := map[graphgen.Invocation]executedInvocation{}
			for _, record := range executor.log {
				records[record.invocation] = record
			}
			for _, record := range executor.log {
				node := workflow.Nodes[record.invocation.NodeID]
				if record.fault == graphgen.FailNonRetryable {
					features["non-retryable-failure"] = true
				}
				if record.status == StatusSucceeded && strings.Contains(node.ID, "--") && len(node.Context) > 1 {
					features["nested-decisions-in-calls"] = true
				}
				next := record.invocation
				next.Attempt++
				if retried, ok := records[next]; ok && retried.status == StatusSucceeded {
					if record.fault == graphgen.Timeout {
						features["timeout-then-success"] = true
					}
					if record.fault == graphgen.OrphanFailure && record.published != "" {
						features["orphan-output-then-success"] = true
					}
				}
			}
			for _, step := range oracle.journal.Steps {
				if step.Items == nil {
					continue
				}
				node, outcome := workflow.Nodes[step.NodeID], prediction.Nodes[step.NodeID]
				items := *step.Items
				if step.Status == StatusSucceeded {
					if len(items) == 0 && len(node.Context) > 0 {
						features["empty-map-in-selected-branch"] = true
					}
					if len(items) >= 64 {
						features["large-map-succeeds"] = true
					}
					if node.CollectItemFailures {
						failed, retried, signal := false, false, false
						for i, item := range items {
							failed = failed || item.Status == StatusFailed
							retried = retried || item.Status == StatusSucceeded && len(item.Attempts) > 1
							for _, fault := range outcome.Items[i].Attempts {
								signal = signal || fault == graphgen.Crash
							}
						}
						features["collected-item-failures-beside-retried-siblings"] = failed && retried
						features["collected-signal"], features["ci-original-signal"] = signal, signal
					}
				}
				if step.Status != StatusFailed {
					continue
				}
				if node.CollectItemFailures && outcome.Fatal != nil {
					features["fatal-collecting-map"] = true
				}
				if outcome.Fatal != nil && outcome.Fatal.Attempt > 1 {
					for i, item := range outcome.Items {
						if i != outcome.Fatal.Item && item.Status == graphgen.Failed && len(item.Attempts) > 0 && len(item.Attempts) < node.MaxAttempts && item.Attempts[len(item.Attempts)-1].Retryable() {
							features["fatal-during-retry-backoff"] = true
						}
					}
				}
				if outcome.Fatal == nil {
					waiting, terminal := false, false
					for _, item := range outcome.Items {
						if item.Status != graphgen.Failed || len(item.Attempts) == 0 {
							continue
						}
						last := item.Attempts[len(item.Attempts)-1]
						waiting = waiting || last.Retryable() && len(item.Attempts) < node.MaxAttempts
						terminal = terminal || !last.Retryable()
					}
					features["sibling-retry-after-terminal-item"] = features["sibling-retry-after-terminal-item"] || !node.CollectItemFailures && waiting && terminal
					features["contract-failure-fails-collecting-map"] = features["contract-failure-fails-collecting-map"] || node.CollectItemFailures && terminal
				}
			}
			interrupts := map[string]graphgen.InterruptKind{
				"cancel-before-map-batch":             graphgen.CancelBefore,
				"cancel-during-map-batch":             graphgen.CancelDuring,
				"cancel-after-map-batch":              graphgen.CancelAfter,
				"infrastructure-failure-in-map-batch": graphgen.InfrastructureFailure,
			}
			if kind, ok := interrupts[name]; ok {
				features[name] = executor.fired != nil && *executor.fired == kind && workflow.Interrupt != nil && workflow.Nodes[workflow.Interrupt.NodeID].Kind == "map"
			}
			if !features[name] {
				t.Fatalf("seed no longer exercises %s", name)
			}
		})
	}
}
