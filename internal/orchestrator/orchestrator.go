package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const jsonContentType = "application/json"

type executionIndex struct {
	nodesByID       map[string]*planpb.GraphNode
	symbolsByRef    map[string]*planpb.SymbolEntry
	contractsByRef  map[string]*planpb.ExecutionContract
	packagesByID    map[string]sourcePackageArtifact
	inboundByTarget map[string][]*planpb.GraphEdge
	stepOrder       []string
	schemaRefs      map[string]bool
}

type sourcePackageArtifact struct {
	PackageID   string
	Language    string
	PackageHash string
	Key         string
	ContentType string
}

type nodeOutput struct {
	Artifact manifestDataArtifact
	Body     []byte
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

	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: config.DatastoreRoot})
	if err != nil {
		return nil, fmt.Errorf("open local datastore: %w", err)
	}
	projectKey := NormalizeProjectKey(config.ProjectID)
	runID := config.RunID
	if runID == "" {
		runID = uuid.NewString()
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

	manifest := newRunManifest(config.Plan.GetPlanHash(), projectKey, runID, index.stepOrder)
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

	for _, nodeID := range index.stepOrder {
		node := index.nodesByID[nodeID]
		inputBytes, err := inputForNode(node, index.inboundByTarget[nodeID], outputs)
		if err != nil {
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
		}

		inputArtifact := manifestDataArtifact{
			Key:         runInputKey(projectKey, runID, nodeID).String(),
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
			markAttemptFailed(&manifest, nodeID, diagnostic)
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, diagnostic)
		}

		output, err := validateOutputArtifact(ctx, store, descriptor, outcome.ExpectedOutputHash, index)
		if err != nil {
			markAttemptFailed(&manifest, nodeID, err.Error())
			return failRun(ctx, store, manifestKey, &manifest, result, nodeID, err.Error())
		}
		outputs[nodeID] = output
		markAttemptSucceeded(&manifest, nodeID, output.Artifact)
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

	sourceRoot, err := filepath.Abs(config.SourcePackageRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve source package root: %w", err)
	}
	sourcePointer, err := marshalCanonicalJSON(sourceFetchPointer{SourceFetch: sourceRoot})
	if err != nil {
		return nil, err
	}
	sourceHash := canonical.DigestBytes(sourcePointer)
	sourceKey := sourcePackageKey(sourceHash)

	if _, err := store.Put(ctx, datastore.MustKey(sourceKey), sourcePointer, datastore.PutOptions{ContentType: SourceDirectoryContentType}); err != nil {
		return nil, fmt.Errorf("write source package artifact: %w", err)
	}

	packages := make(map[string]sourcePackageArtifact, len(config.Plan.GetSourcePackages()))
	for _, sourcePackage := range config.Plan.GetSourcePackages() {
		packages[sourcePackage.GetPackageId()] = sourcePackageArtifact{
			PackageID:   sourcePackage.GetPackageId(),
			Language:    sourcePackage.GetLanguage(),
			PackageHash: sourceHash,
			Key:         sourceKey,
			ContentType: SourceDirectoryContentType,
		}
	}
	return packages, nil
}

type sourceFetchPointer struct {
	SourceFetch string `json:"sourceFetch"`
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
		SchemaVersion: 0,
		Encoding:      "json-v0",
		PlanHash:      config.Plan.GetPlanHash(),
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
				Hash:        sourcePackage.PackageHash,
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
		Output: DataArtifactDestination{
			Artifact: ArtifactDestination{
				Key:         runOutputKey(projectKey, runID, node.GetId(), 1).String(),
				ContentType: jsonContentType,
			},
			Schema: node.GetOutputSchema(),
		},
		ChannelReads:  []ChannelArtifactRef{},
		ChannelWrites: []ChannelArtifactDestination{},
		Datastore: DatastoreDescriptor{
			Kind: "local",
			Path: config.DatastoreRoot,
		},
	}, nil
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

func validateOutputArtifact(ctx context.Context, store datastore.Datastore, descriptor StepInvocationDescriptor, expectedHash string, index executionIndex) (nodeOutput, error) {
	if !index.schemaRefs[descriptor.Output.Schema] {
		return nodeOutput{}, fmt.Errorf("output schema ref %s is not present in the plan", descriptor.Output.Schema)
	}

	outputKey, err := datastore.ParseKey(descriptor.Output.Artifact.Key)
	if err != nil {
		return nodeOutput{}, err
	}
	object, err := store.Get(ctx, outputKey)
	if err != nil {
		return nodeOutput{}, fmt.Errorf("output artifact %s is missing: %w", outputKey, err)
	}

	actualHash := canonical.DigestBytes(object.Body)
	if expectedHash != "" && actualHash != expectedHash {
		return nodeOutput{}, fmt.Errorf("output artifact %s hash mismatch: expected %s, got %s", outputKey, expectedHash, actualHash)
	}
	canonicalBody, err := canonical.CanonicalizeJSON(object.Body)
	if err != nil {
		return nodeOutput{}, fmt.Errorf("output artifact %s is not canonical JSON: %w", outputKey, err)
	}
	if !bytes.Equal(canonicalBody, object.Body) {
		return nodeOutput{}, fmt.Errorf("output artifact %s is not canonical JSON", outputKey)
	}

	schemaBytes, err := validateSchemaBlob(ctx, store, descriptor.Output.Schema)
	if err != nil {
		return nodeOutput{}, err
	}
	if err := validateJSON(schemaBytes, object.Body); err != nil {
		return nodeOutput{}, fmt.Errorf("output artifact %s violates schema %s: %w", outputKey, descriptor.Output.Schema, err)
	}

	return nodeOutput{
		Artifact: manifestDataArtifact{
			Key:         outputKey.String(),
			Hash:        actualHash,
			ContentType: descriptor.Output.Artifact.ContentType,
			Schema:      descriptor.Output.Schema,
		},
		Body: object.Body,
	}, nil
}

func validateSchemaBlob(ctx context.Context, store datastore.Datastore, schemaRef string) ([]byte, error) {
	key, err := blobKeyForHash(schemaRef)
	if err != nil {
		return nil, err
	}
	object, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("schema blob %s is missing: %w", key, err)
	}
	if err := verifyDigest(schemaRef, object.Body); err != nil {
		return nil, fmt.Errorf("schema blob %s: %w", key, err)
	}
	canonicalBody, err := canonical.CanonicalizeJSON(object.Body)
	if err != nil {
		return nil, fmt.Errorf("schema blob %s is not canonical JSON: %w", key, err)
	}
	if !bytes.Equal(canonicalBody, object.Body) {
		return nil, fmt.Errorf("schema blob %s is not canonical JSON", key)
	}
	return object.Body, nil
}

func validateJSON(schemaBytes []byte, documentBytes []byte) error {
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(documentBytes))
	if err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", schemaDocument); err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	if err := compiled.Validate(instance); err != nil {
		return err
	}
	return nil
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

	index := executionIndex{
		nodesByID:       make(map[string]*planpb.GraphNode, len(graph.GetNodes())),
		symbolsByRef:    make(map[string]*planpb.SymbolEntry, len(workflowPlan.GetSymbols())),
		contractsByRef:  make(map[string]*planpb.ExecutionContract, len(workflowPlan.GetContracts())),
		inboundByTarget: make(map[string][]*planpb.GraphEdge, len(graph.GetNodes())),
		stepOrder:       make([]string, 0, len(graph.GetNodes())),
		schemaRefs:      make(map[string]bool, len(workflowPlan.GetSchemas())),
	}
	for _, schemaEntry := range workflowPlan.GetSchemas() {
		index.schemaRefs[schemaEntry.GetHash()] = true
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
		if node.GetKind() == "step" {
			index.stepOrder = append(index.stepOrder, node.GetId())
		}
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
	return index, nil
}

func newRunManifest(planHash string, projectKey string, runID string, stepOrder []string) runManifest {
	steps := make([]manifestStep, 0, len(stepOrder))
	for _, stepID := range stepOrder {
		steps = append(steps, manifestStep{NodeID: stepID, Status: StatusPending, Attempts: []manifestAttempt{}})
	}
	return runManifest{
		Kind:          "RunManifest",
		SchemaVersion: 0,
		PlanHash:      planHash,
		ProjectKey:    projectKey,
		RunID:         runID,
		Status:        StatusRunning,
		Steps:         steps,
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

func markAttemptSucceeded(manifest *runManifest, nodeID string, output manifestDataArtifact) {
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

func runInputKey(projectKey string, runID string, stepID string) datastore.Key {
	return datastore.MustKey("projects/" + projectKey + "/runs/" + runID + "/inputs/" + stepID + ".json")
}

func runOutputKey(projectKey string, runID string, stepID string, attempt int) datastore.Key {
	return datastore.MustKey("projects/" + projectKey + "/runs/" + runID + "/steps/" + stepID + "/" + fmt.Sprint(attempt) + "/output.json")
}

func runResultKey(projectKey string, runID string) datastore.Key {
	return datastore.MustKey("projects/" + projectKey + "/runs/" + runID + "/result.json")
}

func sourcePackageKey(hash string) string {
	return "packages/" + strings.Replace(hash, "sha256:", "sha256-", 1) + "/source.tar.zst"
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
