package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Sly1029/massive/internal/controlplane"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/runjournal"
)

type InspectCommand struct {
	RunID       string `arg:"" name:"run-id" help:"Run identifier."`
	Project     string `help:"Project identity used when submitting the run." required:""`
	Store       string `help:"Local datastore root."`
	Step        string `help:"Show one step in text output." xor:"view,scope"`
	Environment bool   `help:"Show the verified dependency environment the run recorded." xor:"scope"`
	JSON        bool   `help:"Print the complete validated run journal, or with --environment its record, as JSON." xor:"view"`
}

func (command *InspectCommand) Run(ctx context.Context, stdout io.Writer) error {
	if command.Environment {
		return command.renderEnvironment(ctx, stdout)
	}
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
	if journal.Environment != nil {
		fmt.Fprintf(stdout, "environment %s\n", journal.Environment.RealizationHash)
	}
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
		fmt.Fprintf(stdout, "%sattempt %d  %s\n", indent, attempt.Attempt, attempt.Status)
		fmt.Fprintf(stdout, "%sinput %s  %s\n", indent, attempt.Input.Key, attempt.Input.Hash)
		if attempt.Output != nil {
			fmt.Fprintf(stdout, "%soutput %s  %s\n", indent, attempt.Output.Manifest.Key, attempt.Output.Manifest.Hash)
		}
		if attempt.Diagnostic != "" {
			fmt.Fprintf(stdout, "%serror %s\n", indent, attempt.Diagnostic)
		}
	}
}

func (command *InspectCommand) renderEnvironment(ctx context.Context, stdout io.Writer) error {
	journal, record, err := controlplane.InspectEnvironment(ctx, command.Store, command.Project, command.RunID)
	if err != nil {
		return err
	}
	if command.JSON {
		body, err := environment.MarshalRecord(record)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", body)
		return err
	}
	requirement, realization := record.GetRequirement(), record.GetRealization()
	fmt.Fprintf(stdout, "run %s  %s\nrequirement  %s\n", journal.RunID, journal.Status, record.GetRequirementHash())
	if requirement.RequiresPython != nil {
		fmt.Fprintf(stdout, "  requires-python  %s\n", requirement.GetRequiresPython())
	}
	if requirement.LockHash != nil {
		fmt.Fprintf(stdout, "  uv.lock          %s\n", requirement.GetLockHash())
	}
	for _, dependency := range requirement.GetDependencies() {
		fmt.Fprintf(stdout, "  dependency       %s\n", dependency)
	}
	fmt.Fprintf(stdout, "realization  %s\n  verification     %s\n  python           %s %s (%s, %s/%s)\n  materializer     %s %s\n",
		record.GetRealizationHash(), realization.GetVerification(),
		realization.GetImplementation(), realization.GetPythonVersion(), realization.GetSysconfigPlatform(),
		realization.GetOs(), realization.GetArch(), realization.GetMaterializerName(), realization.GetMaterializerVersion())
	for _, distribution := range realization.GetDistributions() {
		source := ""
		if distribution.GetEditable() {
			source = "  (editable)"
		} else if distribution.GetDirect() {
			source = "  (direct)"
		}
		fmt.Fprintf(stdout, "  %s==%s%s\n", distribution.GetName(), distribution.GetVersion(), source)
	}
	return nil
}
