// Package controlplane owns the frontend -> plan -> target workflow used by
// the public CLI. Language SDKs stop at the WorkflowSpec seam; target-specific
// control flow stays here in Go.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/deployment"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/runjournal"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/taskprocess"
)

// Version is injected by the wheel build; source builds report a development version.
var Version = developmentVersion

const developmentVersion = "0.0.0-dev"

type FrontendResult struct {
	Spec        *spec.WorkflowSpec
	Canonical   []byte
	PackageRoot string
	// Environment is the preflight report for a Python workflow, whose
	// interpreter also runs every task. TypeScript workflows have none.
	Environment *environment.Report
}

// CheckEnvironment runs dependency preflight for a Python workflow file without
// importing it. Findings are returned in the report, not as an error.
func CheckEnvironment(ctx context.Context, workflowFile string, scope environment.Scope) (*environment.Report, error) {
	if info, err := os.Stat(workflowFile); err != nil || info.IsDir() || filepath.Ext(workflowFile) != ".py" {
		return nil, fmt.Errorf("dependency preflight requires a Python workflow file, not %q", workflowFile)
	}
	request, err := PythonEnvironment()
	if err != nil {
		return nil, err
	}
	request.ProjectRoot, request.Scope = filepath.Dir(workflowFile), scope
	return environment.Check(ctx, request)
}

// PythonEnvironment is the preflight request for the launching interpreter and
// this control plane; callers choose the project root and scope.
func PythonEnvironment() (environment.Request, error) {
	python := os.Getenv("MASSIVE_PYTHON")
	if python == "" {
		return environment.Request{}, errors.New("Python workflows need the project interpreter; launch the massive command installed by massive-workflows (for example `uv run --locked massive …`) or set MASSIVE_PYTHON")
	}
	return environment.Request{
		Python: python, ControlPlaneVersion: Version,
		// A source-built control plane is paired with its checkout's SDK.
		RequireSDK: Version != developmentVersion,
	}, nil
}

// Emit loads a language frontend as a process adapter. The only data crossing
// this seam is the canonical WorkflowSpec projection, which the frontend writes
// to its --output file; its stdout and stderr belong to author code. Scope
// states whether the Python interpreter will also run tasks (Execution) or only
// emit a graph for a container target (Emission).
func Emit(ctx context.Context, entry string, scope environment.Scope) (*FrontendResult, error) {
	path := entry
	if index := strings.LastIndex(entry, "#"); index >= 0 {
		path = entry[:index]
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow entrypoint: %w", err)
	}
	resolvedEntry := absolute + strings.TrimPrefix(entry, path)

	var argv []string
	var report *environment.Report
	language := "Python"
	packageRoot := filepath.Dir(absolute)
	extension := filepath.Ext(absolute)
	if info, err := os.Stat(absolute); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(absolute, "massive.config.ts")); err != nil {
			return nil, fmt.Errorf("directory entrypoints require massive.config.ts; Python entrypoints must name a .py file: %w", err)
		}
		extension = ".ts"
		packageRoot = absolute
	}
	switch extension {
	case ".py":
		report, err = CheckEnvironment(ctx, absolute, scope)
		if err != nil {
			return nil, err
		}
		if err := report.Err(); err != nil {
			return nil, err
		}
		argv = []string{report.Interpreter.Executable, "-I", "-m", "massive.frontend"}
	case ".ts":
		language = "TypeScript"
		frontend := os.Getenv("MASSIVE_TYPESCRIPT_FRONTEND")
		if frontend == "" {
			frontend = "massive-typescript-frontend"
		}
		argv = []string{frontend}
		for directory := packageRoot; ; directory = filepath.Dir(directory) {
			if _, err := os.Stat(filepath.Join(directory, "massive.config.ts")); err == nil {
				packageRoot = directory
				break
			}
			if filepath.Dir(directory) == directory {
				break
			}
		}
	default:
		return nil, fmt.Errorf("unsupported workflow entrypoint %q; use a Python or TypeScript entrypoint", entry)
	}
	outputDirectory, err := os.MkdirTemp("", "massive-emit-")
	if err != nil {
		return nil, fmt.Errorf("create frontend output directory: %w", err)
	}
	defer os.RemoveAll(outputDirectory)
	output := filepath.Join(outputDirectory, "workflow-spec.json")
	argv = append(argv, "emit", "--output", output, resolvedEntry)
	// Author output from either stream is only a diagnostic.
	var authorOutput bytes.Buffer
	if err := taskprocess.RunTo(ctx, argv, filepath.Dir(absolute), nil, &authorOutput, &authorOutput); err != nil {
		diagnostic := strings.TrimSpace(authorOutput.String())
		if errors.Is(err, exec.ErrWaitDelay) {
			if diagnostic != "" {
				return nil, fmt.Errorf("%s frontend failed: %s: %w", language, diagnostic, err)
			}
			return nil, fmt.Errorf("%s frontend failed: %w", language, err)
		}
		if diagnostic == "" {
			diagnostic = err.Error()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s frontend unavailable; install its language adapter: %s", language, diagnostic)
		}
		return nil, fmt.Errorf("%s frontend failed: %s", language, diagnostic)
	}
	canonicalBytes, err := os.ReadFile(output)
	if err != nil {
		return nil, fmt.Errorf("%s frontend did not write a WorkflowSpec: %w", language, err)
	}
	workflowSpec, err := spec.Parse(canonicalBytes)
	if err != nil {
		return nil, fmt.Errorf("%s frontend emitted an invalid WorkflowSpec: %w", language, err)
	}
	return &FrontendResult{
		Spec:        workflowSpec,
		Canonical:   canonicalBytes,
		PackageRoot: packageRoot,
		Environment: report,
	}, nil
}

type LocalRunRequest struct {
	Frontend *FrontendResult
	Input    []byte
	Store    string
	Project  string
	RunID    string
}

type LocalRunResult struct {
	Run    *orchestrator.RunResult
	Plan   *plan.CompileResult
	Result json.RawMessage
	Reused bool
	Store  string
}

func RunLocal(ctx context.Context, request LocalRunRequest) (*LocalRunResult, error) {
	if request.Frontend == nil {
		return nil, errors.New("frontend result is required")
	}
	compiled, err := plan.Compile(request.Frontend.Spec, request.Frontend.Canonical)
	if err != nil {
		return nil, fmt.Errorf("compile workflow plan: %w", err)
	}
	storeRoot, err := resolveStore(request.Store)
	if err != nil {
		return nil, err
	}
	project := request.Project
	if project == "" {
		project, err = projectFromGitOrigin(request.Frontend.PackageRoot)
		if err != nil {
			return nil, err
		}
	}

	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		return nil, fmt.Errorf("open local datastore: %w", err)
	}
	specKey := datastore.MustKey("specs/sha256-" + strings.TrimPrefix(request.Frontend.Spec.SpecHash, "sha256:") + "/workflow-spec.json")
	if _, err := store.Put(ctx, specKey, request.Frontend.Canonical, datastore.PutOptions{ContentType: "application/json", IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
		return nil, fmt.Errorf("persist workflow spec: %w", err)
	}
	planKey := datastore.MustKey("plans/sha256-" + strings.TrimPrefix(compiled.PlanHash, "sha256:") + "/workflow.json")
	reused := false
	if _, err := store.Put(ctx, planKey, compiled.CanonicalJSON, datastore.PutOptions{ContentType: "application/json", IfAbsent: true}); err != nil {
		if !errors.Is(err, datastore.ErrAlreadyExists) {
			return nil, fmt.Errorf("persist workflow plan: %w", err)
		}
		reused = true
	}

	var runnerCommand []string
	var realized *runjournal.Environment
	if report := request.Frontend.Environment; report != nil {
		if report.Record == nil {
			return nil, errors.New("this workflow was checked only for emission; emit it with environment.Execution to run it locally")
		}
		runnerCommand = orchestrator.PythonRunnerCommand(report.Interpreter.Executable)
		if realized, err = environment.Store(ctx, store, report.Record); err != nil {
			return nil, err
		}
	}
	runResult, runErr := orchestrator.Run(ctx, orchestrator.RunConfig{
		Plan:              compiled.Plan,
		RunnerCommand:     runnerCommand,
		Environment:       realized,
		DatastoreRoot:     storeRoot,
		ProjectID:         project,
		RunID:             request.RunID,
		RunnerWorkingDir:  request.Frontend.PackageRoot,
		SourcePackageRoot: request.Frontend.PackageRoot,
		SourceManifests:   sourceManifests(request.Frontend.Spec, request.Frontend.PackageRoot),
	}, request.Input)
	if runErr != nil {
		return &LocalRunResult{Run: runResultFromError(runErr), Plan: compiled, Reused: reused, Store: storeRoot}, runErr
	}
	key, err := datastore.ParseKey(runResult.ResultKey)
	if err != nil {
		return nil, fmt.Errorf("parse result key: %w", err)
	}
	body, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read workflow result: %w", err)
	}
	return &LocalRunResult{
		Run: runResult, Plan: compiled, Result: append(json.RawMessage(nil), body.Body...),
		Reused: reused, Store: storeRoot,
	}, nil
}

type ArgoBundleRequest struct {
	Frontend                  *FrontendResult
	OutputDirectory           string
	ProfileName               string
	ArtifactStoreBinding      string
	ArtifactCredentialsSecret string
	SecretBindings            map[string]deployment.SecretKeyRef
	Namespace                 string
	ServiceAccountName        string
	WorkflowTemplateName      string
	RuntimeTransport          string
}

type ArgoBundleResult struct {
	PlanHash         string
	DeploymentHash   string
	BundleHash       string
	RuntimeTransport string
	Files            []string
}

func BundleArgo(request ArgoBundleRequest) (*ArgoBundleResult, error) {
	inputs, err := PrepareArgo(request.Frontend)
	if err != nil {
		return nil, err
	}
	compiled, err := CompileArgo(*inputs, deployment.Profile{
		Name: request.ProfileName, ArtifactStoreBinding: request.ArtifactStoreBinding,
		Target: deployment.Target{
			Kind: "argo", Namespace: request.Namespace,
			ServiceAccountName:        request.ServiceAccountName,
			WorkflowTemplateName:      request.WorkflowTemplateName,
			RuntimeTransport:          request.RuntimeTransport,
			ArtifactCredentialsSecret: request.ArtifactCredentialsSecret,
			SecretBindings:            request.SecretBindings,
		},
	})
	if err != nil {
		return nil, err
	}
	bundle := compiled.Bundle
	if err := os.MkdirAll(request.OutputDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create bundle directory: %w", err)
	}
	files := make([]string, 0, len(bundle.Files)+3)
	for _, file := range bundle.Files {
		path := filepath.Join(request.OutputDirectory, file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create bundle path %q: %w", file.Path, err)
		}
		if err := os.WriteFile(path, file.Bytes, 0o644); err != nil {
			return nil, fmt.Errorf("write bundle file %q: %w", file.Path, err)
		}
		files = append(files, file.Path)
	}
	extra := []struct {
		name string
		body []byte
	}{
		{"bundle-manifest.json", bundle.ManifestJSON},
		{"deployment-spec.json", compiled.DeploymentJSON},
		{"workflow-spec.json", inputs.WorkflowSpec},
	}
	for _, file := range extra {
		if err := os.WriteFile(filepath.Join(request.OutputDirectory, file.name), file.body, 0o644); err != nil {
			return nil, fmt.Errorf("write bundle file %q: %w", file.name, err)
		}
		files = append(files, file.name)
	}
	return &ArgoBundleResult{
		PlanHash: compiled.Plan.PlanHash, DeploymentHash: compiled.Deployment.DeploymentHash,
		BundleHash: bundle.Manifest.GetBundleHash(), RuntimeTransport: bundle.Manifest.GetRuntimeTransport(),
		Files: files,
	}, nil
}

func resolveStore(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".massive", "store"), nil
}

func sourceManifests(workflowSpec *spec.WorkflowSpec, root string) map[string]orchestrator.SourcePackageManifest {
	manifests := make(map[string]orchestrator.SourcePackageManifest, len(workflowSpec.SourcePackages))
	for packageID, sourcePackage := range workflowSpec.SourcePackages {
		files := make([]orchestrator.SourcePackageFile, 0, len(sourcePackage.Files))
		for _, file := range sourcePackage.Files {
			files = append(files, orchestrator.SourcePackageFile{Path: file.Path, Hash: file.Hash})
		}
		manifests[packageID] = orchestrator.SourcePackageManifest{Root: root, Files: files}
	}
	return manifests
}

func runResultFromError(err error) *orchestrator.RunResult {
	var runError *orchestrator.RunError
	if errors.As(err, &runError) {
		return runError.Result
	}
	return nil
}

func projectFromGitOrigin(directory string) (string, error) {
	command := exec.Command("git", "config", "--get", "remote.origin.url")
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		return "", errors.New("run requires --project when the workflow package has no supported git origin")
	}
	origin := strings.TrimSpace(string(output))
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`^https://(?:github|gitlab)\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`),
		regexp.MustCompile(`^git@(?:github|gitlab)\.com:([^/]+)/([^/]+?)(?:\.git)?$`),
	} {
		matches := pattern.FindStringSubmatch(origin)
		if len(matches) == 3 {
			return matches[1] + "/" + matches[2], nil
		}
	}
	// Origins can embed credentials, such as a CI job token, so never echo one.
	return "", errors.New("run requires --project because the git origin is not a github.com or gitlab.com repository URL")
}
