package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Sly1029/massive/internal/controlplane"
	"github.com/Sly1029/massive/internal/runjournal"
)

type InspectCommand struct {
	RunID   string `arg:"" name:"run-id" help:"Run identifier."`
	Project string `help:"Project identity used when submitting the run." required:""`
	Store   string `help:"Local datastore root."`
	Step    string `help:"Show one step in text output." xor:"view"`
	JSON    bool   `help:"Print the complete validated run journal as JSON." xor:"view"`
}

func (command *InspectCommand) Run(ctx context.Context, stdout io.Writer) error {
	journal, err := controlplane.Inspect(ctx, command.Store, command.Project, command.RunID)
	if err != nil {
		return err
	}
	if command.Step != "" {
		found := false
		for _, step := range journal.Steps {
			if step.NodeID == command.Step {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("step %q is not in this run; omit --step to list all steps", command.Step)
		}
	}
	if command.JSON {
		return json.NewEncoder(stdout).Encode(journal)
	}
	fmt.Fprintf(stdout, "run %s  %s\nplan %s\n", journal.RunID, journal.Status, journal.PlanHash)
	for _, step := range journal.Steps {
		if command.Step != "" && command.Step != step.NodeID {
			continue
		}
		fmt.Fprintf(stdout, "  %s  %s\n", step.NodeID, step.Status)
		renderAttempts(stdout, step.Attempts, "    ")
		if step.SkipReason != nil {
			fmt.Fprintf(stdout, "    inactive case %s of %s\n", step.SkipReason.Case, step.SkipReason.DecisionID)
		}
		if step.Items != nil {
			for _, item := range *step.Items {
				fmt.Fprintf(stdout, "    [%d] %s\n", item.Index, item.Status)
				renderAttempts(stdout, item.Attempts, "      ")
				if item.Diagnostic != "" {
					fmt.Fprintf(stdout, "      error %s\n", item.Diagnostic)
				}
			}
		}
	}
	for _, decision := range journal.Decisions {
		fmt.Fprintf(stdout, "  decision %s  %s  %s\n", decision.NodeID, decision.Status, decision.SelectedCase)
	}
	if journal.Result != nil {
		fmt.Fprintf(stdout, "result %s  %s\n", journal.Result.Key, journal.Result.Hash)
	}
	return nil
}

func renderAttempts(stdout io.Writer, attempts []runjournal.Attempt, indent string) {
	for _, attempt := range attempts {
		fmt.Fprintf(stdout, "%sinput %s  %s\n", indent, attempt.Input.Key, attempt.Input.Hash)
		if attempt.Output != nil {
			fmt.Fprintf(stdout, "%soutput %s  %s\n", indent, attempt.Output.Manifest.Key, attempt.Output.Manifest.Hash)
		}
		if attempt.Diagnostic != "" {
			fmt.Fprintf(stdout, "%serror %s\n", indent, attempt.Diagnostic)
		}
	}
}
