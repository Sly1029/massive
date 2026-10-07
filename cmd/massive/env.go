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
	Entry string `arg:"" name:"entry" help:"Python workflow entrypoint, optionally followed by #export." type:"path"`
	JSON  bool   `help:"Emit one structured JSON report."`
}

type envCheckOutput struct {
	Status string `json:"status"`
	*environment.Report
}

func (command *EnvCheckCommand) Run(ctx context.Context, stdout io.Writer) error {
	report, err := controlplane.CheckEnvironment(ctx, command.Entry)
	if err != nil {
		return err
	}
	failure := report.Err()
	if command.JSON {
		status := "ready"
		if failure != nil {
			status = "failed"
		}
		if err := json.NewEncoder(stdout).Encode(envCheckOutput{Status: status, Report: report}); err != nil {
			return err
		}
		return failure
	}
	if failure != nil {
		return failure
	}
	interpreter := report.Interpreter
	_, err = fmt.Fprintf(stdout, "✓ environment ready  %s\n  python        %s %s (%s/%s)\n  executable    %s\n  verification  %s\n  distributions %d\n",
		report.ProjectRoot, interpreter.Implementation, interpreter.Version, interpreter.OS, interpreter.Arch,
		interpreter.Executable, report.Verification, len(report.Distributions))
	return err
}
