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
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target/argo"
)

const Version = "0.1.0"

type FrontendResult struct {
	Spec        *spec.WorkflowSpec
	Canonical   []byte
	PackageRoot string
}

// Emit loads a language frontend as a process adapter. The only data crossing
// this seam is the canonical WorkflowSpec projection.
func Emit(ctx context.Context, entry string) (*FrontendResult, error) {
	path := entry
	if index := strings.LastIndex(entry, "#"); index >= 0 {
		path = entry[:index]
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow entrypoint: %w", err)
	}
	if filepath.Ext(absolute) != ".py" {
		return nil, fmt.Errorf("0.1 supports Python workflow entrypoints; got %q", entry)
	}
	resolvedEntry := absolute + strings.TrimPrefix(entry, path)

	var command *exec.Cmd
	if python := os.Getenv("MASSIVE_PYTHON"); python != "" {
		command = exec.CommandContext(ctx, python, "-m", "massive.frontend", "emit", resolvedEntry)
	} else {
		frontend := os.Getenv("MASSIVE_PYTHON_FRONTEND")
		if frontend == "" {
			frontend = "massive-python-frontend"
		}
		command = exec.CommandContext(ctx, frontend, "emit", resolvedEntry)
	}
	command.Dir = filepath.Dir(absolute)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic == "" {
			diagnostic = err.Error()
		}
		return nil, fmt.Errorf("Python frontend failed: %s", diagnostic)
	}
	canonicalBytes := stdout.Bytes()
	workflowSpec, err := spec.Parse(canonicalBytes)
	if err != nil {
		return nil, fmt.Errorf("Python frontend emitted an invalid WorkflowSpec: %w", err)
	}
	return &FrontendResult{
		Spec:        workflowSpec,
		Canonical:   append([]byte(nil), canonicalBytes...),
		PackageRoot: filepath.Dir(absolute),
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
	planKey := datastore.MustKey("plans/sha256-" + strings.TrimPrefix(compiled.PlanHash, "sha256:") + "/workflow.json")
	reused := false
	if _, err := store.Put(ctx, planKey, compiled.CanonicalJSON, datastore.PutOptions{ContentType: "application/json", IfAbsent: true}); err != nil {
		if !errors.Is(err, datastore.ErrAlreadyExists) {
			return nil, fmt.Errorf("persist workflow plan: %w", err)
		}
		reused = true
	}

	runnerCommand := []string(nil)
	if python := os.Getenv("MASSIVE_PYTHON"); python != "" {
		runnerCommand = []string{python, "-m", "massive.runner", "{descriptor}"}
	}
	runResult, runErr := orchestrator.Run(ctx, orchestrator.RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         project,
		RunID:             request.RunID,
		RunnerCommand:     runnerCommand,
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
	Frontend             *FrontendResult
	OutputDirectory      string
	ProfileName          string
	ArtifactStoreBinding string
	Namespace            string
	ServiceAccountName   string
	WorkflowTemplateName string
}

type ArgoBundleResult struct {
	PlanHash       string
	DeploymentHash string
	BundleHash     string
	Files          []string
}

func BundleArgo(request ArgoBundleRequest) (*ArgoBundleResult, error) {
	if request.Frontend == nil {
		return nil, errors.New("frontend result is required")
	}
	compiled, err := plan.Compile(request.Frontend.Spec, request.Frontend.Canonical)
	if err != nil {
		return nil, fmt.Errorf("compile workflow plan: %w", err)
	}
	deploymentSpec, deploymentJSON, err := deployment.New(compiled.PlanHash, deployment.Profile{
		Name: request.ProfileName, ArtifactStoreBinding: request.ArtifactStoreBinding,
		Target: deployment.Target{
			Kind: "argo", Namespace: request.Namespace,
			ServiceAccountName:   request.ServiceAccountName,
			WorkflowTemplateName: request.WorkflowTemplateName,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("construct deployment spec: %w", err)
	}
	bundle, err := argo.Compile(compiled.CanonicalJSON, deploymentSpec)
	if err != nil {
		return nil, err
	}
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
		{"deployment-spec.json", deploymentJSON},
		{"workflow-spec.json", request.Frontend.Canonical},
	}
	for _, file := range extra {
		if err := os.WriteFile(filepath.Join(request.OutputDirectory, file.name), file.body, 0o644); err != nil {
			return nil, fmt.Errorf("write bundle file %q: %w", file.name, err)
		}
		files = append(files, file.name)
	}
	return &ArgoBundleResult{
		PlanHash: compiled.PlanHash, DeploymentHash: deploymentSpec.DeploymentHash,
		BundleHash: bundle.Manifest.GetBundleHash(), Files: files,
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
	return "", fmt.Errorf("run requires --project because git origin %q is unsupported", origin)
}
