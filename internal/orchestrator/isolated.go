package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/mapexec"
	"github.com/Sly1029/massive/internal/runjournal"
	"github.com/Sly1029/massive/internal/sourceidentity"
)

// IsolatedStepConfig contains the portable inputs available inside one remote
// executor pod. SourceArchives pins each plan source package, by package hash,
// to the archive the language runner fetches and verifies by digest.
type IsolatedStepConfig struct {
	Plan      *planpb.WorkflowPlan
	NodeID    string
	Datastore DatastoreDescriptor
	ProjectID string
	RunID     string
	// Environment checks the executor's Python interpreter against a Python
	// node's archived project; ProjectRoot is filled in per invocation.
	Environment    environment.Request
	WorkingDir     string
	SourceArchives map[string]SourceArchive
	// Attempt is the 1-based attempt the target is dispatching. Zero means 1.
	// Targets own retry scheduling; this primitive runs exactly one attempt.
	Attempt int
}

// SourceArchive is the runtime half of a target's source transport.
type SourceArchive struct {
	// Digest is the SHA-256 of the archive bytes pinned by the deployment.
	Digest string
	// Body holds embedded-v0 bytes mounted beside the plan. Each invocation
	// verifies and installs them. Without a body the archive must already be
	// published (object-store-v0). Python preflight fetches and verifies it
	// before the runner does; both refuse a missing or mismatched object
	// without a retry.
	Body []byte
}

// EmbeddedSourceArchive pins mounted archive bytes by their own digest.
func EmbeddedSourceArchive(body []byte) SourceArchive {
	return SourceArchive{Digest: canonical.DigestBytes(body), Body: body}
}

func (archive SourceArchive) install(ctx context.Context, store datastore.Datastore, packageHash string) (string, error) {
	if !validSHA256Ref(archive.Digest) {
		return "", fmt.Errorf("isolated source package %s has no pinned archive digest", packageHash)
	}
	key := sourceArchiveKey(packageHash, archive.Digest)
	if archive.Body == nil {
		return key, nil
	}
	if canonical.DigestBytes(archive.Body) != archive.Digest {
		return "", fmt.Errorf("isolated source archive for %s does not match its pinned digest", packageHash)
	}
	if err := sourceidentity.VerifyArchive(archive.Body, packageHash); err != nil {
		return "", err
	}
	if _, err := store.Put(ctx, datastore.MustKey(key), archive.Body, datastore.PutOptions{ContentType: SourceArchiveContentType, IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
		return "", fmt.Errorf("write isolated source archive: %w", err)
	}
	return key, nil
}

// InvocationFailure reports a runner attempt that completed unsuccessfully, so
// a target entrypoint can surface the runner exit code to its own scheduler.
type InvocationFailure struct {
	NodeID        string
	Attempt       int
	MaxAttempts   int
	ExitCode      int
	TimedOutAfter time.Duration
	Diagnostic    string
	// ItemFailure is the outcome a map that collects item failures keeps for
	// this map item once the failure is terminal; nil when such a map would
	// still fail, or for a static step.
	ItemFailure *mapexec.Failure
}

func (failure *InvocationFailure) Error() string {
	return fmt.Sprintf("isolated step %s failed: %s", failure.NodeID, failure.Diagnostic)
}

// Retryable reports whether the target may schedule another attempt.
func (failure *InvocationFailure) Retryable() bool {
	return failure.Attempt < failure.MaxAttempts && retryableOutcome(StepInvocationOutcome{Status: StatusFailed, ExitCode: failure.ExitCode})
}

// RunIsolatedStep executes exactly one static step through the same descriptor
// and language runner seam as local orchestration. It is the runtime primitive
// used by Argo; DAG readiness and JSON value passing stay target-owned.
func RunIsolatedStep(ctx context.Context, config IsolatedStepConfig, inputJSON []byte) ([]byte, error) {
	return runIsolatedInvocation(ctx, config, inputJSON, nil)
}

// RunIsolatedMapItem executes one source-indexed invocation of a static map
// node. Argo uses this primitive for each native fan-out task while retaining
// the same scoped descriptor and artifact identity as local orchestration.
func RunIsolatedMapItem(ctx context.Context, config IsolatedStepConfig, inputJSON []byte, itemIndex int) ([]byte, error) {
	if itemIndex < 0 {
		return nil, fmt.Errorf("isolated map item index %d must be nonnegative", itemIndex)
	}
	return runIsolatedInvocation(ctx, config, inputJSON, &itemIndex)
}

func runIsolatedInvocation(ctx context.Context, config IsolatedStepConfig, inputJSON []byte, mapItemIndex *int) ([]byte, error) {
	if config.Plan == nil {
		return nil, errors.New("isolated step requires a workflow plan")
	}
	if config.Datastore == nil || config.ProjectID == "" || config.RunID == "" {
		return nil, errors.New("isolated step requires datastore descriptor, project id, and run id")
	}
	if !ValidSafePathSegment(config.RunID) {
		return nil, &InvalidRunInputError{Field: "run id", Value: config.RunID, Message: "must be a safe path segment"}
	}
	store, err := OpenDatastore(ctx, config.Datastore)
	if err != nil {
		return nil, fmt.Errorf("open isolated datastore: %w", err)
	}
	index, err := buildExecutionIndex(config.Plan)
	if err != nil {
		return nil, err
	}
	node := index.nodesByID[config.NodeID]
	if node == nil || mapItemIndex == nil && node.GetKind() != "step" || mapItemIndex != nil && node.GetKind() != "map" {
		kind := "step"
		if mapItemIndex != nil {
			kind = "map"
		}
		return nil, fmt.Errorf("isolated node %q is not a plan %s", config.NodeID, kind)
	}
	policy := policyForContract(index.contractsByRef[node.GetContractRef()])
	attempt := max(config.Attempt, 1)
	if attempt > policy.maxAttempts {
		return nil, fmt.Errorf("isolated node %s attempt %d exceeds its retry policy of %d attempts", node.GetId(), attempt, policy.maxAttempts)
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
		archive := config.SourceArchives[sourcePackage.GetPackageHash()]
		key, err := archive.install(ctx, store, sourcePackage.GetPackageHash())
		if err != nil {
			return nil, err
		}
		archiveHash := archive.Digest
		packages[sourcePackage.GetPackageId()] = sourcePackageArtifact{
			PackageID: sourcePackage.GetPackageId(), Language: sourcePackage.GetLanguage(),
			PackageHash: sourcePackage.GetPackageHash(), Key: key,
			ArchiveHash: archiveHash, ContentType: SourceArchiveContentType,
		}
	}
	index.packagesByID = packages
	input, err := canonical.CanonicalizeJSON(inputJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize isolated step input: %w", err)
	}
	projectKey := NormalizeProjectKey(config.ProjectID)
	var scope *ExecutionScope
	inputSchema := node.GetInputSchema()
	if mapItemIndex != nil {
		scope = &ExecutionScope{Frames: []MapItemScopeFrame{{Kind: "map-item", MapID: node.GetId(), Index: *mapItemIndex}}}
		inputSchema = node.GetItemInputSchema()
	}
	inputArtifact := runjournal.DataArtifact{
		Key:  runInputKey(projectKey, config.RunID, node.GetId(), scope).String(),
		Hash: canonical.DigestBytes(input), ContentType: jsonContentType, Schema: inputSchema,
	}
	if _, err := store.Put(ctx, datastore.MustKey(inputArtifact.Key), input, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return nil, fmt.Errorf("write isolated step input: %w", err)
	}
	var descriptor StepInvocationDescriptor
	if mapItemIndex == nil {
		descriptor, err = descriptorForStep(config.Plan.GetPlanHash(), config.Datastore, projectKey, config.RunID, node, inputArtifact, index, attempt)
	} else {
		descriptor, err = descriptorForMapItem(config.Plan.GetPlanHash(), config.Datastore, projectKey, config.RunID, node, inputArtifact, index, *mapItemIndex, attempt)
	}
	if err != nil {
		return nil, err
	}
	var runnerCommand []string
	if descriptor.Symbol.Language == "python" {
		if config.Environment.Python == "" {
			return nil, &PreflightError{NodeID: node.GetId(), Err: errors.New("Python tasks need MASSIVE_PYTHON; launch through the massive command installed by massive-workflows")}
		}
		python, err := isolatedPreflight(ctx, store, config.Environment, descriptor, config.SourceArchives[descriptor.SourcePackage.PackageHash])
		if err != nil {
			return nil, err
		}
		runnerCommand = PythonRunnerCommand(python)
	}
	invoker := ProcessStepInvoker{CommandTemplate: runnerCommand, WorkingDir: config.WorkingDir, ProcessLimit: 1}
	outcomes, err := invoker.InvokeSteps(ctx, StepInvocationBatch{Steps: []StepInvocation{{Descriptor: descriptor, Timeout: policy.timeout}}, MaxConcurrency: 1})
	if err != nil {
		return nil, err
	}
	if len(outcomes) != 1 || outcomes[0].Status != StatusSucceeded {
		if len(outcomes) == 1 {
			failure := &InvocationFailure{
				NodeID: node.GetId(), Attempt: attempt, MaxAttempts: policy.maxAttempts,
				ExitCode: outcomes[0].ExitCode, TimedOutAfter: outcomes[0].TimedOutAfter,
				Diagnostic: runnerDiagnostic(outcomes[0]) + attemptSuffix(attempt, policy),
			}
			if itemFailure, collected := mapItemFailure(outcomes[0], attempt); mapItemIndex != nil && collected {
				failure.ItemFailure = &itemFailure
			}
			return nil, failure
		}
		return nil, fmt.Errorf("isolated step %s produced no outcome", node.GetId())
	}
	output, err := resolveOutputArtifact(ctx, store, descriptor, index)
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

// PublishedSourceArchive reports one object-store source upload.
type PublishedSourceArchive struct {
	PackageHash string `json:"packageHash"`
	ArchiveHash string `json:"archiveHash"`
	Key         string `json:"key"`
	// Created is false when an identical archive was already present.
	Created bool `json:"created"`
}

// PublishSourceArchives installs verified archives at their content-addressed
// keys for object-store-v0 pods. Publication is idempotent; an object with
// different bytes at the key is a conflict and is never overwritten.
func PublishSourceArchives(ctx context.Context, descriptor DatastoreDescriptor, archives map[string][]byte) ([]PublishedSourceArchive, error) {
	store, err := OpenDatastore(ctx, descriptor)
	if err != nil {
		return nil, fmt.Errorf("open publication datastore: %w", err)
	}
	packageHashes := slices.Sorted(maps.Keys(archives))
	published := make([]PublishedSourceArchive, 0, len(archives))
	for _, packageHash := range packageHashes {
		archive := archives[packageHash]
		if err := sourceidentity.VerifyArchive(archive, packageHash); err != nil {
			return nil, err
		}
		archiveHash := canonical.DigestBytes(archive)
		key := sourceArchiveKey(packageHash, archiveHash)
		record := PublishedSourceArchive{PackageHash: packageHash, ArchiveHash: archiveHash, Key: key, Created: true}
		_, err := store.Put(ctx, datastore.MustKey(key), archive, datastore.PutOptions{ContentType: SourceArchiveContentType, IfAbsent: true})
		if errors.Is(err, datastore.ErrAlreadyExists) {
			existing, getErr := store.Get(ctx, datastore.MustKey(key))
			if getErr != nil {
				return nil, fmt.Errorf("verify existing source archive %s: %w", key, getErr)
			}
			if actual := canonical.DigestBytes(existing.Body); actual != record.ArchiveHash || existing.Info.ContentType != SourceArchiveContentType {
				return nil, fmt.Errorf("datastore already holds a different source archive at %s (%s, %s); expected %s", key, actual, existing.Info.ContentType, record.ArchiveHash)
			}
			record.Created, err = false, nil
		}
		if err != nil {
			return nil, fmt.Errorf("publish source archive %s: %w", key, err)
		}
		published = append(published, record)
	}
	return published, nil
}
