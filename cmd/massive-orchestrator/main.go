package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var runErr *orchestrator.RunError
		if errors.As(err, &runErr) && runErr.Result != nil {
			printStepSummaries(runErr.Result)
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
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected subcommand: run")
	}
	if args[0] != "run" {
		return fmt.Errorf("unknown subcommand %q", args[0])
	}

	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	specPath := flags.String("spec", "", "workflow spec JSON file")
	storePath := flags.String("store", "", "local datastore root")
	projectID := flags.String("project", "", "project id")
	runID := flags.String("run-id", "", "run id")
	input := flags.String("input", "", "workflow input JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse run flags: %w", err)
	}
	if *specPath == "" {
		return fmt.Errorf("run requires --spec")
	}
	if *input == "" {
		return fmt.Errorf("run requires --input")
	}

	specData, err := os.ReadFile(*specPath)
	if err != nil {
		return fmt.Errorf("read spec %q: %w", *specPath, err)
	}
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		return err
	}
	compiled, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		return fmt.Errorf("compile workflow plan: %w", err)
	}

	storeRoot := *storePath
	if storeRoot == "" {
		storeRoot, err = defaultStoreRoot()
		if err != nil {
			return err
		}
	}
	project := *projectID
	if project == "" {
		project, err = projectFromGitOrigin()
		if err != nil {
			return err
		}
	}
	sourceRoot, err := filepath.Abs(filepath.Dir(*specPath))
	if err != nil {
		return fmt.Errorf("resolve source package root: %w", err)
	}
	repoRoot, err := repoRoot()
	if err != nil {
		return err
	}

	result, err := orchestrator.Run(context.Background(), orchestrator.RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         project,
		RunID:             *runID,
		RunnerWorkingDir:  repoRoot,
		SourcePackageRoot: sourceRoot,
		SourceManifests:   sourceManifests(workflowSpec),
	}, []byte(*input))
	if err != nil {
		return err
	}

	printStepSummaries(result)
	fmt.Printf("result: %s\n", result.ResultKey)
	return nil
}

func sourceManifests(workflowSpec *spec.WorkflowSpec) map[string][]orchestrator.SourcePackageFile {
	manifests := make(map[string][]orchestrator.SourcePackageFile, len(workflowSpec.SourcePackages))
	for packageID, sourcePackage := range workflowSpec.SourcePackages {
		files := make([]orchestrator.SourcePackageFile, 0, len(sourcePackage.Files))
		for _, file := range sourcePackage.Files {
			files = append(files, orchestrator.SourcePackageFile{Path: file.Path, Hash: file.Hash})
		}
		manifests[packageID] = files
	}
	return manifests
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
