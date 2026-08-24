package orchestrator

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/artifact"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/mapexec"
	"github.com/Sly1029/massive/internal/sourceidentity"
	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const jsonContentType = "application/json"

// The shared schemas cap safe segments at 128 JSON characters. The allowed
// character class is ASCII-only, so Go's byte length has the same boundary.
const maxSafePathSegmentLength = 128

var (
	safePathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.@:#-]+$`)
)

func validSHA256Ref(ref string) bool {
	return canonical.IsSHA256Ref(ref)
}

func validSafePathSegment(value string) bool {
	if len(value) > maxSafePathSegmentLength || value == "." || value == ".." || !safePathSegmentPattern.MatchString(value) {
		return false
	}
	_, err := datastore.ParseKey(value)
	return err == nil
}

type executionIndex struct {
	nodesByID       map[string]*planpb.GraphNode
	symbolsByRef    map[string]*planpb.SymbolEntry
	contractsByRef  map[string]*planpb.ExecutionContract
	packagesByID    map[string]sourcePackageArtifact
	inboundByTarget map[string][]*planpb.GraphEdge
	nodeOrder       []string
	stepOrder       []string
	schemaRefs      map[string]bool
	schemaJSON      map[string]string
}

type sourcePackageArtifact struct {
	PackageID   string
	Language    string
	PackageHash string
	Key         string
	// ArchiveHash is the digest of the actual artifact body written to the
	// datastore (the portable source archive), which is distinct from the
	// plan's semantic PackageHash.
	ArchiveHash string
	ContentType string
}

type nodeOutput struct {
	Artifact  manifestDataArtifact
	Published manifestPublishedArtifact
	Body      []byte
}

func Run(ctx context.Context, config RunConfig, inputJSON []byte) (*RunResult, error) {
	if config.Plan == nil {
		return nil, fmt.Errorf("run config requires a workflow plan")
	}
	if config.DatastoreRoot == "" {
		return nil, fmt.Errorf("run config requires a datastore root")
	}
	if config.ProjectID == "" {
		return nil, fmt.Errorf("run config requires an explicit project id")
	}
	if config.SourcePackageRoot == "" {
		return nil, fmt.Errorf("run config requires a source package root")
	}
	datastoreRoot, err := filepath.Abs(config.DatastoreRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve datastore root: %w", err)
	}
	config.DatastoreRoot = datastoreRoot

	projectKey := NormalizeProjectKey(config.ProjectID)
	runID := config.RunID
	if runID == "" {
		runID = uuid.NewString()
	}
	// The run id is interpolated into datastore keys (and thereby filesystem
	// paths). Reject a traversal or otherwise unsafe id up front, using the same
	// segment rules the datastore key parser enforces, before any run artifact
	// is written. A run id must be a single normalized path segment.
	if !validSafePathSegment(runID) {
		return nil, &InvalidRunInputError{Field: "run id", Value: runID, Message: "must be a single safe path segment of at most 128 characters (datastore key segment rules)"}
	}
	if err := validatePlanIdentitySegments(config.Plan); err != nil {
		return nil, err
	}
	// Every source-package hash is interpolated into a snapshot directory name
	// and a datastore key. The spec schema constrains it, but Run also accepts a
	// plan directly, so validate here — before any materialization touches the
	// filesystem — and fail with a typed error rather than a downstream panic.
	for _, sourcePackage := range config.Plan.GetSourcePackages() {
		if !validSHA256Ref(sourcePackage.GetPackageHash()) {
			return nil, &InvalidRunInputError{Field: "source package hash", Value: sourcePackage.GetPackageHash(), Message: "must be a canonical sha256:<64 lowercase hex> digest"}
		}
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: config.DatastoreRoot})
	if err != nil {
		return nil, fmt.Errorf("open local datastore: %w", err)
	}

	index, err := buildExecutionIndex(config.Plan)
	if err != nil {
		return nil, err
	}
	sourcePackages, err := materializePrerequisites(ctx, store, config)
	if err != nil {
		return nil, err
	}
	index.packagesByID = sourcePackages

	workflowInput, err := canonical.CanonicalizeJSON(inputJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workflow input: %w", err)
	}

	manifest := newRunManifest(config.Plan.GetPlanHash(), projectKey, runID, index.stepOrder, index.nodesByID)
	manifestKey := runManifestKey(projectKey, runID)
	if err := writeRunManifest(ctx, store, manifestKey, manifest); err != nil {
		return nil, err
	}

	result := &RunResult{
		RunID:       runID,
		ProjectKey:  projectKey,
		Status:      StatusRunning,
		ManifestKey: manifestKey.String(),
		Steps:       summariesFromManifest(manifest),
	}

	invoker := config.StepInvoker
	if invoker == nil {
		invoker = ProcessStepInvoker{
			CommandTemplate: config.RunnerCommand,
			WorkingDir:      config.RunnerWorkingDir,
		}
	}

	outputs := map[string]nodeOutput{
		config.Plan.GetGraph().GetStartNode(): {
			Artifact: manifestDataArtifact{
				Hash:        canonical.DigestBytes(workflowInput),
				ContentType: jsonContentType,
				Schema:      config.Plan.GetGraph().GetInputSchema(),
			},
			Body: workflowInput,
		},
	}

	selectedCases := make(map[string]string)
	inactive := make(map[string]manifestSkipReason)
	for _, nodeID := range index.nodeOrder {
		node := index.nodesByID[nodeID]
		reason, err := activationSkipReason(node, index.inboundByTarget[nodeID], selectedCases, inactive)
		if err != nil {
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
		}
		if reason != nil {
			inactive[nodeID] = *reason
			switch node.GetKind() {
			case "step", "map":
				markStepSkipped(&manifest, nodeID, *reason)
			case "decision":
				markDecisionSkipped(&manifest, nodeID, *reason)
			case "select":
				// Selects are graph control nodes. Their inactivity is propagated
				// through the activation map rather than materializing an artifact.
			default:
				return failRun(ctx, store, manifestKey, &manifest, result, nodeID, fmt.Sprintf("unsupported plan node kind %q for %q", node.GetKind(), nodeID))
			}
			if err := writeRunManifest(ctx, store, manifestKey, manifest); err != nil {
				return nil, err
			}
			continue
		}
		switch node.GetKind() {
		case "decision":
			selectedCase, output, err := routeDecision(node, index, outputs)
			if err != nil {
				markDecisionFailed(&manifest, nodeID, err.Error())
				return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
			}
			selectedCases[nodeID] = selectedCase
			outputs[nodeID] = output
			manifest.Decisions = append(manifest.Decisions, manifestDecision{
				NodeID:       nodeID,
				Status:       "selected",
				SelectedCase: selectedCase,
			})
			if err := writeRunManifest(ctx, store, manifestKey, manifest); err != nil {
				return nil, err
			}
			continue
		case "select":
			output, err := selectOutput(node, outputs, selectedCases)
			if err != nil {
				return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
			}
			outputs[nodeID] = output
			continue
		case "start", "end":
			continue
		case "map":
			output, err := runMapNode(ctx, store, config, invoker, projectKey, runID, node, index, outputs, &manifest, manifestKey)
			if err != nil {
				return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
			}
			outputs[nodeID] = output
			continue
		case "step":
		default:
			return failRun(ctx, store, manifestKey, &manifest, result, "", fmt.Sprintf("unsupported plan node kind %q for %q", node.GetKind(), nodeID))
		}

		inputBytes, err := inputForNode(node, index.inboundByTarget[nodeID], outputs)
		if err != nil {
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
		}

		inputArtifact := manifestDataArtifact{
			Key:         runInputKey(projectKey, runID, nodeID, nil).String(),
			Hash:        canonical.DigestBytes(inputBytes),
			ContentType: jsonContentType,
			Schema:      node.GetInputSchema(),
		}
		if _, err := store.Put(ctx, datastore.MustKey(inputArtifact.Key), inputBytes, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
			return nil, fmt.Errorf("write input artifact for %s: %w", nodeID, err)
		}

		descriptor, err := descriptorForStep(config, projectKey, runID, node, inputArtifact, index)
		if err != nil {
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
		}

		markAttemptRunning(&manifest, nodeID, inputArtifact)
		if err := writeRunManifest(ctx, store, manifestKey, manifest); err != nil {
			return nil, err
		}

		outcomes, err := invoker.InvokeSteps(ctx, StepInvocationBatch{Steps: []StepInvocation{{Descriptor: descriptor}}})
		if err != nil {
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
		}
		if len(outcomes) != 1 {
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, fmt.Sprintf("step invoker returned %d outcomes, want 1", len(outcomes)))
		}

		if config.Hooks.AfterStepInvocation != nil {
			if err := config.Hooks.AfterStepInvocation(ctx, descriptor); err != nil {
				return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
			}
		}

		outcome := outcomes[0]
		if outcome.Status != StatusSucceeded {
			diagnostic := runnerDiagnostic(outcome)
			markAttemptFailed(&manifest, nodeID, durableRunnerDiagnostic(outcome))
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, diagnostic)
		}

		output, err := resolveOutputArtifact(ctx, store, descriptor, index)
		if err != nil {
			markAttemptFailed(&manifest, nodeID, err.Error())
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
		}
		outputs[nodeID] = output
		markAttemptSucceeded(&manifest, nodeID, output.Published)
		if err := writeRunManifest(ctx, store, manifestKey, manifest); err != nil {
			return nil, err
		}
	}

	resultArtifact, err := resultForEnd(ctx, store, projectKey, runID, config.Plan.GetGraph().GetEndNode(), index, outputs)
	if err != nil {
		return failRun(ctx, store, manifestKey, &manifest, result, "", err.Error())
	}
	manifest.Status = StatusSucceeded
	manifest.Result = &resultArtifact
	if err := writeRunManifest(ctx, store, manifestKey, manifest); err != nil {
		return nil, err
	}

	result.Status = StatusSucceeded
	result.ResultKey = resultArtifact.Key
	result.Steps = summariesFromManifest(manifest)
	return result, nil
}

func validatePlanIdentitySegments(plan *planpb.WorkflowPlan) error {
	graph := plan.GetGraph()
	if graph == nil {
		return &InvalidRunInputError{Field: "plan graph", Message: "is required"}
	}
	if !validSafePathSegment(graph.GetStartNode()) {
		return invalidPlanIdentity("plan graph start node", graph.GetStartNode())
	}
	if !validSafePathSegment(graph.GetEndNode()) {
		return invalidPlanIdentity("plan graph end node", graph.GetEndNode())
	}
	for _, node := range graph.GetNodes() {
		if !validSafePathSegment(node.GetId()) {
			return invalidPlanIdentity("plan graph node id", node.GetId())
		}
		for _, sourceID := range node.GetMergeInputs() {
			if !validSafePathSegment(sourceID) {
				return invalidPlanIdentity("plan graph node merge input", sourceID)
			}
		}
	}
	for _, edge := range graph.GetEdges() {
		if !validSafePathSegment(edge.GetFrom()) {
			return invalidPlanIdentity("plan graph edge from", edge.GetFrom())
		}
		if !validSafePathSegment(edge.GetTo()) {
			return invalidPlanIdentity("plan graph edge to", edge.GetTo())
		}
	}
	return nil
}

func invalidPlanIdentity(field string, value string) *InvalidRunInputError {
	return &InvalidRunInputError{Field: field, Value: value, Message: "must be a single safe path segment of at most 128 characters (descriptor and datastore key rules)"}
}

func materializePrerequisites(ctx context.Context, store datastore.Datastore, config RunConfig) (map[string]sourcePackageArtifact, error) {
	for _, schemaEntry := range config.Plan.GetSchemas() {
		schemaBytes := []byte(schemaEntry.GetCanonicalJson())
		if err := verifyDigest(schemaEntry.GetHash(), schemaBytes); err != nil {
			return nil, fmt.Errorf("schema %s: %w", schemaEntry.GetHash(), err)
		}
		key, err := blobKeyForHash(schemaEntry.GetHash())
		if err != nil {
			return nil, err
		}
		if _, err := store.Put(ctx, key, schemaBytes, datastore.PutOptions{ContentType: jsonContentType}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
			return nil, fmt.Errorf("write schema blob %s: %w", key, err)
		}
	}

	packages := make(map[string]sourcePackageArtifact, len(config.Plan.GetSourcePackages()))
	for _, sourcePackage := range config.Plan.GetSourcePackages() {
		packageID := sourcePackage.GetPackageId()
		// planPackageHash is validated as a strict sha256 ref at the Run entry
		// (before any materialization), so it is safe to derive paths and keys
		// from it here.
		planPackageHash := sourcePackage.GetPackageHash()
		manifest, ok := config.SourceManifests[packageID]
		if !ok {
			return nil, fmt.Errorf("source package %q has no file manifest; cannot verify source integrity before running", packageID)
		}

		// Confirm the threaded manifest is the one the plan was compiled from
		// (cheap, no disk access) before touching the working tree.
		recomputed, err := recomputeSourcePackageHash(manifest.Files)
		if err != nil {
			return nil, fmt.Errorf("recompute source package hash for %q: %w", packageID, err)
		}
		if recomputed != planPackageHash {
			return nil, fmt.Errorf("source package %q drifted since compile: its manifest recomputes to %s but the plan records package hash %s", packageID, recomputed, planPackageHash)
		}

		sourceRoot := manifest.Root
		if sourceRoot == "" {
			sourceRoot = config.SourcePackageRoot
		}
		sourceRoot, err = filepath.Abs(sourceRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve source package root for %q: %w", packageID, err)
		}

		// Snapshot verified source locally before packaging it. The snapshot is an
		// implementation detail: runners receive only the portable archive.
		snapshotDir := sourceSnapshotDir(config.DatastoreRoot, planPackageHash)
		if err := ensureSourceSnapshot(config.DatastoreRoot, sourceRoot, snapshotDir, manifest.Files); err != nil {
			return nil, err
		}

		archive, err := deterministicSourceArchive(snapshotDir, manifest.Files)
		if err != nil {
			return nil, fmt.Errorf("package source archive for %q: %w", packageID, err)
		}
		bodyHash := canonical.DigestBytes(archive)
		// The archive key is templated on the plan's package hash per
		// datastore-layout.md. Writing is if-absent because deterministic bytes
		// make repeated materialization converge.
		key := sourcePackageKey(planPackageHash)
		if _, err := store.Put(ctx, datastore.MustKey(key), archive, datastore.PutOptions{ContentType: SourceArchiveContentType, IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
			return nil, fmt.Errorf("write source package artifact for %q: %w", packageID, err)
		}

		packages[packageID] = sourcePackageArtifact{
			PackageID:   packageID,
			Language:    sourcePackage.GetLanguage(),
			PackageHash: planPackageHash,
			Key:         key,
			ArchiveHash: bodyHash,
			ContentType: SourceArchiveContentType,
		}
	}
	return packages, nil
}

// deterministicSourceArchive emits a USTAR stream with fixed metadata and a
// lexicographic file order. The source manifest has already authenticated each
// snapshot file; this function rejects any non-regular or unsafe entry before
// emitting it so a runner can hydrate the artifact without host paths.
func deterministicSourceArchive(snapshotDir string, files []SourcePackageFile) ([]byte, error) {
	ordered := append([]SourcePackageFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for _, file := range ordered {
		if !safeArchivePath(file.Path) {
			return nil, fmt.Errorf("unsafe source archive path %q", file.Path)
		}
		path := filepath.Join(snapshotDir, filepath.FromSlash(file.Path))
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("stat source snapshot file %q: %w", file.Path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("source snapshot file %q is not regular", file.Path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read source snapshot file %q: %w", file.Path, err)
		}
		if canonical.DigestBytes(content) != file.Hash {
			return nil, fmt.Errorf("source snapshot file %q failed integrity verification", file.Path)
		}
		header := &tar.Header{Name: file.Path, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := writer.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write source archive header for %q: %w", file.Path, err)
		}
		if _, err := writer.Write(content); err != nil {
			return nil, fmt.Errorf("write source archive file %q: %w", file.Path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close source archive: %w", err)
	}
	return body.Bytes(), nil
}

// BuildSourceArchive verifies a frontend's source manifest against real files
// and returns the deterministic archive consumed by isolated and remote step
// runners. Target compilers call this before embedding or uploading source.
func BuildSourceArchive(sourceRoot string, files []SourcePackageFile) ([]byte, error) {
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve source package root: %w", err)
	}
	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file.Path))
		contained, err := pathWithin(root, path)
		if err != nil {
			return nil, fmt.Errorf("resolve source file %q: %w", file.Path, err)
		}
		if !contained {
			return nil, fmt.Errorf("source file %q resolves outside the source package root", file.Path)
		}
	}
	return deterministicSourceArchive(root, files)
}

// SourceArchiveBundleName is the portable filename used to mount one verified
// source archive into a remote runtime pod.
func SourceArchiveBundleName(packageHash string) (string, error) {
	if !validSHA256Ref(packageHash) {
		return "", fmt.Errorf("invalid source package hash %q", packageHash)
	}
	return "source-sha256-" + strings.TrimPrefix(packageHash, "sha256:") + ".tar", nil
}

// VerifySourceArchiveIdentity derives the semantic source-package identity
// from exact tar entry bytes. Remote runtimes use it to reject a mounted asset
// that does not match the package hash committed into the compiled plan.
func VerifySourceArchiveIdentity(archive []byte, expectedHash string) error {
	reader := tar.NewReader(bytes.NewReader(archive))
	files := make([]sourceidentity.File, 0)
	seen := map[string]bool{}
	totalSize := int64(0)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || !safeArchivePath(header.Name) || seen[header.Name] {
			return fmt.Errorf("source archive contains invalid entry %q", header.Name)
		}
		seen[header.Name] = true
		totalSize += header.Size
		if len(files) >= 1024 || totalSize > 50*1024*1024 {
			return errors.New("source archive exceeds source package limits")
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("read source archive entry %q: %w", header.Name, err)
		}
		files = append(files, sourceidentity.File{Path: header.Name, Hash: canonical.DigestBytes(body)})
	}
	sort.Slice(files, func(i, j int) bool { return canonical.LessUTF16(files[i].Path, files[j].Path) })
	actual, err := sourceidentity.Digest(files)
	if err != nil {
		return fmt.Errorf("derive source archive identity: %w", err)
	}
	if actual != expectedHash {
		return fmt.Errorf("source archive identity %s does not match plan package hash %s", actual, expectedHash)
	}
	return nil
}

func safeArchivePath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// sourceSnapshotDir is the content-addressed, immutable snapshot location for a
// package hash, kept under the datastore root (but outside the datastore key
// space, since a leading-dot segment is not a valid key). Because it depends
// only on the store and the package hash, the resulting pointer body is
// deterministic across runs.
func sourceSnapshotDir(storeRoot string, planPackageHash string) string {
	segment := strings.Replace(planPackageHash, "sha256:", "sha256-", 1)
	return filepath.Join(storeRoot, ".snapshots", segment)
}

// ensureSourceSnapshot guarantees a read-only snapshot of the manifest files
// exists at snapshotDir. If a snapshot already verifies against the manifest it
// is reused untouched; otherwise verified source is staged in a temp dir and
// atomically renamed into place (mirroring the local datastore's temp+rename
// idiom), so a concurrent run either wins the rename or converges on the
// identical bytes.
func ensureSourceSnapshot(storeRoot string, sourceRoot string, snapshotDir string, files []SourcePackageFile) error {
	if snapshotMatchesManifest(snapshotDir, files) {
		return nil
	}

	parent := filepath.Dir(snapshotDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create snapshot root %q: %w", parent, err)
	}
	staging, err := os.MkdirTemp(parent, ".tmp-"+filepath.Base(snapshotDir)+"-")
	if err != nil {
		return fmt.Errorf("create snapshot staging dir: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = forceRemoveAll(staging)
		}
	}()

	if err := populateSnapshot(sourceRoot, staging, files); err != nil {
		return err
	}

	if err := os.Rename(staging, snapshotDir); err != nil {
		// A concurrent run may have installed an identical snapshot, or a stale
		// partial one may be blocking the move.
		if snapshotMatchesManifest(snapshotDir, files) {
			return nil
		}
		// Removing a composed path: only ever do so once it is confirmed to be
		// strictly inside the datastore root, never a caller-influenced path.
		contained, err := pathWithin(storeRoot, snapshotDir)
		if err != nil {
			return fmt.Errorf("verify source snapshot %q is inside the datastore: %w", snapshotDir, err)
		}
		if !contained {
			return fmt.Errorf("refusing to remove source snapshot %q: outside datastore root %q", snapshotDir, storeRoot)
		}
		if removeErr := forceRemoveAll(snapshotDir); removeErr != nil {
			return fmt.Errorf("remove stale source snapshot %q: %w", snapshotDir, removeErr)
		}
		if err := os.Rename(staging, snapshotDir); err != nil {
			return fmt.Errorf("install source snapshot %q: %w", snapshotDir, err)
		}
	}
	committed = true
	return nil
}

// pathWithin reports whether target resolves to a location strictly inside
// root, following symlinks on the components that exist. It is the guard for
// any destructive filesystem operation on a composed path.
func pathWithin(root string, target string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	if resolved, err := filepath.EvalSymlinks(targetAbs); err == nil {
		targetAbs = resolved
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false, err
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

// populateSnapshot verifies each manifest file on disk under sourceRoot against
// its recorded content hash and writes verified files into dir as read-only
// (0444), preserving relative paths, then locks the directory tree to 0555. On
// the first hash mismatch it fails loudly, naming the file and explaining the
// drift.
func populateSnapshot(sourceRoot string, dir string, files []SourcePackageFile) error {
	for _, file := range files {
		relPath := filepath.FromSlash(file.Path)
		absPath := filepath.Join(sourceRoot, relPath)
		// Symlink-safe containment: reject not only lexical "../" escapes but a
		// path whose components resolve (via EvalSymlinks) outside sourceRoot,
		// so a symlink in the working tree cannot make ReadFile pull in an
		// external file.
		contained, err := pathWithin(sourceRoot, absPath)
		if err != nil {
			return fmt.Errorf("resolve source file %q: %w", file.Path, err)
		}
		if !contained {
			return fmt.Errorf("source file %q resolves outside the source package root", file.Path)
		}
		content, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Errorf("source file %q could not be read for integrity verification: %w", file.Path, err)
		}
		actual := canonical.DigestBytes(content)
		if actual != file.Hash {
			return fmt.Errorf("source package drifted since compile: file %q hashes to %s but the compiled manifest recorded %s; recompile the workflow or run the plan against the source it was compiled from", file.Path, actual, file.Hash)
		}
		dest := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("create source snapshot directory for %q: %w", file.Path, err)
		}
		if err := os.WriteFile(dest, content, 0o644); err != nil {
			return fmt.Errorf("write source snapshot file %q: %w", file.Path, err)
		}
		// Read-only so a step cannot mutate the snapshot mid-run.
		if err := os.Chmod(dest, 0o444); err != nil {
			return fmt.Errorf("lock source snapshot file %q: %w", file.Path, err)
		}
	}
	return lockSnapshotDirs(dir)
}

// snapshotMatchesManifest reports whether every manifest file is present under
// dir with content that hashes to its recorded digest.
func snapshotMatchesManifest(dir string, files []SourcePackageFile) bool {
	for _, file := range files {
		candidate := filepath.Join(dir, filepath.FromSlash(file.Path))
		contained, err := pathWithin(dir, candidate)
		if err != nil || !contained {
			return false
		}
		content, err := os.ReadFile(candidate)
		if err != nil {
			return false
		}
		if canonical.DigestBytes(content) != file.Hash {
			return false
		}
	}
	return true
}

// lockSnapshotDirs makes every directory under root read-and-execute only so
// the snapshot tree is immutable after population.
func lockSnapshotDirs(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0o555); err != nil {
				return fmt.Errorf("lock source snapshot directory %q: %w", path, err)
			}
		}
		return nil
	})
}

// forceRemoveAll restores write permission on directories (snapshot dirs are
// 0555) before removing the tree, since unlinking children needs a writable
// parent directory.
func forceRemoveAll(root string) error {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// recomputeSourcePackageHash reproduces the versioned SDK source-package
// identity over normalized paths and exact file byte hashes.
func recomputeSourcePackageHash(files []SourcePackageFile) (string, error) {
	entries := make([]sourceidentity.File, 0, len(files))
	for _, file := range files {
		entries = append(entries, sourceidentity.File{Path: file.Path, Hash: file.Hash})
	}
	return sourceidentity.Digest(entries)
}

func descriptorForStep(config RunConfig, projectKey string, runID string, node *planpb.GraphNode, input manifestDataArtifact, index executionIndex) (StepInvocationDescriptor, error) {
	symbol := index.symbolsByRef[node.GetSymbolRef()]
	if symbol == nil {
		return StepInvocationDescriptor{}, fmt.Errorf("missing symbol %q", node.GetSymbolRef())
	}
	sourcePackage, ok := index.packagesByID[symbol.GetPackageId()]
	if !ok {
		return StepInvocationDescriptor{}, fmt.Errorf("missing source package %q", symbol.GetPackageId())
	}
	contract := index.contractsByRef[node.GetContractRef()]
	if contract == nil {
		return StepInvocationDescriptor{}, fmt.Errorf("missing execution contract %q", node.GetContractRef())
	}

	return StepInvocationDescriptor{
		Kind:          "StepInvocationDescriptor",
		SchemaVersion: 2,
		Encoding:      "json-v2",
		PlanHash:      config.Plan.GetPlanHash(),
		ProjectKey:    projectKey,
		RunID:         runID,
		NodeID:        node.GetId(),
		Attempt:       1,
		Symbol: StepSymbolRef{
			PackageID: symbol.GetPackageId(),
			Language:  symbol.GetLanguage(),
			Module:    symbol.GetModule(),
			Export:    symbol.GetExport(),
		},
		SourcePackage: SourcePackageRef{
			PackageID:   sourcePackage.PackageID,
			Language:    sourcePackage.Language,
			PackageHash: sourcePackage.PackageHash,
			SourceArchive: ArtifactRef{
				Key:         sourcePackage.Key,
				Hash:        sourcePackage.ArchiveHash,
				ContentType: sourcePackage.ContentType,
			},
		},
		EnvironmentRef: contract.GetEnvironmentRef(),
		Input: DataArtifactRef{
			Artifact: ArtifactRef{
				Key:         input.Key,
				Hash:        input.Hash,
				ContentType: input.ContentType,
			},
			Schema: input.Schema,
		},
		Output: DataArtifactManifestDestination{
			ManifestKey: runOutputManifestKey(projectKey, runID, node.GetId(), nil, 1).String(),
			Schema:      node.GetOutputSchema(),
		},
		ChannelReads:  []ChannelArtifactRef{},
		ChannelWrites: []ChannelArtifactDestination{},
		Datastore: LocalDatastoreDescriptor{
			Kind: "local",
			Path: config.DatastoreRoot,
		},
	}, nil
}

func descriptorForMapItem(config RunConfig, projectKey string, runID string, node *planpb.GraphNode, input manifestDataArtifact, index executionIndex, itemIndex int) (StepInvocationDescriptor, error) {
	descriptor, err := descriptorForStep(config, projectKey, runID, node, input, index)
	if err != nil {
		return StepInvocationDescriptor{}, err
	}
	scope := &ExecutionScope{Frames: []MapItemScopeFrame{{Kind: "map-item", MapID: node.GetId(), Index: itemIndex}}}
	descriptor.Scope = scope
	descriptor.Input.Schema = node.GetItemInputSchema()
	descriptor.Output.Schema = node.GetItemOutputSchema()
	descriptor.Output.ManifestKey = runOutputManifestKey(projectKey, runID, node.GetId(), scope, 1).String()
	return descriptor, nil
}

func runMapNode(ctx context.Context, store datastore.Datastore, config RunConfig, invoker StepInvoker, projectKey string, runID string, node *planpb.GraphNode, index executionIndex, outputs map[string]nodeOutput, manifest *runManifest, manifestKey datastore.Key) (nodeOutput, error) {
	inputBytes, err := inputForNode(node, index.inboundByTarget[node.GetId()], outputs)
	if err != nil {
		return nodeOutput{}, err
	}
	input := manifestDataArtifact{
		Key:         runInputKey(projectKey, runID, node.GetId(), nil).String(),
		Hash:        canonical.DigestBytes(inputBytes),
		ContentType: jsonContentType,
		Schema:      node.GetInputSchema(),
	}
	if _, err := store.Put(ctx, datastore.MustKey(input.Key), inputBytes, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return nodeOutput{}, fmt.Errorf("write map input artifact for %s: %w", node.GetId(), err)
	}

	markAttemptRunning(manifest, node.GetId(), input)
	if err := writeRunManifest(ctx, store, manifestKey, *manifest); err != nil {
		return nodeOutput{}, err
	}
	items, err := mapexec.Expand(inputBytes)
	if err != nil {
		return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map input expansion failed", err)
	}
	markMapItemsPending(manifest, node.GetId(), items)

	descriptors := make([]StepInvocation, 0, len(items))
	for _, item := range items {
		scope := &ExecutionScope{Frames: []MapItemScopeFrame{{Kind: "map-item", MapID: node.GetId(), Index: item.Index}}}
		itemInput := manifestDataArtifact{
			Key:         runInputKey(projectKey, runID, node.GetId(), scope).String(),
			Hash:        canonical.DigestBytes(item.Body),
			ContentType: jsonContentType,
			Schema:      node.GetItemInputSchema(),
		}
		if _, err := store.Put(ctx, datastore.MustKey(itemInput.Key), item.Body, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
			return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map item input publication failed", fmt.Errorf("write map item %d input: %w", item.Index, err))
		}
		descriptor, err := descriptorForMapItem(config, projectKey, runID, node, itemInput, index, item.Index)
		if err != nil {
			return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map item descriptor construction failed", err)
		}
		descriptors = append(descriptors, StepInvocation{Descriptor: descriptor})
	}
	if err := writeRunManifest(ctx, store, manifestKey, *manifest); err != nil {
		return nodeOutput{}, err
	}

	if uint64(node.GetMaxConcurrency()) > uint64(^uint(0)>>1) {
		return nodeOutput{}, failMapNode(
			ctx, store, manifestKey, manifest, node.GetId(),
			"map concurrency is unsupported by this executor",
			fmt.Errorf("map maxConcurrency %d exceeds the local executor integer range", node.GetMaxConcurrency()),
		)
	}
	outcomes, invokeErr := invoker.InvokeSteps(ctx, StepInvocationBatch{Steps: descriptors, MaxConcurrency: int(node.GetMaxConcurrency())})
	byIndex, err := mapOutcomesByIndex(node.GetId(), outcomes, len(items), invokeErr == nil)
	if err != nil {
		return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map invocation protocol failed", err)
	}

	results := make([]mapexec.Result, 0, len(items))
	firstRunnerFailure := ""
	for itemIndex, descriptor := range descriptors {
		outcome, started := byIndex[itemIndex]
		if !started {
			continue
		}
		markMapItemRunning(manifest, node.GetId(), itemIndex, manifestDataArtifact{
			Key:         descriptor.Descriptor.Input.Artifact.Key,
			Hash:        descriptor.Descriptor.Input.Artifact.Hash,
			ContentType: descriptor.Descriptor.Input.Artifact.ContentType,
			Schema:      descriptor.Descriptor.Input.Schema,
		})
		if config.Hooks.AfterStepInvocation != nil {
			if err := config.Hooks.AfterStepInvocation(ctx, descriptor.Descriptor); err != nil {
				return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map post-invocation hook failed", err)
			}
		}
		if outcome.Status != StatusSucceeded {
			if outcome.Status == stepInvocationStatusCancelled || outcome.Status == stepInvocationStatusInfraFailed {
				// failMapNode terminalizes this started item without classifying a
				// context-killed process as an author-code runner failure.
				continue
			}
			if firstRunnerFailure == "" {
				firstRunnerFailure = runnerDiagnostic(outcome)
			}
			markMapItemFailed(manifest, node.GetId(), itemIndex, durableRunnerDiagnostic(outcome))
			if err := writeRunManifest(ctx, store, manifestKey, *manifest); err != nil {
				return nodeOutput{}, err
			}
			continue
		}
		output, err := resolveOutputArtifact(ctx, store, descriptor.Descriptor, index)
		if err != nil {
			if firstRunnerFailure == "" {
				firstRunnerFailure = err.Error()
			}
			markMapItemFailed(manifest, node.GetId(), itemIndex, "map item output verification failed")
			if writeErr := writeRunManifest(ctx, store, manifestKey, *manifest); writeErr != nil {
				return nodeOutput{}, writeErr
			}
			continue
		}
		markMapItemSucceeded(manifest, node.GetId(), itemIndex, output.Published)
		results = append(results, mapexec.Result{Index: itemIndex, Body: output.Body})
		if err := writeRunManifest(ctx, store, manifestKey, *manifest); err != nil {
			return nodeOutput{}, err
		}
	}
	if invokeErr != nil {
		return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map invocation infrastructure failed", invokeErr)
	}
	if mapHasFailedItem(*manifest, node.GetId()) {
		if firstRunnerFailure == "" {
			firstRunnerFailure = "one or more map items failed"
		}
		return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "one or more map items failed", errors.New(firstRunnerFailure))
	}
	collected, err := mapexec.Collect(len(items), results)
	if err != nil {
		return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map collection failed", err)
	}
	collectionDestination := artifact.Destination{
		ManifestKey: runOutputManifestKey(projectKey, runID, node.GetId(), nil, 1),
		Schema:      node.GetOutputSchema(),
	}
	collectionProducer := artifact.Producer{
		ProjectKey: projectKey,
		PlanHash:   config.Plan.GetPlanHash(),
		RunID:      runID,
		NodeID:     node.GetId(),
		Attempt:    1,
	}
	if _, err := artifact.PublishJSON(ctx, store, collectionDestination, collectionProducer, collected); err != nil {
		return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map collection publication failed", fmt.Errorf("publish map collection: %w", err))
	}
	published, verifiedBody, err := artifact.ResolveJSON(ctx, store, collectionDestination, collectionProducer)
	if err != nil {
		return nodeOutput{}, failMapNode(ctx, store, manifestKey, manifest, node.GetId(), "map collection verification failed", fmt.Errorf("verify map collection: %w", err))
	}
	output := nodeOutputFromPublished(published, verifiedBody)
	markAttemptSucceeded(manifest, node.GetId(), output.Published)
	if err := writeRunManifest(ctx, store, manifestKey, *manifest); err != nil {
		return nodeOutput{}, err
	}
	return output, nil
}

func mapOutcomesByIndex(mapID string, outcomes []StepInvocationOutcome, itemCount int, requireComplete bool) (map[int]StepInvocationOutcome, error) {
	if requireComplete && len(outcomes) != itemCount {
		return nil, fmt.Errorf("map invoker returned %d outcomes, want %d", len(outcomes), itemCount)
	}
	byIndex := make(map[int]StepInvocationOutcome, itemCount)
	for _, outcome := range outcomes {
		if outcome.NodeID != mapID || outcome.Attempt != 1 || outcome.Scope == nil || len(outcome.Scope.Frames) != 1 {
			return nil, fmt.Errorf("map invoker returned an outcome without the expected scoped identity")
		}
		frame := outcome.Scope.Frames[0]
		if frame.Kind != "map-item" || frame.MapID != mapID || frame.Index < 0 || frame.Index >= itemCount {
			return nil, fmt.Errorf("map invoker returned an outcome outside the map item scope")
		}
		if _, exists := byIndex[frame.Index]; exists {
			return nil, fmt.Errorf("map invoker returned duplicate outcome for item %d", frame.Index)
		}
		byIndex[frame.Index] = outcome
	}
	if requireComplete && len(byIndex) != itemCount {
		return nil, fmt.Errorf("map invoker omitted an item outcome")
	}
	return byIndex, nil
}

func nodeOutputFromPublished(published artifact.PublishedJSON, body []byte) nodeOutput {
	return nodeOutput{
		Artifact: manifestDataArtifact{Key: published.Body.Key, Hash: published.Body.Hash, ContentType: published.Body.ContentType, Schema: published.Schema},
		Published: manifestPublishedArtifact{
			Manifest: manifestArtifactRef{Key: published.Manifest.Key, Hash: published.Manifest.Hash, Size: published.Manifest.Size, ContentType: published.Manifest.ContentType},
			Body:     manifestArtifactRef{Key: published.Body.Key, Hash: published.Body.Hash, Size: published.Body.Size, ContentType: published.Body.ContentType},
			Schema:   published.Schema,
		},
		Body: body,
	}
}

func inputForNode(node *planpb.GraphNode, inbound []*planpb.GraphEdge, outputs map[string]nodeOutput) ([]byte, error) {
	if len(node.GetMergeInputs()) == 0 {
		if len(inbound) != 1 {
			return nil, fmt.Errorf("local runner v0 requires exactly one input edge for %q", node.GetId())
		}
		output, ok := outputs[inbound[0].GetFrom()]
		if !ok {
			return nil, fmt.Errorf("missing output from %q for %q", inbound[0].GetFrom(), node.GetId())
		}
		return output.Body, nil
	}

	inboundSources := make(map[string]bool, len(inbound))
	for _, edge := range inbound {
		inboundSources[edge.GetFrom()] = true
	}
	for _, source := range node.GetMergeInputs() {
		if !inboundSources[source] {
			return nil, fmt.Errorf("merge step %q is missing edge from %q", node.GetId(), source)
		}
	}
	if len(inbound) != len(node.GetMergeInputs()) {
		return nil, fmt.Errorf("merge step %q has edges that are not declared merge inputs", node.GetId())
	}

	var out bytes.Buffer
	out.WriteByte('[')
	for index, source := range node.GetMergeInputs() {
		if index > 0 {
			out.WriteByte(',')
		}
		output, ok := outputs[source]
		if !ok {
			return nil, fmt.Errorf("missing output from %q for %q", source, node.GetId())
		}
		out.Write(output.Body)
	}
	out.WriteByte(']')
	return canonical.CanonicalizeJSON(out.Bytes())
}

// routeDecision is deliberately data-only: it reads the already-published
// classifier value, validates the selected branch schema, and forwards the
// same immutable artifact to the conditional edge. It never invokes user code.
func routeDecision(node *planpb.GraphNode, index executionIndex, outputs map[string]nodeOutput) (string, nodeOutput, error) {
	inbound := index.inboundByTarget[node.GetId()]
	if len(inbound) != 1 {
		return "", nodeOutput{}, fmt.Errorf("decision %q requires exactly one input edge", node.GetId())
	}
	inputBytes, err := inputForNode(node, inbound, outputs)
	if err != nil {
		return "", nodeOutput{}, fmt.Errorf("decision %q input: %w", node.GetId(), err)
	}
	input, ok := outputs[inbound[0].GetFrom()]
	if !ok {
		return "", nodeOutput{}, fmt.Errorf("decision %q input source %q has no output", node.GetId(), inbound[0].GetFrom())
	}
	if input.Artifact.Schema != node.GetInputSchema() {
		return "", nodeOutput{}, fmt.Errorf("decision %q input schema %q does not match declared schema %q", node.GetId(), input.Artifact.Schema, node.GetInputSchema())
	}

	var value map[string]json.RawMessage
	if err := json.Unmarshal(inputBytes, &value); err != nil {
		return "", nodeOutput{}, fmt.Errorf("decision %q selector %q requires a JSON object: %w", node.GetId(), node.GetSelector(), err)
	}
	rawTag, exists := value[node.GetSelector()]
	if !exists {
		return "", nodeOutput{}, fmt.Errorf("decision %q selector %q is missing", node.GetId(), node.GetSelector())
	}
	var tag string
	if err := json.Unmarshal(rawTag, &tag); err != nil {
		return "", nodeOutput{}, fmt.Errorf("decision %q selector %q must be a string", node.GetId(), node.GetSelector())
	}
	for _, decisionCase := range node.GetCases() {
		if decisionCase.GetTag() != tag {
			continue
		}
		schema := index.schemaJSON[decisionCase.GetSchema()]
		if schema == "" {
			return "", nodeOutput{}, fmt.Errorf("decision %q case %q references unavailable schema %q", node.GetId(), tag, decisionCase.GetSchema())
		}
		if err := validateJSONAgainstSchema(schema, inputBytes); err != nil {
			// Schema-library errors can include a rendered instance. The manifest is
			// durable user-visible control-plane state, so retain the actionable
			// route identity without copying classified data into its diagnostic.
			return "", nodeOutput{}, fmt.Errorf("decision %q selected case %q does not satisfy its schema", node.GetId(), tag)
		}
		return tag, input, nil
	}
	return "", nodeOutput{}, fmt.Errorf("decision %q selector %q selected an undeclared case", node.GetId(), node.GetSelector())
}

// selectOutput aliases the chosen branch's DataArtifactRef and body. It must
// not republish the data: the source attempt already committed it manifest-last.
func selectOutput(node *planpb.GraphNode, outputs map[string]nodeOutput, selectedCases map[string]string) (nodeOutput, error) {
	tag, exists := selectedCases[node.GetDecisionRef()]
	if !exists {
		return nodeOutput{}, fmt.Errorf("select %q has no durable selection for decision %q", node.GetId(), node.GetDecisionRef())
	}
	for _, input := range node.GetSelectInputs() {
		if input.GetCase() != tag {
			continue
		}
		output, exists := outputs[input.GetSource()]
		if !exists {
			return nodeOutput{}, fmt.Errorf("select %q selected source %q has no output", node.GetId(), input.GetSource())
		}
		if output.Artifact.Schema != node.GetOutputSchema() {
			return nodeOutput{}, fmt.Errorf("select %q output schema %q does not match selected source schema %q", node.GetId(), node.GetOutputSchema(), output.Artifact.Schema)
		}
		return output, nil
	}
	return nodeOutput{}, fmt.Errorf("select %q has no input for selected case %q", node.GetId(), tag)
}

// activationSkipReason is the scheduler's control-region gate. Every node
// kind uses it before execution: an inactive outer branch suppresses nested
// decisions and selects as well as ordinary steps. Selects deliberately read
// only their chosen input; the other select inputs are inactive by design.
func activationSkipReason(node *planpb.GraphNode, inbound []*planpb.GraphEdge, selectedCases map[string]string, inactive map[string]manifestSkipReason) (*manifestSkipReason, error) {
	if node.GetKind() == "select" {
		if reason, exists := inactive[node.GetDecisionRef()]; exists {
			return &reason, nil
		}
		selectedCase, exists := selectedCases[node.GetDecisionRef()]
		if !exists {
			return nil, fmt.Errorf("select %q depends on unresolved decision %q", node.GetId(), node.GetDecisionRef())
		}
		for _, input := range node.GetSelectInputs() {
			if input.GetCase() != selectedCase {
				continue
			}
			if reason, exists := inactive[input.GetSource()]; exists {
				return nil, fmt.Errorf(
					"select %q selected source %q is inactive because decision %q did not select case %q",
					node.GetId(), input.GetSource(), reason.DecisionID, reason.Case,
				)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("select %q has no input for selected case %q", node.GetId(), selectedCase)
	}

	for _, edge := range inbound {
		if reason, exists := inactive[edge.GetFrom()]; exists {
			return &reason, nil
		}
		if edge.GetCase() != "" {
			selectedCase, exists := selectedCases[edge.GetFrom()]
			if !exists {
				return nil, fmt.Errorf("conditional node %q depends on unresolved decision %q", node.GetId(), edge.GetFrom())
			}
			if selectedCase != edge.GetCase() {
				return &manifestSkipReason{Kind: "decision-not-selected", DecisionID: edge.GetFrom(), Case: edge.GetCase()}, nil
			}
		}
	}
	return nil, nil
}

func validateJSONAgainstSchema(schemaJSON string, valueJSON []byte) error {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(schemaJSON)))
	if err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(valueJSON))
	if err != nil {
		return fmt.Errorf("decode value: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("decision-case.schema.json", document); err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	schema, err := compiler.Compile("decision-case.schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return err
	}
	return nil
}

func resolveOutputArtifact(ctx context.Context, store datastore.Datastore, descriptor StepInvocationDescriptor, index executionIndex) (nodeOutput, error) {
	if !index.schemaRefs[descriptor.Output.Schema] {
		return nodeOutput{}, fmt.Errorf("output schema ref %s is not present in the plan", descriptor.Output.Schema)
	}
	manifestKey, err := datastore.ParseKey(descriptor.Output.ManifestKey)
	if err != nil {
		return nodeOutput{}, err
	}
	published, body, err := artifact.ResolveJSON(ctx, store, artifact.Destination{
		ManifestKey: manifestKey,
		Schema:      descriptor.Output.Schema,
	}, artifact.Producer{
		ProjectKey: descriptor.ProjectKey,
		PlanHash:   descriptor.PlanHash,
		RunID:      descriptor.RunID,
		NodeID:     descriptor.NodeID,
		Attempt:    descriptor.Attempt,
		Scope:      descriptor.Scope,
	})
	if err != nil {
		return nodeOutput{}, fmt.Errorf("resolve output artifact manifest %s: %w", manifestKey, err)
	}

	return nodeOutput{
		Artifact: manifestDataArtifact{
			Key:         published.Body.Key,
			Hash:        published.Body.Hash,
			ContentType: published.Body.ContentType,
			Schema:      descriptor.Output.Schema,
		},
		Published: manifestPublishedArtifact{
			Manifest: manifestArtifactRef{
				Key:         published.Manifest.Key,
				Hash:        published.Manifest.Hash,
				Size:        published.Manifest.Size,
				ContentType: published.Manifest.ContentType,
			},
			Body: manifestArtifactRef{
				Key:         published.Body.Key,
				Hash:        published.Body.Hash,
				Size:        published.Body.Size,
				ContentType: published.Body.ContentType,
			},
			Schema: published.Schema,
		},
		Body: body,
	}, nil
}

func resultForEnd(ctx context.Context, store datastore.Datastore, projectKey string, runID string, endNode string, index executionIndex, outputs map[string]nodeOutput) (manifestDataArtifact, error) {
	inbound := index.inboundByTarget[endNode]
	if len(inbound) != 1 {
		return manifestDataArtifact{}, fmt.Errorf("local runner v0 requires exactly one input edge for %q", endNode)
	}
	output, ok := outputs[inbound[0].GetFrom()]
	if !ok {
		return manifestDataArtifact{}, fmt.Errorf("missing output from %q for %q", inbound[0].GetFrom(), endNode)
	}

	key := runResultKey(projectKey, runID)
	result := manifestDataArtifact{
		Key:         key.String(),
		Hash:        canonical.DigestBytes(output.Body),
		ContentType: jsonContentType,
		Schema:      output.Artifact.Schema,
	}
	if _, err := store.Put(ctx, key, output.Body, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return manifestDataArtifact{}, fmt.Errorf("write result artifact: %w", err)
	}
	return result, nil
}

func buildExecutionIndex(workflowPlan *planpb.WorkflowPlan) (executionIndex, error) {
	graph := workflowPlan.GetGraph()
	if graph == nil {
		return executionIndex{}, fmt.Errorf("workflow plan is missing graph")
	}
	nodeOrder, err := topologicalPlanOrder(graph)
	if err != nil {
		return executionIndex{}, err
	}

	index := executionIndex{
		nodesByID:       make(map[string]*planpb.GraphNode, len(graph.GetNodes())),
		symbolsByRef:    make(map[string]*planpb.SymbolEntry, len(workflowPlan.GetSymbols())),
		contractsByRef:  make(map[string]*planpb.ExecutionContract, len(workflowPlan.GetContracts())),
		inboundByTarget: make(map[string][]*planpb.GraphEdge, len(graph.GetNodes())),
		nodeOrder:       make([]string, 0, len(graph.GetNodes())),
		stepOrder:       make([]string, 0, len(graph.GetNodes())),
		schemaRefs:      make(map[string]bool, len(workflowPlan.GetSchemas())),
		schemaJSON:      make(map[string]string, len(workflowPlan.GetSchemas())),
	}
	for _, schemaEntry := range workflowPlan.GetSchemas() {
		index.schemaRefs[schemaEntry.GetHash()] = true
		index.schemaJSON[schemaEntry.GetHash()] = schemaEntry.GetCanonicalJson()
	}
	for _, symbol := range workflowPlan.GetSymbols() {
		index.symbolsByRef[symbol.GetSymbolRef()] = symbol
	}
	for _, contract := range workflowPlan.GetContracts() {
		index.contractsByRef[contract.GetContractRef()] = contract
	}
	for _, node := range graph.GetNodes() {
		index.nodesByID[node.GetId()] = node
		index.inboundByTarget[node.GetId()] = nil
	}
	for _, edge := range graph.GetEdges() {
		if index.nodesByID[edge.GetFrom()] == nil {
			return executionIndex{}, fmt.Errorf("graph edge source %q does not exist", edge.GetFrom())
		}
		if index.nodesByID[edge.GetTo()] == nil {
			return executionIndex{}, fmt.Errorf("graph edge target %q does not exist", edge.GetTo())
		}
		index.inboundByTarget[edge.GetTo()] = append(index.inboundByTarget[edge.GetTo()], edge)
	}
	for _, nodeID := range nodeOrder {
		node := index.nodesByID[nodeID]
		if node.GetKind() != "start" && node.GetKind() != "end" {
			index.nodeOrder = append(index.nodeOrder, nodeID)
		}
		if node.GetKind() == "step" || node.GetKind() == "map" {
			index.stepOrder = append(index.stepOrder, nodeID)
		}
	}
	return index, nil
}

func topologicalPlanOrder(graph *planpb.GraphIR) ([]string, error) {
	nodes := make(map[string]bool, len(graph.GetNodes()))
	indegree := make(map[string]int, len(graph.GetNodes()))
	adjacency := make(map[string][]string, len(graph.GetNodes()))
	for _, node := range graph.GetNodes() {
		if node.GetId() == "" {
			return nil, errors.New("workflow plan contains a graph node without an id")
		}
		if nodes[node.GetId()] {
			return nil, fmt.Errorf("workflow plan contains duplicate graph node id %q", node.GetId())
		}
		nodes[node.GetId()] = true
		indegree[node.GetId()] = 0
	}
	for _, edge := range graph.GetEdges() {
		if !nodes[edge.GetFrom()] || !nodes[edge.GetTo()] {
			return nil, fmt.Errorf("workflow plan edge %q -> %q references an unknown node", edge.GetFrom(), edge.GetTo())
		}
		adjacency[edge.GetFrom()] = append(adjacency[edge.GetFrom()], edge.GetTo())
		indegree[edge.GetTo()]++
	}
	ready := make([]string, 0, len(nodes))
	for nodeID, degree := range indegree {
		if degree == 0 {
			ready = append(ready, nodeID)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return canonical.LessUTF16(ready[i], ready[j]) })
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		order = append(order, current)
		for _, next := range adjacency[current] {
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return canonical.LessUTF16(ready[i], ready[j]) })
	}
	if len(order) != len(nodes) {
		return nil, errors.New("workflow plan graph contains a cycle")
	}
	return order, nil
}

func newRunManifest(planHash string, projectKey string, runID string, stepOrder []string, nodesByID map[string]*planpb.GraphNode) runManifest {
	steps := make([]manifestStep, 0, len(stepOrder))
	for _, stepID := range stepOrder {
		step := manifestStep{NodeID: stepID, Status: StatusPending, Attempts: []manifestAttempt{}}
		if nodesByID[stepID].GetKind() == "map" {
			items := []manifestMapItem{}
			step.Items = &items
		}
		steps = append(steps, step)
	}
	return runManifest{
		Kind:          "RunManifest",
		SchemaVersion: 3,
		Encoding:      "json-v3",
		PlanHash:      planHash,
		ProjectKey:    projectKey,
		RunID:         runID,
		Status:        StatusRunning,
		Steps:         steps,
		Decisions:     []manifestDecision{},
	}
}

func findManifestStep(manifest *runManifest, nodeID string) *manifestStep {
	for index := range manifest.Steps {
		if manifest.Steps[index].NodeID == nodeID {
			return &manifest.Steps[index]
		}
	}
	return nil
}

func markMapItemsPending(manifest *runManifest, nodeID string, items []mapexec.Item) {
	step := findManifestStep(manifest, nodeID)
	if step == nil || step.Items == nil {
		return
	}
	journalItems := make([]manifestMapItem, len(items))
	for index, item := range items {
		journalItems[index] = manifestMapItem{Index: item.Index, Status: StatusPending, Attempts: []manifestAttempt{}}
	}
	step.Items = &journalItems
}

func markMapItemRunning(manifest *runManifest, nodeID string, itemIndex int, input manifestDataArtifact) {
	step := findManifestStep(manifest, nodeID)
	if step == nil || step.Items == nil || itemIndex < 0 || itemIndex >= len(*step.Items) {
		return
	}
	item := &(*step.Items)[itemIndex]
	item.Status = StatusRunning
	item.Attempts = []manifestAttempt{{Attempt: 1, Status: StatusRunning, Input: input}}
}

func markMapItemSucceeded(manifest *runManifest, nodeID string, itemIndex int, output manifestPublishedArtifact) {
	step := findManifestStep(manifest, nodeID)
	if step == nil || step.Items == nil || itemIndex < 0 || itemIndex >= len(*step.Items) {
		return
	}
	item := &(*step.Items)[itemIndex]
	item.Status = StatusSucceeded
	item.Attempts[0].Status = StatusSucceeded
	item.Attempts[0].Output = &output
}

func markMapItemFailed(manifest *runManifest, nodeID string, itemIndex int, diagnostic string) {
	step := findManifestStep(manifest, nodeID)
	if step == nil || step.Items == nil || itemIndex < 0 || itemIndex >= len(*step.Items) {
		return
	}
	item := &(*step.Items)[itemIndex]
	item.Status = StatusFailed
	if len(item.Attempts) == 0 {
		item.Attempts = []manifestAttempt{{Attempt: 1, Status: StatusFailed, Diagnostic: diagnostic}}
		return
	}
	item.Attempts[0].Status = StatusFailed
	item.Attempts[0].Diagnostic = diagnostic
}

func mapHasFailedItem(manifest runManifest, nodeID string) bool {
	for _, step := range manifest.Steps {
		if step.NodeID != nodeID || step.Items == nil {
			continue
		}
		for _, item := range *step.Items {
			if item.Status != StatusSucceeded {
				return true
			}
		}
	}
	return false
}

func failMapNode(ctx context.Context, store datastore.Datastore, manifestKey datastore.Key, manifest *runManifest, nodeID string, durableDiagnostic string, cause error) error {
	markUnfinishedMapItemsTerminal(manifest, nodeID)
	markAttemptFailed(manifest, nodeID, durableDiagnostic)
	if err := writeRunManifest(ctx, store, manifestKey, *manifest); err != nil {
		return err
	}
	return cause
}

func markUnfinishedMapItemsTerminal(manifest *runManifest, nodeID string) {
	step := findManifestStep(manifest, nodeID)
	if step == nil || step.Items == nil {
		return
	}
	for index := range *step.Items {
		item := &(*step.Items)[index]
		if item.Status == StatusPending {
			item.Status = StatusNotStarted
			item.Diagnostic = "map item was not started because the map failed"
			continue
		}
		if item.Status == StatusRunning {
			item.Status = StatusFailed
			item.Attempts[0].Status = StatusFailed
			item.Attempts[0].Diagnostic = "map item did not complete"
		}
	}
}

func markAttemptRunning(manifest *runManifest, nodeID string, input manifestDataArtifact) {
	for index := range manifest.Steps {
		if manifest.Steps[index].NodeID != nodeID {
			continue
		}
		manifest.Steps[index].Status = StatusRunning
		manifest.Steps[index].Attempts = []manifestAttempt{{
			Attempt: 1,
			Status:  StatusRunning,
			Input:   input,
		}}
		return
	}
}

func markAttemptSucceeded(manifest *runManifest, nodeID string, output manifestPublishedArtifact) {
	for index := range manifest.Steps {
		if manifest.Steps[index].NodeID != nodeID {
			continue
		}
		manifest.Steps[index].Status = StatusSucceeded
		manifest.Steps[index].Attempts[0].Status = StatusSucceeded
		manifest.Steps[index].Attempts[0].Output = &output
		return
	}
}

func markAttemptFailed(manifest *runManifest, nodeID string, diagnostic string) {
	for index := range manifest.Steps {
		if manifest.Steps[index].NodeID != nodeID {
			continue
		}
		manifest.Steps[index].Status = StatusFailed
		if len(manifest.Steps[index].Attempts) == 0 {
			manifest.Steps[index].Attempts = []manifestAttempt{{Attempt: 1, Status: StatusFailed, Diagnostic: diagnostic}}
			return
		}
		manifest.Steps[index].Attempts[0].Status = StatusFailed
		manifest.Steps[index].Attempts[0].Diagnostic = diagnostic
		return
	}
}

func markStepSkipped(manifest *runManifest, nodeID string, reason manifestSkipReason) {
	for index := range manifest.Steps {
		if manifest.Steps[index].NodeID != nodeID {
			continue
		}
		manifest.Steps[index].Status = StatusSkipped
		manifest.Steps[index].SkipReason = &reason
		return
	}
}

func markDecisionFailed(manifest *runManifest, nodeID string, diagnostic string) {
	manifest.Decisions = append(manifest.Decisions, manifestDecision{
		NodeID:     nodeID,
		Status:     "failed",
		Diagnostic: diagnostic,
	})
}

func markDecisionSkipped(manifest *runManifest, nodeID string, reason manifestSkipReason) {
	manifest.Decisions = append(manifest.Decisions, manifestDecision{
		NodeID:     nodeID,
		Status:     StatusSkipped,
		SkipReason: &reason,
	})
}

func failRun(ctx context.Context, store datastore.Datastore, manifestKey datastore.Key, manifest *runManifest, result *RunResult, stepID string, diagnostic string) (*RunResult, error) {
	manifest.Status = StatusFailed
	if err := writeRunManifest(ctx, store, manifestKey, *manifest); err != nil {
		return nil, err
	}
	result.Status = StatusFailed
	result.Steps = summariesFromManifest(*manifest)
	return result, &RunError{StepID: stepID, Diagnostic: diagnostic, Result: result}
}

func summariesFromManifest(manifest runManifest) []StepSummary {
	summaries := make([]StepSummary, 0, len(manifest.Steps))
	for _, step := range manifest.Steps {
		diagnostic := ""
		if len(step.Attempts) > 0 {
			diagnostic = step.Attempts[0].Diagnostic
		}
		summaries = append(summaries, StepSummary{NodeID: step.NodeID, Status: step.Status, Diagnostic: diagnostic})
	}
	return summaries
}

func writeRunManifest(ctx context.Context, store datastore.Datastore, key datastore.Key, manifest runManifest) error {
	body, err := marshalCanonicalJSON(manifest)
	if err != nil {
		return fmt.Errorf("marshal run manifest: %w", err)
	}
	if _, err := store.Put(ctx, key, body, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return fmt.Errorf("write run manifest %s: %w", key, err)
	}
	return nil
}

func runnerDiagnostic(outcome StepInvocationOutcome) string {
	label := "runner failed"
	switch outcome.ExitCode {
	case 64:
		label = "descriptor-resolution-failure"
	case 65:
		label = "schema-validation-failure"
	case 66:
		label = "step-execution-failure"
	}
	if outcome.Diagnostic == "" {
		return fmt.Sprintf("%s (exit %d)", label, outcome.ExitCode)
	}
	return fmt.Sprintf("%s (exit %d): %s", label, outcome.ExitCode, outcome.Diagnostic)
}

func durableRunnerDiagnostic(outcome StepInvocationOutcome) string {
	label := "runner-failure"
	switch outcome.ExitCode {
	case 64:
		label = "descriptor-resolution-failure"
	case 65:
		label = "schema-validation-failure"
	case 66:
		label = "step-execution-failure"
	}
	return fmt.Sprintf("%s (exit %d)", label, outcome.ExitCode)
}

func NormalizeProjectKey(projectID string) string {
	trimmed := strings.Trim(projectID, " \t\r\n")
	// ASCII-only lowercasing per datastore-layout.md project-key
	// normalization; Unicode-aware lowercasing would diverge from other
	// language implementations.
	normalized := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, trimmed)
	sum := sha256.Sum256([]byte(normalized))
	return "sha256-" + hex.EncodeToString(sum[:])
}

func runManifestKey(projectKey string, runID string) datastore.Key {
	return datastore.MustKey("projects/" + projectKey + "/runs/" + runID + "/run-manifest.json")
}

func runInputKey(projectKey string, runID string, stepID string, scope *ExecutionScope) datastore.Key {
	key := "projects/" + projectKey + "/runs/" + runID + "/inputs/" + stepID
	if scope != nil {
		key += "/scopes"
		for _, frame := range scope.Frames {
			key += "/maps/" + frame.MapID + "/items/" + fmt.Sprint(frame.Index)
		}
	}
	return datastore.MustKey(key + ".json")
}

func runOutputManifestKey(projectKey string, runID string, stepID string, scope *ExecutionScope, attempt int) datastore.Key {
	key := "projects/" + projectKey + "/runs/" + runID + "/steps/" + stepID
	if scope != nil {
		key += "/scopes"
		for _, frame := range scope.Frames {
			key += "/maps/" + frame.MapID + "/items/" + fmt.Sprint(frame.Index)
		}
	}
	return datastore.MustKey(key + "/" + fmt.Sprint(attempt) + "/output-manifest.json")
}

func runResultKey(projectKey string, runID string) datastore.Key {
	return datastore.MustKey("projects/" + projectKey + "/runs/" + runID + "/result.json")
}

func sourcePackageKey(hash string) string {
	return "packages/" + strings.Replace(hash, "sha256:", "sha256-", 1) + "/source.tar"
}

func blobKeyForHash(hash string) (datastore.Key, error) {
	digest, err := digestHex(hash)
	if err != nil {
		return datastore.Key{}, err
	}
	return datastore.BlobKeySHA256Hex(digest)
}

func digestHex(hash string) (string, error) {
	digest, ok := strings.CutPrefix(hash, "sha256:")
	if !ok || len(digest) != 64 {
		return "", fmt.Errorf("invalid sha256 digest ref %q", hash)
	}
	return digest, nil
}

func verifyDigest(expected string, body []byte) error {
	actual := canonical.DigestBytes(body)
	if actual != expected {
		return fmt.Errorf("hash mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

func marshalCanonicalJSON(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal JSON: %w", err)
	}
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil {
		return nil, err
	}
	return canonicalBody, nil
}

func repoRootFrom(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	for {
		if fileExists(filepath.Join(current, "go.mod")) && fileExists(filepath.Join(current, "deno.json")) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("could not find repo root from %q", start)
		}
		current = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
