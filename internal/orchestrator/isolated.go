package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
)

// IsolatedStepConfig contains the portable inputs available inside one remote
// executor pod. SourceArchives is keyed by the semantic package hash recorded
// in the plan; archive bytes have already been verified during target build.
type IsolatedStepConfig struct {
	Plan           *planpb.WorkflowPlan
	NodeID         string
	DatastoreRoot  string
	ProjectID      string
	RunID          string
	RunnerCommand  []string
	WorkingDir     string
	SourceArchives map[string][]byte
}

// RunIsolatedStep executes exactly one static step through the same descriptor
// and language runner seam as local orchestration. It is the runtime primitive
// used by Argo; DAG readiness and JSON value passing stay target-owned.
func RunIsolatedStep(ctx context.Context, config IsolatedStepConfig, inputJSON []byte) ([]byte, error) {
	if config.Plan == nil {
		return nil, errors.New("isolated step requires a workflow plan")
	}
	if config.DatastoreRoot == "" || config.ProjectID == "" || config.RunID == "" {
		return nil, errors.New("isolated step requires datastore root, project id, and run id")
	}
	if !validSafePathSegment(config.RunID) {
		return nil, &InvalidRunInputError{Field: "run id", Value: config.RunID, Message: "must be a safe path segment"}
	}
	storeRoot, err := filepath.Abs(config.DatastoreRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve isolated datastore root: %w", err)
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		return nil, fmt.Errorf("open isolated datastore: %w", err)
	}
	index, err := buildExecutionIndex(config.Plan)
	if err != nil {
		return nil, err
	}
	node := index.nodesByID[config.NodeID]
	if node == nil || node.GetKind() != "step" {
		return nil, fmt.Errorf("isolated node %q is not a plan step", config.NodeID)
	}
	for _, schema := range config.Plan.GetSchemas() {
		body := []byte(schema.GetCanonicalJson())
		if err := verifyDigest(schema.GetHash(), body); err != nil {
			return nil, err
		}
		key, err := blobKeyForHash(schema.GetHash())
		if err != nil {
			return nil, err
		}
		if _, err := store.Put(ctx, key, body, datastore.PutOptions{ContentType: jsonContentType}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
			return nil, fmt.Errorf("write isolated schema %s: %w", schema.GetHash(), err)
		}
	}
	packages := make(map[string]sourcePackageArtifact, len(config.Plan.GetSourcePackages()))
	for _, sourcePackage := range config.Plan.GetSourcePackages() {
		archive, ok := config.SourceArchives[sourcePackage.GetPackageHash()]
		if !ok || len(archive) == 0 {
			return nil, fmt.Errorf("isolated source archive %s is unavailable", sourcePackage.GetPackageHash())
		}
		if err := VerifySourceArchiveIdentity(archive, sourcePackage.GetPackageHash()); err != nil {
			return nil, err
		}
		key := sourcePackageKey(sourcePackage.GetPackageHash())
		if _, err := store.Put(ctx, datastore.MustKey(key), archive, datastore.PutOptions{ContentType: SourceArchiveContentType, IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
			return nil, fmt.Errorf("write isolated source archive: %w", err)
		}
		packages[sourcePackage.GetPackageId()] = sourcePackageArtifact{
			PackageID: sourcePackage.GetPackageId(), Language: sourcePackage.GetLanguage(),
			PackageHash: sourcePackage.GetPackageHash(), Key: key,
			ArchiveHash: canonical.DigestBytes(archive), ContentType: SourceArchiveContentType,
		}
	}
	index.packagesByID = packages
	input, err := canonical.CanonicalizeJSON(inputJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize isolated step input: %w", err)
	}
	projectKey := NormalizeProjectKey(config.ProjectID)
	inputArtifact := manifestDataArtifact{
		Key:  runInputKey(projectKey, config.RunID, node.GetId(), nil).String(),
		Hash: canonical.DigestBytes(input), ContentType: jsonContentType, Schema: node.GetInputSchema(),
	}
	if _, err := store.Put(ctx, datastore.MustKey(inputArtifact.Key), input, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return nil, fmt.Errorf("write isolated step input: %w", err)
	}
	runConfig := RunConfig{Plan: config.Plan, DatastoreRoot: storeRoot}
	descriptor, err := descriptorForStep(runConfig, projectKey, config.RunID, node, inputArtifact, index)
	if err != nil {
		return nil, err
	}
	invoker := ProcessStepInvoker{CommandTemplate: config.RunnerCommand, WorkingDir: config.WorkingDir, ProcessLimit: 1}
	outcomes, err := invoker.InvokeSteps(ctx, StepInvocationBatch{Steps: []StepInvocation{{Descriptor: descriptor}}, MaxConcurrency: 1})
	if err != nil {
		return nil, err
	}
	if len(outcomes) != 1 || outcomes[0].Status != StatusSucceeded {
		if len(outcomes) == 1 {
			return nil, fmt.Errorf("isolated step %s failed: %s", node.GetId(), outcomes[0].Diagnostic)
		}
		return nil, fmt.Errorf("isolated step %s produced no outcome", node.GetId())
	}
	output, err := resolveOutputArtifact(ctx, store, descriptor, index)
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}
