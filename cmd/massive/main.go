package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sly1029/massive/internal/controlplane"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/alecthomas/kong"
)

type CLI struct {
	Run     RunCommand     `cmd:"" help:"Compile and execute a workflow locally."`
	Build   BuildCommand   `cmd:"" help:"Compile a workflow for a deployment target."`
	Version VersionCommand `cmd:"" help:"Print the Massive version."`
	Runtime RuntimeCommand `cmd:"" hidden:""`
}

type RunCommand struct {
	Entry     string `arg:"" name:"entry" help:"Python workflow entrypoint, optionally followed by #export." type:"path"`
	Input     string `help:"Workflow input as JSON; defaults to null."`
	InputFile string `name:"input-file" help:"Read workflow input JSON from this file." type:"existingfile"`
	Store     string `help:"Local artifact store root." type:"path"`
	Project   string `help:"Stable project identity, for example owner/repository."`
	RunID     string `name:"run-id" help:"Caller-provided run identifier."`
	JSON      bool   `help:"Emit one structured JSON result."`
	Verbose   bool   `help:"Include plan and artifact identities."`
}

type BuildCommand struct {
	Entry          string `arg:"" name:"entry" help:"Python workflow entrypoint, optionally followed by #export." type:"path"`
	Target         string `help:"Deployment target." enum:"argo" default:"argo"`
	Output         string `short:"o" help:"Bundle output directory." required:"" type:"path"`
	Profile        string `help:"Deployment profile name." default:"argo"`
	Namespace      string `help:"Kubernetes namespace." required:""`
	ServiceAccount string `name:"service-account" help:"Kubernetes service account used by workflow pods." required:""`
	ArtifactStore  string `name:"artifact-store" help:"Stable binding identity for future remote artifact transport." default:"embedded-v0"`
	Name           string `help:"WorkflowTemplate name; defaults to the workflow name."`
	JSON           bool   `help:"Emit one structured JSON result."`
}

type VersionCommand struct{}

type RuntimeCommand struct {
	Step RuntimeStepCommand `cmd:"" help:"Execute one compiled step in a remote executor."`
}

type RuntimeStepCommand struct {
	Plan      string `help:"Mounted canonical WorkflowPlan." required:"" type:"existingfile"`
	BundleDir string `name:"bundle-dir" help:"Directory containing mounted source archives." required:"" type:"existingdir"`
	Node      string `help:"Static plan node to execute." required:""`
	Input     string `help:"Canonical JSON step input." required:""`
	Output    string `help:"Write canonical JSON result to this path." required:"" type:"path"`
	Project   string `help:"Stable remote project identity." required:""`
	RunID     string `name:"run-id" help:"Remote workflow run identifier." required:""`
	Store     string `help:"Ephemeral local artifact store." default:"/tmp/massive-store" type:"path"`
}

type runOutput struct {
	RunID    string          `json:"runId"`
	Status   string          `json:"status"`
	Result   json.RawMessage `json:"result,omitempty"`
	PlanHash string          `json:"planHash,omitempty"`
	Store    string          `json:"store,omitempty"`
}

func (command *RunCommand) Run(ctx context.Context, stdout io.Writer) error {
	input, err := command.input()
	if err != nil {
		return err
	}
	frontend, err := controlplane.Emit(ctx, command.Entry)
	if err != nil {
		return err
	}
	result, err := controlplane.RunLocal(ctx, controlplane.LocalRunRequest{
		Frontend: frontend, Input: input, Store: command.Store,
		Project: command.Project, RunID: command.RunID,
	})
	if err != nil {
		if result != nil && result.Run != nil {
			_ = renderRun(stdout, command.JSON, command.Verbose, result, nil)
		}
		return err
	}
	return renderRun(stdout, command.JSON, command.Verbose, result, result.Result)
}

func (command *RunCommand) input() ([]byte, error) {
	if command.InputFile != "" && command.Input != "" {
		return nil, errors.New("--input and --input-file are mutually exclusive")
	}
	input := []byte("null")
	if command.Input != "" {
		input = []byte(command.Input)
	}
	if command.InputFile != "" {
		body, err := os.ReadFile(command.InputFile)
		if err != nil {
			return nil, fmt.Errorf("read input file: %w", err)
		}
		input = body
	}
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return nil, fmt.Errorf("workflow input is not valid JSON: %w", err)
	}
	return input, nil
}

func renderRun(writer io.Writer, jsonMode, verbose bool, result *controlplane.LocalRunResult, value json.RawMessage) error {
	status := "failed"
	runID := ""
	if result.Run != nil {
		status = result.Run.Status
		runID = result.Run.RunID
	}
	if jsonMode {
		output := runOutput{RunID: runID, Status: status, Result: value}
		if verbose {
			output.PlanHash = result.Plan.PlanHash
			output.Store = result.Store
		}
		return json.NewEncoder(writer).Encode(output)
	}
	if result.Run != nil {
		for _, step := range result.Run.Steps {
			mark := "✓"
			if step.Status == orchestrator.StatusFailed {
				mark = "✗"
			} else if step.Status != orchestrator.StatusSucceeded {
				mark = "·"
			}
			fmt.Fprintf(writer, "  %-16s %s %s\n", step.NodeID, mark, step.Status)
		}
	}
	if status == orchestrator.StatusSucceeded {
		fmt.Fprintf(writer, "\n✓ succeeded  run %s\n  result  %s\n", runID, value)
	} else {
		fmt.Fprintf(writer, "\n✗ failed  run %s\n", runID)
	}
	if verbose {
		fmt.Fprintf(writer, "  plan    %s\n  store   %s\n", result.Plan.PlanHash, result.Store)
	}
	return nil
}

func (command *BuildCommand) Run(ctx context.Context, stdout io.Writer) error {
	frontend, err := controlplane.Emit(ctx, command.Entry)
	if err != nil {
		return err
	}
	output, err := filepath.Abs(command.Output)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}
	result, err := controlplane.BundleArgo(controlplane.ArgoBundleRequest{
		Frontend: frontend, OutputDirectory: output, ProfileName: command.Profile,
		ArtifactStoreBinding: command.ArtifactStore, Namespace: command.Namespace,
		ServiceAccountName: command.ServiceAccount, WorkflowTemplateName: command.Name,
	})
	if err != nil {
		return err
	}
	if command.JSON {
		return json.NewEncoder(stdout).Encode(map[string]any{
			"status": "built", "target": command.Target, "output": output,
			"planHash": result.PlanHash, "deploymentHash": result.DeploymentHash,
			"bundleHash": result.BundleHash, "files": result.Files,
		})
	}
	fmt.Fprintf(stdout, "✓ built Argo bundle\n  output      %s\n  plan        %s\n  deployment  %s\n", output, result.PlanHash, result.DeploymentHash)
	return nil
}

func (*VersionCommand) Run(stdout io.Writer) error {
	_, err := fmt.Fprintf(stdout, "massive %s\n", controlplane.Version)
	return err
}

func (command *RuntimeStepCommand) Run(ctx context.Context) error {
	planJSON, err := os.ReadFile(command.Plan)
	if err != nil {
		return fmt.Errorf("read runtime plan: %w", err)
	}
	parsed, err := plan.ParseCanonicalJSON(planJSON)
	if err != nil {
		return err
	}
	workflowPlan, err := plan.VerifyCanonicalJSON(planJSON, parsed.GetPlanHash())
	if err != nil {
		return err
	}
	archives := make(map[string][]byte, len(workflowPlan.GetSourcePackages()))
	for _, sourcePackage := range workflowPlan.GetSourcePackages() {
		name, err := orchestrator.SourceArchiveBundleName(sourcePackage.GetPackageHash())
		if err != nil {
			return err
		}
		body, err := os.ReadFile(filepath.Join(command.BundleDir, name))
		if err != nil {
			return fmt.Errorf("read runtime source archive %s: %w", name, err)
		}
		archives[sourcePackage.GetPackageHash()] = body
	}
	runnerCommand := []string(nil)
	if python := os.Getenv("MASSIVE_PYTHON"); python != "" {
		runnerCommand = []string{python, "-m", "massive.runner", "{descriptor}"}
	}
	result, err := orchestrator.RunIsolatedStep(ctx, orchestrator.IsolatedStepConfig{
		Plan: workflowPlan, NodeID: command.Node, DatastoreRoot: command.Store,
		ProjectID: command.Project, RunID: command.RunID, RunnerCommand: runnerCommand,
		SourceArchives: archives,
	}, []byte(command.Input))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(command.Output), 0o755); err != nil {
		return fmt.Errorf("create runtime output directory: %w", err)
	}
	if err := os.WriteFile(command.Output, result, 0o644); err != nil {
		return fmt.Errorf("write runtime output: %w", err)
	}
	return nil
}

func main() {
	cli := CLI{}
	parser, err := kong.New(&cli,
		kong.Name("massive"),
		kong.Description("Typed workflows compiled once for local and remote targets."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true, Summary: true}),
		kong.Writers(os.Stdout, os.Stderr),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	parseContext, err := parser.Parse(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	parseContext.BindTo(context.Background(), (*context.Context)(nil))
	parseContext.BindTo(os.Stdout, (*io.Writer)(nil))
	if err := parseContext.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ %s\n", strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
