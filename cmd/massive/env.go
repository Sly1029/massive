package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Sly1029/massive/internal/controlplane"
	"github.com/Sly1029/massive/internal/environment"
)

type EnvCommand struct {
	Check EnvCheckCommand `cmd:"" help:"Check the Python environment against a workflow's pyproject.toml and uv.lock without importing it."`
}

type EnvCheckCommand struct {
	Entry string `arg:"" name:"entry" help:"Python workflow file." type:"existingfile"`
	JSON  bool   `help:"Emit one structured JSON report."`
}

type envCheckOutput struct {
	Status string `json:"status"`
	// Identities are present only when there are no findings.
	RequirementHash string `json:"requirementHash,omitempty"`
	RealizationHash string `json:"realizationHash,omitempty"`
	*environment.Report
}

func (command *EnvCheckCommand) Run(ctx context.Context, stdout io.Writer) error {
	report, err := controlplane.CheckEnvironment(ctx, command.Entry, environment.Execution)
	if err != nil {
		return err
	}
	failure := report.Err()
	// "unverified" passes but declares nothing to check; CI can reject it.
	status, mark := "ready", "✓ environment ready"
	switch {
	case failure != nil:
		status = "failed"
	case report.Verification == environment.Undeclared:
		status, mark = "unverified", "! no [project] dependencies declared; only interpreter safety and the SDK release were checked"
	}
	if command.JSON {
		output := envCheckOutput{Status: status, Report: report,
			RequirementHash: report.Record.GetRequirementHash(), RealizationHash: report.Record.GetRealizationHash()}
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			return err
		}
		return failure
	}
	if failure != nil {
		return failure
	}
	interpreter := report.Interpreter
	_, err = fmt.Fprintf(stdout, "%s  %s\n  python        %s %s (%s/%s)\n  executable    %s\n  verification  %s\n  distributions %d\n  requirement   %s\n  realization   %s\n",
		mark, report.ProjectRoot, interpreter.Implementation, interpreter.Version, interpreter.OS, interpreter.Arch,
		interpreter.Executable, report.Verification, len(report.Distributions),
		report.Record.GetRequirementHash(), report.Record.GetRealizationHash())
	return err
}
