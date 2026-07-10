package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func main() {
	jsonMode, err := run(os.Args[1:])
	if err == nil {
		return
	}
	var runErr *orchestrator.RunError
	if errors.As(err, &runErr) && runErr.Result != nil {
		// A run was created, so a machine caller still gets the structured
		// object (with per-step statuses); the human diagnostic goes to stderr.
		if jsonMode {
			_ = emitRunJSON(os.Stdout, runErr.Result)
		} else {
			printStepSummaries(runErr.Result)
		}
		fmt.Fprintf(os.Stderr, "massive-orchestrator: %v\n", runErr)
		os.Exit(1)
	}
	var diagnostics *spec.DiagnosticsError
	if errors.As(err, &diagnostics) {
		for _, diagnostic := range diagnostics.Diagnostics {
			fmt.Fprintf(os.Stderr, "invalid workflow spec: %s\n", diagnostic.String())
		}
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "massive-orchestrator: %v\n", err)
	os.Exit(1)
}

// run parses the run subcommand and drives one workflow. It returns whether
// --json was requested (the first value) so main can render failures in the
// matching format even when the run itself errored after flag parsing.
func run(args []string) (bool, error) {
	if len(args) == 0 {
		return false, fmt.Errorf("expected subcommand: run")
	}
	if args[0] != "run" {
		return false, fmt.Errorf("unknown subcommand %q", args[0])
	}

	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	specPath := flags.String("spec", "", "workflow spec JSON file")
	storePath := flags.String("store", "", "local datastore root")
	projectID := flags.String("project", "", "project id")
	runID := flags.String("run-id", "", "run id")
	input := flags.String("input", "", "workflow input JSON")
	sourceRoot := flags.String("source-root", "", "base directory for resolving relative source-package roots (defaults to the spec's directory)")
	jsonOutput := flags.Bool("json", false, "emit a single machine-readable JSON run object to stdout instead of human-readable lines")
	if err := flags.Parse(args[1:]); err != nil {
		return false, fmt.Errorf("parse run flags: %w", err)
	}
	if *specPath == "" {
		return *jsonOutput, fmt.Errorf("run requires --spec")
	}
	if *input == "" {
		return *jsonOutput, fmt.Errorf("run requires --input")
	}

	specData, err := os.ReadFile(*specPath)
	if err != nil {
		return *jsonOutput, fmt.Errorf("read spec %q: %w", *specPath, err)
	}
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		return *jsonOutput, err
	}
	compiled, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		return *jsonOutput, fmt.Errorf("compile workflow plan: %w", err)
	}

	storeRoot := *storePath
	if storeRoot == "" {
		storeRoot, err = defaultStoreRoot()
		if err != nil {
			return *jsonOutput, err
		}
	}
	project := *projectID
	if project == "" {
		project, err = projectFromGitOrigin()
		if err != nil {
			return *jsonOutput, err
		}
	}
	// resolveBase is the directory relative source-package roots resolve
	// against. It defaults to the spec's own directory, but --source-root
	// overrides it so the spec file can live outside the package tree (absolute
	// spec roots still win — see resolvePackageRoot).
	resolveBase, err := filepath.Abs(filepath.Dir(*specPath))
	if err != nil {
		return *jsonOutput, fmt.Errorf("resolve spec directory: %w", err)
	}
	if *sourceRoot != "" {
		resolveBase, err = filepath.Abs(*sourceRoot)
		if err != nil {
			return *jsonOutput, fmt.Errorf("resolve source root %q: %w", *sourceRoot, err)
		}
	}
	repoRoot, err := repoRoot()
	if err != nil {
		return *jsonOutput, err
	}

	// Persist the compiled plan at its content-addressed key so a CLI can
	// observe plan reuse across runs (datastore-layout.md `plans/<plan-key>`).
	// The orchestrator recompiles in-process regardless; this only materializes
	// the artifact. If-absent + content addressing make repeated runs converge.
	if err := persistCompiledPlan(storeRoot, compiled); err != nil {
		return *jsonOutput, err
	}

	result, err := orchestrator.Run(context.Background(), orchestrator.RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         project,
		RunID:             *runID,
		RunnerWorkingDir:  repoRoot,
		SourcePackageRoot: resolveBase,
		SourceManifests:   sourceManifests(workflowSpec, resolveBase),
	}, []byte(*input))
	if err != nil {
		return *jsonOutput, err
	}

	if *jsonOutput {
		return true, emitRunJSON(os.Stdout, result)
	}
	printStepSummaries(result)
	fmt.Printf("result: %s\n", result.ResultKey)
	return false, nil
}

func persistCompiledPlan(storeRoot string, compiled *plan.CompileResult) error {
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		return fmt.Errorf("open datastore to persist plan: %w", err)
	}
	segment := "sha256-" + strings.TrimPrefix(compiled.PlanHash, "sha256:")
	key, err := datastore.ParseKey("plans/" + segment + "/workflow.json")
	if err != nil {
		return fmt.Errorf("build plan key: %w", err)
	}
	if _, err := store.Put(context.Background(), key, compiled.CanonicalJSON, datastore.PutOptions{ContentType: "application/json", IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
		return fmt.Errorf("persist compiled plan: %w", err)
	}
	return nil
}

func sourceManifests(workflowSpec *spec.WorkflowSpec, resolveBase string) map[string]orchestrator.SourcePackageManifest {
	manifests := make(map[string]orchestrator.SourcePackageManifest, len(workflowSpec.SourcePackages))
	for packageID, sourcePackage := range workflowSpec.SourcePackages {
		files := make([]orchestrator.SourcePackageFile, 0, len(sourcePackage.Files))
		for _, file := range sourcePackage.Files {
			files = append(files, orchestrator.SourcePackageFile{Path: file.Path, Hash: file.Hash})
		}
		manifests[packageID] = orchestrator.SourcePackageManifest{
			Root:  resolvePackageRoot(sourcePackage.Root, resolveBase),
			Files: files,
		}
	}
	return manifests
}

// resolvePackageRoot honours the spec's recorded source root: absolute roots
// are used as-is, relative roots resolve against resolveBase, and a missing or
// "." root falls back to resolveBase. resolveBase is dirname(--spec) by default
// or --source-root when supplied.
func resolvePackageRoot(specRoot string, resolveBase string) string {
	if specRoot == "" || specRoot == "." {
		return resolveBase
	}
	if filepath.IsAbs(specRoot) {
		return specRoot
	}
	return filepath.Join(resolveBase, specRoot)
}

// jsonRunStep and jsonRun are the --json wire shape: a single object built from
// the run manifest/result the orchestrator already produced.
type jsonRunStep struct {
	NodeID     string `json:"nodeId"`
	Status     string `json:"status"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

type jsonRun struct {
	RunID     string        `json:"runId"`
	Status    string        `json:"status"`
	ResultKey string        `json:"resultKey,omitempty"`
	Steps     []jsonRunStep `json:"steps"`
}

func emitRunJSON(w io.Writer, result *orchestrator.RunResult) error {
	steps := make([]jsonRunStep, 0, len(result.Steps))
	for _, step := range result.Steps {
		steps = append(steps, jsonRunStep{NodeID: step.NodeID, Status: step.Status, Diagnostic: step.Diagnostic})
	}
	encoder := json.NewEncoder(w)
	return encoder.Encode(jsonRun{
		RunID:     result.RunID,
		Status:    result.Status,
		ResultKey: result.ResultKey,
		Steps:     steps,
	})
}

func printStepSummaries(result *orchestrator.RunResult) {
	for _, step := range result.Steps {
		if step.Diagnostic == "" {
			fmt.Printf("step %s: %s\n", step.NodeID, step.Status)
			continue
		}
		fmt.Printf("step %s: %s: %s\n", step.NodeID, step.Status, step.Diagnostic)
	}
}

func defaultStoreRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default store: %w", err)
	}
	return filepath.Join(home, ".massive", "store"), nil
}

func projectFromGitOrigin() (string, error) {
	output, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", fmt.Errorf("run requires --project when git origin cannot be resolved")
	}
	origin := strings.TrimSpace(string(output))
	if origin == "" {
		return "", fmt.Errorf("run requires --project when git origin is empty")
	}
	project, ok := normalizeGitRemote(origin)
	if !ok {
		return "", fmt.Errorf("run requires --project because git origin %q is not a supported GitHub/GitLab remote", origin)
	}
	return project, nil
}

var (
	httpsRemotePattern = regexp.MustCompile(`^https://(?:github|gitlab)\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`)
	sshRemotePattern   = regexp.MustCompile(`^git@(?:github|gitlab)\.com:([^/]+)/([^/]+?)(?:\.git)?$`)
)

func normalizeGitRemote(origin string) (string, bool) {
	for _, pattern := range []*regexp.Regexp{httpsRemotePattern, sshRemotePattern} {
		matches := pattern.FindStringSubmatch(origin)
		if len(matches) == 3 {
			return matches[1] + "/" + matches[2], true
		}
	}
	return "", false
}

func repoRoot() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if exists(filepath.Join(current, "go.mod")) && exists(filepath.Join(current, "deno.json")) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("could not find repo root from %q", current)
		}
		current = parent
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
