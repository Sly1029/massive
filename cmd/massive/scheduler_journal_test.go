package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/runjournal"
)

func TestLocalDecisionJournalIsDeterministic(t *testing.T) {
	source := `from typing import Annotated, Literal
from massive import GraphBuilder, StepContext, container, execution
from pydantic import BaseModel, Field

class Accepted(BaseModel):
    kind: Literal["accepted"] = "accepted"
    value: int
class Rejected(BaseModel):
    kind: Literal["rejected"] = "rejected"
    value: int
Classification = Annotated[Accepted | Rejected, Field(discriminator="kind")]

def source(ctx: StepContext[int]) -> int: return ctx.inputs
def left(ctx: StepContext[int]) -> Classification: return Accepted(value=ctx.inputs)
def right(ctx: StepContext[int]) -> Classification: return Accepted(value=ctx.inputs)
def accept(ctx: StepContext[Accepted]) -> int: return ctx.inputs.value
def reject(ctx: StepContext[Rejected]) -> int: return ctx.inputs.value
def forward(ctx: StepContext[int]) -> int: return ctx.inputs
def merge(ctx: StepContext[list[int]]) -> list[int]: return ctx.inputs

graph = GraphBuilder(name="journal-order", input_type=int, output_type=list[int],
    defaults=execution(environment=container("example.invalid/runner@sha256:" + "1" * 64)))
producer = graph.add(source)
graph.edge_from(graph.start).to(producer)
outputs = []
for classifier, name in [(left, "left"), (right, "right")]:
    classified = graph.add(classifier)
    graph.edge_from(producer).to(classified)
    route = graph.decision(classified, on="kind", id=name + "-route")
    accepted = graph.add(accept, id=name + "-accepted")
    rejected = graph.add(reject, id=name + "-rejected")
    graph.edge_from(route.case(Accepted)).to(accepted)
    graph.edge_from(route.case(Rejected)).to(rejected)
    selected = route.select(int, accepted=accepted, rejected=rejected)
    output = graph.add(forward, id=name + "-output")
    graph.edge_from(selected).to(output)
    outputs.append(output)
merged = graph.add(merge)
graph.gather(*outputs).to(merged)
graph.edge_from(merged).to_end(graph.end)
`
	config, _ := compiledExitHookFixture(t, source)
	var first []byte
	for _, delayed := range []string{"left", "right"} {
		config.DatastoreRoot = progressStore(t)
		config.MaxWorkers = 2
		ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
		other := "left-route"
		if delayed == "left" {
			other = "right-route"
		}
		observed := false
		config.Hooks = orchestrator.RunHooks{AfterStepInvocation: func(_ context.Context, descriptor orchestrator.StepInvocationDescriptor) error {
			if descriptor.NodeID != delayed {
				return nil
			}
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				body, err := os.ReadFile(filepath.Join(config.DatastoreRoot, "projects", descriptor.ProjectKey, "runs", descriptor.RunID, "run-manifest.json"))
				if err == nil {
					journal, err := runjournal.Parse(body)
					if err != nil {
						return err
					}
					for _, decision := range journal.Decisions {
						if decision.NodeID == other {
							observed = true
							return nil
						}
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		}}
		result, err := orchestrator.Run(ctx, config, []byte("20"))
		stop()
		if err != nil || !observed {
			t.Fatalf("delay %s did not settle the opposite decision first: %v", delayed, err)
		}
		body, err := os.ReadFile(filepath.Join(config.DatastoreRoot, filepath.FromSlash(result.ManifestKey)))
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = body
		} else if !bytes.Equal(first, body) {
			t.Fatalf("completion timing changed journal bytes:\n%s\n%s", first, body)
		}
	}
}
