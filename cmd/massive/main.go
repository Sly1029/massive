package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/controlplane"
	"github.com/Sly1029/massive/internal/deployment"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/valueparam"
	"github.com/alecthomas/kong"
)

type CLI struct {
	Inspect InspectCommand `cmd:"" help:"Inspect a recorded local run without executing it."`
	Run     RunCommand     `cmd:"" help:"Compile and execute a workflow locally."`
	Build   BuildCommand   `cmd:"" help:"Compile a workflow for a deployment target."`
	Publish PublishCommand `cmd:"" help:"Upload an object-store-v0 bundle's source archives to its datastore."`
	Env     EnvCommand     `cmd:"" help:"Check a workflow's dependency environment."`
	Version VersionCommand `cmd:"" help:"Print the Massive version."`
	Runtime RuntimeCommand `cmd:"" hidden:""`
}

type RunCommand struct {
	Entry     string `arg:"" name:"entry" help:"Python or TypeScript workflow entrypoint, optionally followed by #export." type:"path"`
	Input     string `help:"Workflow input as JSON; defaults to null." xor:"input-source"`
	InputFile string `name:"input-file" help:"Read workflow input JSON from this file." type:"existingfile" xor:"input-source"`
	Store     string `help:"Local artifact store root." type:"path"`
	Project   string `help:"Stable project identity, for example owner/repository."`
	RunID     string `name:"run-id" help:"Caller-provided run identifier."`
	JSON      bool   `help:"Emit one structured JSON result."`
	Verbose   bool   `help:"Include plan and artifact identities."`
}

type BuildCommand struct {
	SecretBindings            string `name:"secret-bindings" help:"JSON file mapping logical secret refs to Kubernetes Secret name/key bindings." type:"existingfile"`
	ArtifactCredentialsSecret string `name:"artifact-credentials-secret" help:"Optional Secret containing standard AWS credential keys; omit for workload identity."`
	Entry                     string `arg:"" name:"entry" help:"Python or TypeScript workflow entrypoint, optionally followed by #export." type:"path"`
	Target                    string `help:"Deployment target." enum:"argo" default:"argo"`
	Output                    string `short:"o" help:"Bundle output directory." required:"" type:"path"`
	Profile                   string `help:"Deployment profile name." default:"argo"`
	Namespace                 string `help:"Kubernetes namespace." required:""`
	ServiceAccount            string `name:"service-account" help:"Kubernetes service account used by workflow pods." required:""`
	ArtifactStore             string `name:"artifact-store" help:"ConfigMap containing the shared S3 datastore.json descriptor." required:""`
	Name                      string `help:"WorkflowTemplate name; defaults to the workflow name."`
	RuntimeTransport          string `name:"runtime-transport" help:"How pods receive source: embedded-v0 (runtime ConfigMap, at most 700 KiB) or object-store-v0 (shared datastore, uploaded with massive publish)." enum:"embedded-v0,object-store-v0" default:"embedded-v0"`
	JSON                      bool   `help:"Emit one structured JSON result."`
}

type PublishCommand struct {
	Bundle          string `arg:"" name:"bundle" help:"Directory written by massive build --runtime-transport object-store-v0." type:"existingdir"`
	DatastoreConfig string `name:"datastore-config" help:"Credential-free datastore descriptor naming the store the bundle's pods read." required:"" type:"existingfile"`
	JSON            bool   `help:"Emit one structured JSON result."`
}

type VersionCommand struct{}

type RuntimeControlCommand struct {
	Plan            string `help:"Mounted canonical WorkflowPlan." required:"" type:"existingfile"`
	Node            string `help:"Decision or select node." required:""`
	Input           string `help:"Control value parameter: canonical JSON or a value reference." required:""`
	Output          string `help:"Write the validated value parameter here." required:"" type:"path"`
	DatastoreConfig string `name:"datastore-config" help:"Credential-free datastore descriptor JSON file." required:"" type:"existingfile"`
}

func (command *RuntimeControlCommand) Run(ctx context.Context) error {
	verified, err := readRuntimePlan(command.Plan)
	if err != nil {
		return err
	}
	codec, _, err := openRuntimeDatastore(ctx, command.DatastoreConfig)
	if err != nil {
		return err
	}
	input, err := codec.Decode(ctx, []byte(command.Input))
	if err != nil {
		return err
	}
	result, err := orchestrator.ResolveControlValue(verified, command.Node, input.Body)
	if err != nil {
		return err
	}
	if result.CaseIndex != nil {
		if err := writeRuntimeOutput(command.Output+".case", []byte(fmt.Sprint(*result.CaseIndex))); err != nil {
			return err
		}
	}
	// Control tasks pass the validated value through; a reference is forwarded
	// rather than downloaded again by a second publication.
	output := input.Parameter()
	if input.Ref == nil {
		if output, err = codec.Encode(ctx, result.Value); err != nil {
			return err
		}
	}
	return writeRuntimeOutput(command.Output, output)
}

type RuntimeCommand struct {
	Control RuntimeControlCommand `cmd:"" help:"Validate a decision or selected value without invoking user code."`
	Step    RuntimeStepCommand    `cmd:"" help:"Execute one compiled step in a remote executor."`
	Map     RuntimeMapCommand     `cmd:"" help:"Execute finite-map transport operations."`
}

type RuntimeMapCommand struct {
	Expand  RuntimeMapExpandCommand  `cmd:"" help:"Expand a crystallized list into indexed Argo loop items."`
	Item    RuntimeMapItemCommand    `cmd:"" help:"Execute one indexed map item in a remote executor."`
	Collect RuntimeMapCollectCommand `cmd:"" help:"Collect indexed Argo loop results in source order."`
}

type RuntimeStepCommand struct {
	Plan            string `help:"Mounted canonical WorkflowPlan." required:"" type:"existingfile"`
	RuntimeSources  `embed:""`
	Node            string   `help:"Static plan node to execute." required:""`
	Input           string   `help:"Step input parameter: canonical JSON or a value reference." xor:"input" required:""`
	MergeInputs     []string `name:"merge-input" help:"One value parameter per merge source, in the node's mergeInputs order." sep:"none" xor:"input" required:""`
	Output          string   `help:"Write canonical JSON result to this path." required:"" type:"path"`
	Project         string   `help:"Stable remote project identity." required:""`
	RunID           string   `name:"run-id" help:"Remote workflow run identifier." required:""`
	DatastoreConfig string   `name:"datastore-config" help:"Credential-free datastore descriptor JSON file." required:"" type:"existingfile"`
	RetryCount      int      `name:"retry-count" help:"Retries already attempted by the target scheduler; the attempt is retry-count + 1." default:"0"`
}

// RuntimeSources selects the source transport: embedded-v0 mounts archives
// beside the plan; object-store-v0 pins each published archive's digest.
type RuntimeSources struct {
	BundleDir      string            `name:"bundle-dir" help:"Directory containing mounted embedded-v0 source archives." type:"existingdir" xor:"sources" required:""`
	SourceArchives map[string]string `name:"source-archive" help:"Published object-store-v0 archive as <package-hash>=<archive-digest>; repeat per package." xor:"sources" required:""`
}

func (sources RuntimeSources) resolve(workflowPlan *planpb.WorkflowPlan) (map[string]orchestrator.SourceArchive, error) {
	archives := make(map[string]orchestrator.SourceArchive, len(workflowPlan.GetSourcePackages()))
	if sources.BundleDir == "" {
		for packageHash, digest := range sources.SourceArchives {
			archives[packageHash] = orchestrator.SourceArchive{Digest: digest}
		}
		return archives, nil
	}
	for _, sourcePackage := range workflowPlan.GetSourcePackages() {
		name, err := orchestrator.SourceArchiveBundleName(sourcePackage.GetPackageHash())
		if err != nil {
			return nil, err
		}
		body, err := os.ReadFile(filepath.Join(sources.BundleDir, name))
		if err != nil {
			return nil, fmt.Errorf("read runtime source archive %s: %w", name, err)
		}
		archives[sourcePackage.GetPackageHash()] = orchestrator.EmbeddedSourceArchive(body)
	}
	return archives, nil
}

type RuntimeMapExpandCommand struct {
	Input           string `help:"Map input parameter: canonical JSON or a value reference." required:""`
	Output          string `help:"Write indexed Argo loop items to this path." required:"" type:"path"`
	DatastoreConfig string `name:"datastore-config" help:"Credential-free datastore descriptor JSON file." required:"" type:"existingfile"`
}

type RuntimeMapItemCommand struct {
	Plan            string `help:"Mounted canonical WorkflowPlan." required:"" type:"existingfile"`
	RuntimeSources  `embed:""`
	Node            string `help:"Static plan map node to execute." required:""`
	Item            string `help:"Indexed Argo map item envelope." required:""`
	Output          string `help:"Write indexed map result to this path." required:"" type:"path"`
	Project         string `help:"Stable remote project identity." required:""`
	RunID           string `name:"run-id" help:"Remote workflow run identifier." required:""`
	DatastoreConfig string `name:"datastore-config" help:"Credential-free datastore descriptor JSON file." required:"" type:"existingfile"`
	RetryCount      int    `name:"retry-count" help:"Retries already attempted by the target scheduler; the attempt is retry-count + 1." default:"0"`
}

type RuntimeMapCollectCommand struct {
	Input           string `help:"Aggregated indexed Argo map results." required:""`
	Output          string `help:"Write the ordered result list parameter to this path." required:"" type:"path"`
	DatastoreConfig string `name:"datastore-config" help:"Credential-free datastore descriptor JSON file." required:"" type:"existingfile"`
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
	frontend, err := controlplane.Emit(ctx, command.Entry, environment.Execution)
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
		fmt.Fprintf(writer, "\n✗ %s  run %s\n", status, runID)
	}
	if verbose {
		fmt.Fprintf(writer, "  plan    %s\n  store   %s\n", result.Plan.PlanHash, result.Store)
	}
	return nil
}

func (command *BuildCommand) Run(ctx context.Context, stdout io.Writer) error {
	var secretBindings map[string]deployment.SecretKeyRef
	if command.SecretBindings != "" {
		data, err := os.ReadFile(command.SecretBindings)
		if err != nil {
			return fmt.Errorf("read secret bindings: %w", err)
		}
		secretBindings, err = deployment.ParseSecretBindings(data)
		if err != nil {
			return fmt.Errorf("invalid secret bindings: %w", err)
		}
	}
	// The container realizes requirements; its pods run the full preflight.
	frontend, err := controlplane.Emit(ctx, command.Entry, environment.Emission)
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
		ArtifactCredentialsSecret: command.ArtifactCredentialsSecret,
		SecretBindings:            secretBindings,
		ServiceAccountName:        command.ServiceAccount, WorkflowTemplateName: command.Name,
		RuntimeTransport: command.RuntimeTransport,
	})
	if err != nil {
		return err
	}
	if command.JSON {
		return json.NewEncoder(stdout).Encode(map[string]any{
			"status": "built", "target": command.Target, "output": output,
			"planHash": result.PlanHash, "deploymentHash": result.DeploymentHash,
			"bundleHash": result.BundleHash, "runtimeTransport": result.RuntimeTransport,
			"files": result.Files,
		})
	}
	fmt.Fprintf(stdout, "✓ built Argo bundle\n  output      %s\n  plan        %s\n  deployment  %s\n  transport   %s\n", output, result.PlanHash, result.DeploymentHash, result.RuntimeTransport)
	return nil
}

func (command *PublishCommand) Run(ctx context.Context, stdout io.Writer) error {
	descriptorJSON, err := os.ReadFile(command.DatastoreConfig)
	if err != nil {
		return fmt.Errorf("read datastore descriptor: %w", err)
	}
	descriptor, err := orchestrator.ParseDatastoreDescriptor(descriptorJSON)
	if err != nil {
		return err
	}
	published, err := controlplane.PublishArgoSources(ctx, command.Bundle, descriptor)
	if err != nil {
		return err
	}
	if command.JSON {
		return json.NewEncoder(stdout).Encode(map[string]any{"status": "published", "sourceArchives": published})
	}
	for _, archive := range published {
		state := "uploaded"
		if !archive.Created {
			state = "present "
		}
		fmt.Fprintf(stdout, "✓ %s  %s  %s\n", state, archive.ArchiveHash, archive.Key)
	}
	return nil
}

func (*VersionCommand) Run(stdout io.Writer) error {
	_, err := fmt.Fprintf(stdout, "massive %s\n", controlplane.Version)
	return err
}

func (command *RuntimeStepCommand) Run(ctx context.Context) error {
	workflowPlan, err := readRuntimePlan(command.Plan)
	if err != nil {
		return err
	}
	codec, descriptor, err := openRuntimeDatastore(ctx, command.DatastoreConfig)
	if err != nil {
		return err
	}
	input, err := command.input(ctx, codec, workflowPlan)
	if err != nil {
		return err
	}
	result, err := runRuntimeInvocation(ctx, workflowPlan, command.RuntimeSources, command.Node, input, command.Project, command.RunID, descriptor, command.RetryCount, nil)
	if err != nil {
		return err
	}
	parameter, err := valueparam.EncodePublished(result)
	if err != nil {
		return err
	}
	return writeRuntimeOutput(command.Output, parameter)
}

// input resolves the step's value parameter, or assembles a merge step's
// ordered array from one parameter per source so references stay valid.
func (command *RuntimeStepCommand) input(ctx context.Context, codec valueparam.Codec, workflowPlan *planpb.WorkflowPlan) ([]byte, error) {
	if command.MergeInputs == nil {
		value, err := codec.Decode(ctx, []byte(command.Input))
		return value.Body, err
	}
	for _, node := range workflowPlan.GetGraph().GetNodes() {
		if node.GetId() == command.Node && len(node.GetMergeInputs()) != len(command.MergeInputs) {
			return nil, fmt.Errorf("step %q merges %d sources but received %d merge inputs", command.Node, len(node.GetMergeInputs()), len(command.MergeInputs))
		}
	}
	bodies := make([][]byte, len(command.MergeInputs))
	for index, parameter := range command.MergeInputs {
		value, err := codec.Decode(ctx, []byte(parameter))
		if err != nil {
			return nil, fmt.Errorf("merge input %d: %w", index, err)
		}
		bodies[index] = value.Body
	}
	return slices.Concat([]byte("["), bytes.Join(bodies, []byte(",")), []byte("]")), nil
}

func (command *RuntimeMapExpandCommand) Run(ctx context.Context) error {
	codec, _, err := openRuntimeDatastore(ctx, command.DatastoreConfig)
	if err != nil {
		return err
	}
	items, err := codec.ExpandItems(ctx, []byte(command.Input))
	if err != nil {
		return err
	}
	return writeRuntimeOutput(command.Output, items)
}

func (command *RuntimeMapItemCommand) Run(ctx context.Context) error {
	codec, descriptor, err := openRuntimeDatastore(ctx, command.DatastoreConfig)
	if err != nil {
		return err
	}
	item, empty, err := codec.DecodeItem(ctx, []byte(command.Item))
	if err != nil {
		return err
	}
	if empty {
		return writeRuntimeOutput(command.Output, []byte(`{"empty":true}`))
	}
	workflowPlan, err := readRuntimePlan(command.Plan)
	if err != nil {
		return err
	}
	result, err := runRuntimeInvocation(ctx, workflowPlan, command.RuntimeSources, command.Node, item.Body, command.Project, command.RunID, descriptor, command.RetryCount, &item.Index)
	if err != nil {
		return err
	}
	envelope, err := valueparam.EncodePublishedItemResult(item.Index, result)
	if err != nil {
		return err
	}
	return writeRuntimeOutput(command.Output, envelope)
}

func (command *RuntimeMapCollectCommand) Run(ctx context.Context) error {
	codec, _, err := openRuntimeDatastore(ctx, command.DatastoreConfig)
	if err != nil {
		return err
	}
	result, err := codec.CollectResults(ctx, []byte(command.Input))
	if err != nil {
		return err
	}
	return writeRuntimeOutput(command.Output, result)
}

func readRuntimePlan(path string) (*planpb.WorkflowPlan, error) {
	planJSON, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runtime plan: %w", err)
	}
	parsed, err := plan.ParseCanonicalJSON(planJSON)
	if err != nil {
		return nil, err
	}
	return plan.VerifyCanonicalJSON(planJSON, parsed.GetPlanHash())
}

func openRuntimeDatastore(ctx context.Context, path string) (valueparam.Codec, orchestrator.DatastoreDescriptor, error) {
	bindingJSON, err := os.ReadFile(path)
	if err != nil {
		return valueparam.Codec{}, nil, fmt.Errorf("read datastore binding: %w", err)
	}
	descriptor, err := orchestrator.ParseDatastoreDescriptor(bindingJSON)
	if err != nil {
		return valueparam.Codec{}, nil, err
	}
	store, err := orchestrator.OpenDatastore(ctx, descriptor)
	if err != nil {
		return valueparam.Codec{}, nil, err
	}
	return valueparam.Codec{Store: store}, descriptor, nil
}

func runRuntimeInvocation(ctx context.Context, workflowPlan *planpb.WorkflowPlan, runtimeSources RuntimeSources, nodeID string, input []byte, project, runID string, descriptor orchestrator.DatastoreDescriptor, retryCount int, mapItemIndex *int) ([]byte, error) {
	if retryCount < 0 {
		return nil, fmt.Errorf("retry count %d must be nonnegative", retryCount)
	}
	sources, err := runtimeSources.resolve(workflowPlan)
	if err != nil {
		return nil, err
	}
	// The container image realizes the plan's requirements; every Python
	// attempt checks it against the archived project before running.
	// Without MASSIVE_PYTHON, a Python node fails its preflight (exit 68).
	pythonEnvironment, _ := controlplane.PythonEnvironment()
	config := orchestrator.IsolatedStepConfig{
		Environment: pythonEnvironment,
		Plan:        workflowPlan, NodeID: nodeID, Datastore: descriptor,
		ProjectID: project, RunID: runID,
		SourceArchives: sources, Attempt: retryCount + 1,
	}
	if mapItemIndex != nil {
		return orchestrator.RunIsolatedMapItem(ctx, config, input, *mapItemIndex)
	}
	return orchestrator.RunIsolatedStep(ctx, config, input)
}

func writeRuntimeOutput(path string, result []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create runtime output directory: %w", err)
	}
	if err := os.WriteFile(path, result, 0o644); err != nil {
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
		fmt.Fprintf(os.Stderr, "massive: %s\nRun massive --help for usage.\n", err)
		os.Exit(2)
	}
	// Task processes run in their own process groups, so a terminal interrupt
	// reaches only this process; cancellation stops them and closes the journal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	parseContext.BindTo(ctx, (*context.Context)(nil))
	parseContext.BindTo(os.Stdout, (*io.Writer)(nil))
	err = parseContext.Run()
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %s\n", strings.TrimSpace(err.Error()))
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor lets a target scheduler classify a failed runtime attempt from
// the process exit alone: runner exit codes pass through, a per-attempt
// timeout exits 124 like timeout(1), and dependency preflight exits 68.
func exitCodeFor(err error) int {
	var preflight *orchestrator.PreflightError
	if errors.As(err, &preflight) {
		return runtimeExitPreflight
	}
	var failure *orchestrator.InvocationFailure
	if !errors.As(err, &failure) {
		return 1
	}
	if failure.TimedOutAfter > 0 {
		return runtimeExitTimeout
	}
	if failure.ExitCode > 0 {
		return failure.ExitCode
	}
	return 1
}

const (
	runtimeExitTimeout = 124
	// runtimeExitPreflight is non-retryable: the image cannot run the project.
	runtimeExitPreflight = 68
)
