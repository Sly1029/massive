package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"
	"unicode/utf8"

	schema "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/conformance/schema/coordinatorpb"
	"github.com/Sly1029/massive/conformance/schema/failurepb"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/runevent"
	"github.com/Sly1029/massive/internal/runjournal"
	"github.com/Sly1029/massive/internal/taskprocess"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
)

type coordinatorDivergenceError struct{ Reason string }

func (failure *coordinatorDivergenceError) Error() string {
	return "coordinator divergence: " + failure.Reason
}

// This recipe is independently versioned; it never asserts GraphIR compilation.
func coordinatorFingerprint(definition, input []byte, env *runjournal.Environment) (string, error) {
	body, err := canonical.Marshal(map[string]any{"kind": "CoordinatorExecution", "recipeVersion": 1, "definition": json.RawMessage(definition), "input": json.RawMessage(input), "environment": env})
	if err != nil {
		return "", err
	}
	return canonical.DigestBytes(body), nil
}

type CoordinatorRunConfig struct {
	Definition                                           *CoordinatorDefinition
	StoreRoot, ProjectID, RunID, SourceRoot, Interpreter string
	Environment                                          *runjournal.Environment
	EnvironmentRequest                                   environment.Request
	Resume, WorkersQuiescent                             bool
	Input                                                []byte
	Hooks                                                RunHooks
	ProcessLimit                                         int
}

type coordinatorTurn struct {
	Commands []json.RawMessage `json:"commands"`
	Closed   bool              `json:"closed"`
}

type coordinatorRecord struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Definition    json.RawMessage            `json:"definition"`
	Start         json.RawMessage            `json:"start"`
	Interpreter   string                     `json:"interpreter"`
	Deadline      string                     `json:"deadline,omitempty"`
	Turns         []coordinatorTurn          `json:"turns"`
	Results       map[string]json.RawMessage `json:"results"`
	Settlements   []string                   `json:"settlements"`
	Deliveries    []string                   `json:"deliveries"`
	Steps         []runjournal.Step          `json:"steps"`
	RetryAt       map[string]string          `json:"retryAt"`
	// Sequence reserves a high watermark before observations are emitted; crash recovery may leave gaps.
	Sequence int             `json:"sequence"`
	Final    json.RawMessage `json:"final,omitempty"`
}

type coordinatorOutcome struct {
	Key        string
	Descriptor StepInvocationDescriptor
	Outcome    StepInvocationOutcome
	Err        error
}

type localCoordinator struct {
	config                  CoordinatorRunConfig
	ctx                     context.Context
	store                   datastore.Datastore
	manifest                *runjournal.Manifest
	record                  *coordinatorRecord
	index                   executionIndex
	commands                map[string]*coordinatorpb.InvokeCommand
	descriptors             map[string]StepInvocationDescriptor
	active                  map[string]bool
	outcomes                chan coordinatorOutcome
	events                  *runEvents
	replayTurns             []coordinatorTurn
	replayDeliveries        []string
	turn, issued, delivered int
	replaying, quiescent    bool
	cancel                  context.CancelFunc
}

// RunCoordinator uses the existing invoker and artifact runtime, without compiling
// a graph. The immutable snapshot reference in the journal is the commit point.
func RunCoordinator(ctx context.Context, config CoordinatorRunConfig) (result *RunResult, returnedErr error) {
	startedAt := time.Now()
	if !config.Resume && config.Definition != nil && config.Definition.DeadlineSeconds > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadlineCause(ctx, startedAt.Add(time.Duration(config.Definition.DeadlineSeconds)*time.Second), &RunDeadlineError{Seconds: config.Definition.DeadlineSeconds})
		defer stop()
	}
	if config.ProjectID == "" {
		return nil, fmt.Errorf("coordinator requires --project")
	}
	if config.RunID == "" {
		config.RunID = uuid.NewString()
	}
	if !ValidSafePathSegment(config.RunID) {
		return nil, fmt.Errorf("invalid coordinator run id")
	}
	var err error
	config.StoreRoot, err = filepath.Abs(config.StoreRoot)
	if err != nil {
		return nil, err
	}
	project := NormalizeProjectKey(config.ProjectID)
	release, err := claimCoordinator(config.StoreRoot, project, config.RunID)
	if err != nil {
		return nil, err
	}
	defer release()
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: config.StoreRoot})
	if err != nil {
		return nil, err
	}
	engine := &localCoordinator{config: config, store: store, commands: map[string]*coordinatorpb.InvokeCommand{}, descriptors: map[string]StepInvocationDescriptor{}, active: map[string]bool{}, outcomes: make(chan coordinatorOutcome, 32)}
	var startDescriptor StepInvocationDescriptor
	if config.Resume {
		object, err := store.Get(ctx, runManifestKey(project, config.RunID))
		if err != nil {
			return nil, err
		}
		engine.manifest, err = runjournal.Parse(object.Body)
		if err != nil {
			return nil, err
		}
		if engine.manifest.RunKind != "coordinator" {
			return nil, fmt.Errorf("resume capability: only coordinator runs are supported")
		}
		if engine.manifest.Status != StatusRunning && engine.manifest.Status != StatusSucceeded {
			return nil, fmt.Errorf("resume requires an interrupted or succeeded coordinator run")
		}
		if engine.manifest.Status == StatusRunning && !config.WorkersQuiescent {
			return nil, fmt.Errorf("resume precondition: stop all orphan workers first, then supply --workers-quiescent; a file lock does not fence effects")
		}
		ref := engine.manifest.CoordinatorRecord
		record, err := store.Get(ctx, datastore.MustKey(ref.Key))
		if err != nil {
			return nil, fmt.Errorf("coordinator replay record unavailable")
		}
		if canonical.DigestBytes(record.Body) != ref.Hash || len(record.Body) != ref.Size {
			return nil, fmt.Errorf("coordinator replay record identity mismatch")
		}
		if err := validateJSONAgainstSchema(string(schema.CoordinatorRecordSchemaJSON), record.Body); err != nil {
			return nil, fmt.Errorf("invalid coordinator replay record: %w", err)
		}
		if err := json.Unmarshal(record.Body, &engine.record); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(engine.record.Steps, engine.manifest.Steps) {
			return nil, fmt.Errorf("coordinator journal/record disagreement")
		}
		definition, err := ParseCoordinatorDefinition(engine.record.Definition)
		if err != nil {
			return nil, err
		}
		if config.Definition != nil && !bytes.Equal(config.Definition.Canonical, definition.Canonical) {
			return nil, fmt.Errorf("coordinator identity changed; rebuild and start a new run")
		}
		engine.config.Definition = definition
		engine.config.Interpreter = engine.record.Interpreter
		var wire struct {
			Descriptor json.RawMessage `json:"descriptor"`
		}
		if err := json.Unmarshal(engine.record.Start, &wire); err != nil {
			return nil, err
		}
		if err := decodeCoordinatorDescriptor(wire.Descriptor, &startDescriptor); err != nil {
			return nil, err
		}
		if startDescriptor.RunID != config.RunID || startDescriptor.ProjectKey != project || startDescriptor.PlanHash != engine.manifest.PlanHash {
			return nil, fmt.Errorf("coordinator start identity mismatch")
		}
		binding, ok := startDescriptor.Datastore.(LocalDatastoreDescriptor)
		if !ok || binding.Path != config.StoreRoot {
			return nil, fmt.Errorf("coordinator store location changed; restore the original store location")
		}
		if startDescriptor.SourcePackage.PackageHash != definition.Source.GetPackageHash() || startDescriptor.NodeID != "coordinator" || startDescriptor.Input.Schema != definition.InputSchema || startDescriptor.Output.Schema != definition.OutputSchema || startDescriptor.Symbol.Module != definition.Symbol.GetModule() || startDescriptor.Symbol.Export != definition.Symbol.GetExport() {
			return nil, fmt.Errorf("coordinator start/definition disagreement")
		}
		originalInput, err := store.Get(ctx, datastore.MustKey(startDescriptor.Input.Artifact.Key))
		if err != nil {
			return nil, fmt.Errorf("coordinator input artifact unavailable")
		}
		if canonical.DigestBytes(originalInput.Body) != startDescriptor.Input.Artifact.Hash {
			return nil, fmt.Errorf("coordinator input artifact identity mismatch")
		}
		hash, err := coordinatorFingerprint(definition.Canonical, originalInput.Body, engine.manifest.Environment)
		if err != nil || hash != engine.manifest.PlanHash {
			return nil, fmt.Errorf("coordinator execution fingerprint mismatch")
		}
		if config.Input != nil {
			input, err := canonical.CanonicalizeJSON(config.Input)
			if err != nil {
				return nil, err
			}
			if canonical.DigestBytes(input) != startDescriptor.Input.Artifact.Hash {
				return nil, fmt.Errorf("coordinator input identity changed")
			}
		}
		engine.replayTurns = append([]coordinatorTurn(nil), engine.record.Turns...)
		engine.replayDeliveries = append([]string(nil), engine.record.Deliveries...)
		engine.replaying = true
	} else {
		if config.Definition == nil || config.Interpreter == "" {
			return nil, fmt.Errorf("coordinator requires typed entry metadata and a checked interpreter")
		}
		definition := config.Definition
		input, err := canonical.CanonicalizeJSON(config.Input)
		if err != nil {
			return nil, err
		}
		// Versioned execution fingerprint has an explicit coordinator discriminator.
		hash, err := coordinatorFingerprint(definition.Canonical, input, config.Environment)
		if err != nil {
			return nil, err
		}
		engine.manifest = &runjournal.Manifest{Kind: "RunManifest", SchemaVersion: 7, Encoding: "json-v7", RunKind: "coordinator", PlanHash: hash, ProjectKey: project, RunID: config.RunID, Status: StatusRunning, Environment: config.Environment, Steps: []runjournal.Step{}, Decisions: []runjournal.Decision{}}
		// Reuse only prerequisite materialization, not graph validation or scheduling.
		packages, err := materializePrerequisites(ctx, store, RunConfig{Plan: &planpb.WorkflowPlan{SourcePackages: []*planpb.SourcePackageRef{definition.Source}, Schemas: definition.Schemas}, DatastoreRoot: config.StoreRoot, SourcePackageRoot: config.SourceRoot, SourceManifests: map[string]SourcePackageManifest{definition.Source.GetPackageId(): {Root: config.SourceRoot, Files: definition.Files}}})
		if err != nil {
			return nil, err
		}
		engine.index = coordinatorIndex(packages)
		for _, entry := range definition.Schemas {
			engine.index.schemaRefs[entry.GetHash()] = true
			engine.index.schemaJSON[entry.GetHash()] = entry.GetCanonicalJson()
		}
		engine.index.symbolsByRef[definition.Symbol.GetSymbolRef()] = definition.Symbol
		engine.index.contractsByRef[definition.Defaults.GetContractRef()] = definition.Defaults
		node := &planpb.GraphNode{Id: pointer("coordinator"), SymbolRef: definition.Symbol.SymbolRef, ContractRef: definition.Defaults.ContractRef, InputSchema: pointer(definition.InputSchema), OutputSchema: pointer(definition.OutputSchema)}
		inputRef := runjournal.DataArtifact{Key: runInputKey(project, config.RunID, "coordinator", nil).String(), Hash: canonical.DigestBytes(input), ContentType: jsonContentType, Schema: definition.InputSchema}
		if _, err := store.Put(ctx, datastore.MustKey(inputRef.Key), input, datastore.PutOptions{ContentType: jsonContentType, IfAbsent: true}); err != nil {
			return nil, err
		}
		startDescriptor, err = descriptorForStep(hash, LocalDatastoreDescriptor{Kind: "local", Path: config.StoreRoot}, project, config.RunID, node, inputRef, engine.index, 1)
		if err != nil {
			return nil, err
		}
		startProto, err := descriptorToProto(startDescriptor)
		if err != nil {
			return nil, err
		}
		start := &coordinatorpb.CoordinatorStart{Kind: pointer("CoordinatorStart"), ProtocolVersion: pointer(uint32(1)), Descriptor_: startProto, Defaults: definition.Defaults, Environment: definition.Environment}
		deadline := ""
		if definition.DeadlineSeconds > 0 {
			deadline = startedAt.Add(time.Duration(definition.DeadlineSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
			start.Deadline = &deadline
		}
		startJSON, err := coordinatorProtoJSON(start)
		if err != nil {
			return nil, err
		}
		engine.record = &coordinatorRecord{SchemaVersion: 1, Definition: definition.Canonical, Start: startJSON, Interpreter: config.Interpreter, Deadline: deadline, Turns: []coordinatorTurn{{Commands: []json.RawMessage{}}}, Results: map[string]json.RawMessage{}, Settlements: []string{}, Deliveries: []string{}, Steps: []runjournal.Step{}, RetryAt: map[string]string{}}
	}
	if config.Resume && engine.record.Deadline != "" {
		deadline, err := time.Parse(time.RFC3339Nano, engine.record.Deadline)
		if err != nil {
			return nil, err
		}
		var stop context.CancelFunc
		ctx, stop = context.WithDeadlineCause(ctx, deadline, &RunDeadlineError{Seconds: engine.config.Definition.DeadlineSeconds})
		defer stop()
	}
	engine.ctx, engine.cancel = context.WithCancel(ctx)
	defer engine.cancel()
	if config.Hooks.OnRunEvent != nil {
		engine.events = &runEvents{hook: config.Hooks.OnRunEvent, projectKey: project, runID: config.RunID, started: time.Now(), sequence: engine.record.Sequence, attemptStarts: map[eventIdentity]time.Time{}, stepStarts: map[string]time.Time{}}
	}
	result = &RunResult{PlanHash: engine.manifest.PlanHash, RunID: config.RunID, ProjectKey: project, ManifestKey: runManifestKey(project, config.RunID).String(), Status: StatusRunning}
	// All outcome reconciliation precedes the terminal snapshot and journal.
	defer func() {
		engine.cancel()
		for len(engine.active) > 0 {
			outcome := <-engine.outcomes
			if err := engine.settle(outcome); err != nil {
				returnedErr = errors.Join(returnedErr, err)
			}
		}
		if returnedErr != nil {
			status, diagnostic := StatusFailed, "coordinator run failed"
			if errors.Is(returnedErr, ctx.Err()) && ctx.Err() != nil {
				status, diagnostic = StatusCancelled, "coordinator run cancelled"
				var deadline *RunDeadlineError
				if errors.As(context.Cause(ctx), &deadline) {
					status, diagnostic = StatusFailed, deadline.Error()
					returnedErr = errors.Join(returnedErr, deadline)
				}
			}
			var fatal *FatalRunError
			if errors.As(returnedErr, &fatal) {
				engine.manifest.Failure = &fatal.Failure
				result.Failure = &fatal.Failure
				diagnostic = fatal.Error()
				status = StatusFailed
			}
			var divergence *coordinatorDivergenceError
			if errors.As(returnedErr, &divergence) {
				diagnostic = divergence.Error()
			}
			engine.manifest.Terminate(status, diagnostic)
		}
		if err := engine.save(context.WithoutCancel(ctx), false); err != nil {
			returnedErr = errors.Join(returnedErr, err)
		}
		engine.events.runFinished(engine.manifest.Status)
		result.Status = engine.manifest.Status
		result.Steps = summariesFromManifest(*engine.manifest)
		if returnedErr != nil {
			returnedErr = &RunError{Cause: returnedErr, Diagnostic: returnedErr.Error(), Result: result}
		}
	}()
	if config.Resume {
		engine.index = coordinatorIndex(map[string]sourcePackageArtifact{startDescriptor.SourcePackage.PackageID: {PackageID: startDescriptor.SourcePackage.PackageID, Language: "python", PackageHash: startDescriptor.SourcePackage.PackageHash, Key: startDescriptor.SourcePackage.SourceArchive.Key, ArchiveHash: startDescriptor.SourcePackage.SourceArchive.Hash, ContentType: SourceArchiveContentType}})
		for _, entry := range engine.config.Definition.Schemas {
			engine.index.schemaRefs[entry.GetHash()] = true
			engine.index.schemaJSON[entry.GetHash()] = entry.GetCanonicalJson()
		}
		for turn, history := range engine.record.Turns {
			for _, body := range history.Commands {
				command, err := parseCoordinatorCommand(body)
				if err != nil {
					return result, err
				}
				if int(command.GetTurn()) != turn || engine.commands[command.GetInvocationKey()] != nil {
					return result, fmt.Errorf("invalid replay command sequence")
				}
				if err := engine.register(command); err != nil {
					return result, err
				}
				engine.commands[command.GetInvocationKey()] = command
			}
		}
		if len(engine.commands) != len(engine.manifest.Steps) {
			return result, fmt.Errorf("replay steps differ from commands")
		}
		for _, step := range engine.manifest.Steps {
			command := engine.commands[step.NodeID]
			if command == nil {
				return result, fmt.Errorf("replay step has no command")
			}
			descriptor, err := engine.descriptor(command, max(len(step.Attempts), 1))
			if err != nil {
				return result, err
			}
			engine.descriptors[step.NodeID] = descriptor
			if step.Status == StatusSucceeded {
				verification, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				output, err := resolveOutputArtifact(verification, store, descriptor, engine.index)
				stop()
				if err != nil || step.Attempts[len(step.Attempts)-1].Output == nil || !reflect.DeepEqual(output.Published, *step.Attempts[len(step.Attempts)-1].Output) {
					return result, fmt.Errorf("completed coordinator artifact verification failed")
				}
			}
		}
		if len(engine.record.Final) > 0 {
			verification, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			final, err := resolveOutputArtifact(verification, store, startDescriptor, engine.index)
			stop()
			if err != nil || engine.manifest.Result == nil || final.Published.Body.Hash != engine.manifest.Result.Hash {
				return result, fmt.Errorf("completed coordinator final artifact verification failed")
			}
			body, err := store.Get(ctx, datastore.MustKey(engine.manifest.Result.Key))
			if err != nil || canonical.DigestBytes(body.Body) != engine.manifest.Result.Hash {
				return result, fmt.Errorf("completed coordinator result artifact verification failed")
			}
		}
		if err := engine.validateHistory(); err != nil {
			return result, err
		}
		// Check the pinned interpreter against verified archived project bytes.
		req := config.EnvironmentRequest
		req.Python = engine.record.Interpreter
		if _, err := isolatedPreflight(ctx, store, req, startDescriptor, SourceArchive{Digest: startDescriptor.SourcePackage.SourceArchive.Hash}); err != nil {
			return result, err
		}
		env, err := store.Get(ctx, attemptEnvironmentKey(startDescriptor.Output.ManifestKey))
		if err != nil {
			return result, err
		}
		var realized runjournal.Environment
		if err := json.Unmarshal(env.Body, &realized); err != nil {
			return result, err
		}
		if engine.manifest.Environment == nil || realized.RealizationHash != engine.manifest.Environment.RealizationHash || realized.RequirementHash != engine.manifest.Environment.RequirementHash {
			return result, fmt.Errorf("coordinator environment identity changed")
		}
		for i := range engine.manifest.Steps {
			step := &engine.manifest.Steps[i]
			if len(step.Attempts) > 0 && step.Attempts[len(step.Attempts)-1].Status == StatusRunning {
				attempt := &step.Attempts[len(step.Attempts)-1]
				attempt.Status = StatusFailed
				attempt.Diagnostic = "invocation interrupted before durable result"
				policy := policyForContract(engine.commands[step.NodeID].Contract)
				if len(step.Attempts) >= policy.maxAttempts {
					failure := &failurepb.ItemFailure{Attempts: pointer(uint32(len(step.Attempts))), Kind: pointer("killed"), Diagnostic: pointer(attempt.Diagnostic)}
					if err := engine.recordFailure(step.NodeID, failure); err != nil {
						return result, err
					}
				} else {
					engine.record.RetryAt[step.NodeID] = time.Now().Add(policy.delayBefore(len(step.Attempts) + 1)).UTC().Format(time.RFC3339Nano)
				}
			}
		}
	}
	if err := engine.save(ctx, !config.Resume); err != nil {
		return result, err
	}
	if engine.events != nil {
		engine.events.emit(runevent.Event{Type: "run.started", PlanHash: engine.manifest.PlanHash})
	}
	if err := engine.loop(startDescriptor); err != nil {
		return result, err
	}
	result.ResultKey = engine.manifest.Result.Key
	return result, nil
}

func coordinatorIndex(packages map[string]sourcePackageArtifact) executionIndex {
	return executionIndex{packagesByID: packages, symbolsByRef: map[string]*planpb.SymbolEntry{}, contractsByRef: map[string]*planpb.ExecutionContract{}, schemaRefs: map[string]bool{}, schemaJSON: map[string]string{}}
}

// Datastore is an interface union on descriptors; decode its schema-owned local
// variant explicitly, rather than asking encoding/json to instantiate it.
func decodeCoordinatorDescriptor(body []byte, descriptor *StepInvocationDescriptor) error {
	var wire struct {
		Datastore json.RawMessage `json:"datastore"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return err
	}
	binding, err := ParseDatastoreDescriptor(wire.Datastore)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	delete(fields, "datastore")
	remaining, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(remaining, descriptor); err != nil {
		return err
	}
	descriptor.Datastore = binding
	return nil
}

func (engine *localCoordinator) save(ctx context.Context, create bool) error {
	ctx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	engine.record.Steps = engine.manifest.Steps
	if engine.events != nil {
		engine.record.Sequence = max(engine.record.Sequence, engine.events.sequence+64)
	}
	body, err := canonical.Marshal(engine.record)
	if err != nil {
		return err
	}
	hash := canonical.DigestBytes(body)
	key, err := blobKeyForHash(hash)
	if err != nil {
		return err
	}
	if _, err := engine.store.Put(ctx, key, body, datastore.PutOptions{ContentType: jsonContentType, IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
		return err
	}
	engine.manifest.CoordinatorRecord = &runjournal.ArtifactRef{Key: key.String(), Hash: hash, Size: len(body), ContentType: jsonContentType}
	journal, err := canonical.Marshal(engine.manifest)
	if err != nil {
		return err
	}
	if _, err := engine.store.Put(ctx, runManifestKey(engine.manifest.ProjectKey, engine.manifest.RunID), journal, datastore.PutOptions{ContentType: jsonContentType, IfAbsent: create}); err != nil {
		return err
	}
	return nil
}

func (engine *localCoordinator) register(command *coordinatorpb.InvokeCommand) error {
	symbol := command.Symbol
	if symbol.GetPackageId() != engine.config.Definition.Source.GetPackageId() || symbol.GetLanguage() != "python" {
		return fmt.Errorf("coordinator activity requires the pinned Python source package")
	}
	contract, err := coordinatorProtoJSON(command.Contract)
	if err != nil {
		return err
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(contract, &value); err != nil {
		return err
	}
	delete(value, "contractRef")
	recipe, err := canonical.Marshal(value)
	if err != nil {
		return err
	}
	if canonical.DigestBytes(recipe) != command.Contract.GetContractRef() {
		return fmt.Errorf("coordinator contract identity mismatch")
	}
	environment, err := coordinatorProtoJSON(command.Environment.GetContainer())
	if err != nil {
		return err
	}
	var container map[string]json.RawMessage
	if err := json.Unmarshal(environment, &container); err != nil {
		return err
	}
	container["kind"] = json.RawMessage(`"container"`)
	environment, err = canonical.Marshal(container)
	if err != nil {
		return err
	}
	if canonical.DigestBytes(environment) != command.Environment.GetEnvRef() || command.Environment.GetEnvRef() != command.Contract.GetEnvironmentRef() {
		return fmt.Errorf("coordinator environment requirement identity mismatch")
	}
	engine.index.symbolsByRef[command.GetSymbolRef()] = symbol
	engine.index.contractsByRef[command.Contract.GetContractRef()] = command.Contract
	for _, entry := range command.Schemas {
		if err := verifyDigest(entry.GetHash(), []byte(entry.GetCanonicalJson())); err != nil {
			return err
		}
		engine.index.schemaRefs[entry.GetHash()] = true
		engine.index.schemaJSON[entry.GetHash()] = entry.GetCanonicalJson()
		key, err := blobKeyForHash(entry.GetHash())
		if err != nil {
			return err
		}
		if _, err := engine.store.Put(engine.ctx, key, []byte(entry.GetCanonicalJson()), datastore.PutOptions{ContentType: jsonContentType, IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
			return err
		}
	}
	if !engine.index.schemaRefs[command.GetInputSchema()] || !engine.index.schemaRefs[command.GetOutputSchema()] {
		return fmt.Errorf("coordinator activity schemas unavailable")
	}
	input, err := engine.store.Get(engine.ctx, datastore.MustKey(command.Input.GetKey()))
	if err != nil {
		return err
	}
	if canonical.DigestBytes(input.Body) != command.Input.GetHash() || input.Info.ContentType != command.Input.GetContentType() {
		return fmt.Errorf("coordinator input identity mismatch")
	}
	if err := validateJSONAgainstSchema(engine.index.schemaJSON[command.GetInputSchema()], input.Body); err != nil {
		return fmt.Errorf("coordinator input schema mismatch")
	}
	return nil
}

func (engine *localCoordinator) descriptor(command *coordinatorpb.InvokeCommand, attempt int) (StepInvocationDescriptor, error) {
	node := &planpb.GraphNode{Id: command.InvocationKey, SymbolRef: command.SymbolRef, ContractRef: command.Contract.ContractRef, InputSchema: command.InputSchema, OutputSchema: command.OutputSchema}
	return descriptorForStep(engine.manifest.PlanHash, LocalDatastoreDescriptor{Kind: "local", Path: engine.config.StoreRoot}, engine.manifest.ProjectKey, engine.manifest.RunID, node, runjournal.DataArtifact{Key: command.Input.GetKey(), Hash: command.Input.GetHash(), ContentType: command.Input.GetContentType(), Schema: command.GetInputSchema()}, engine.index, attempt)
}

func (engine *localCoordinator) step(key string) *runjournal.Step {
	for i := range engine.manifest.Steps {
		if engine.manifest.Steps[i].NodeID == key {
			return &engine.manifest.Steps[i]
		}
	}
	return nil
}

func (engine *localCoordinator) recordFailure(key string, failure *failurepb.ItemFailure) error {
	result := &coordinatorpb.InvokeResult{Kind: pointer("InvokeResult"), ProtocolVersion: pointer(uint32(1)), Turn: pointer(uint32(0)), InvocationKey: &key, Result: &coordinatorpb.InvokeResult_Failure{Failure: failure}}
	body, err := coordinatorProtoJSON(result)
	if err != nil {
		return err
	}
	engine.step(key).Status = StatusFailed
	engine.record.Results[key] = body
	engine.record.Settlements = append(engine.record.Settlements, key)
	delete(engine.record.RetryAt, key)
	return nil
}

func (engine *localCoordinator) settle(report coordinatorOutcome) error {
	delete(engine.active, report.Key)
	step := engine.step(report.Key)
	attempt := &step.Attempts[len(step.Attempts)-1]
	outcome := report.Outcome
	engine.events.attemptObserved(report.Descriptor, outcome)
	if outcome.Status == StatusSucceeded {
		verification, stop := context.WithTimeout(context.WithoutCancel(engine.ctx), 5*time.Second)
		output, err := resolveOutputArtifact(verification, engine.store, report.Descriptor, engine.index)
		stop()
		if err != nil {
			attempt.Status = StatusFailed
			attempt.Diagnostic = "activity output verification failed"
			step.Status = StatusFailed
			return err
		}
		attempt.Status = StatusSucceeded
		attempt.Output = &output.Published
		step.Status = StatusSucceeded
		result := &coordinatorpb.InvokeResult{Kind: pointer("InvokeResult"), ProtocolVersion: pointer(uint32(1)), Turn: pointer(uint32(0)), InvocationKey: &report.Key, Result: &coordinatorpb.InvokeResult_Output{Output: &planpb.ArtifactRef{Key: &output.Published.Manifest.Key, Hash: &output.Published.Manifest.Hash, ContentType: &output.Published.Manifest.ContentType}}}
		body, err := coordinatorProtoJSON(result)
		if err != nil {
			return err
		}
		engine.record.Results[report.Key] = body
		engine.record.Settlements = append(engine.record.Settlements, report.Key)
	} else {
		attempt.Status = StatusFailed
		attempt.Diagnostic = durableRunnerDiagnostic(outcome)
		if outcome.Status == StatusCancelled {
			attempt.Status = StatusCancelled
		}
		if report.Err != nil {
			step.Status = attempt.Status
			return report.Err
		}
		if outcome.Fatal != nil {
			step.Status = StatusFailed
			return &FatalRunError{Failure: *outcome.Fatal}
		}
		policy := policyForContract(engine.commands[report.Key].Contract)
		if retryableOutcome(outcome) && len(step.Attempts) < policy.maxAttempts {
			engine.record.RetryAt[report.Key] = time.Now().Add(policy.delayBefore(len(step.Attempts) + 1)).UTC().Format(time.RFC3339Nano)
		} else {
			failure, ok := mapItemFailure(outcome, len(step.Attempts))
			if !ok {
				step.Status = StatusFailed
				return fmt.Errorf("coordinator activity contract failed")
			}
			body, err := canonical.Marshal(failure)
			if err != nil {
				return err
			}
			var shared failurepb.ItemFailure
			if err := protojson.Unmarshal(body, &shared); err != nil {
				return err
			}
			if err := engine.recordFailure(report.Key, &shared); err != nil {
				return err
			}
		}
	}
	if err := engine.save(context.WithoutCancel(engine.ctx), false); err != nil {
		return err
	}
	if step.Status == StatusSucceeded || step.Status == StatusFailed {
		engine.events.stepFinished(step)
	}
	return nil
}

func (engine *localCoordinator) dispatch() error {
	if engine.replaying || engine.ctx.Err() != nil {
		return engine.ctx.Err()
	}
	limit := engine.config.ProcessLimit
	if limit <= 0 || limit > defaultLocalProcessConcurrency {
		limit = defaultLocalProcessConcurrency
	}
	for i := range engine.manifest.Steps {
		step := &engine.manifest.Steps[i]
		key := step.NodeID
		if len(engine.active) >= limit {
			break
		}
		if engine.active[key] || engine.record.Results[key] != nil {
			continue
		}
		if when := engine.record.RetryAt[key]; when != "" {
			ready, err := time.Parse(time.RFC3339Nano, when)
			if err != nil {
				return err
			}
			if time.Now().Before(ready) {
				continue
			}
		}
		command := engine.commands[key]
		policy := policyForContract(command.Contract)
		if len(step.Attempts) >= policy.maxAttempts {
			return fmt.Errorf("coordinator interrupted invocation has no eligible attempts")
		}
		descriptor, err := engine.descriptor(command, len(step.Attempts)+1)
		if err != nil {
			return err
		}
		step.Status = StatusRunning
		step.Attempts = append(step.Attempts, runjournal.Attempt{Attempt: descriptor.Attempt, Status: StatusRunning, Input: runjournal.DataArtifact{Key: descriptor.Input.Artifact.Key, Hash: descriptor.Input.Artifact.Hash, ContentType: descriptor.Input.Artifact.ContentType, Schema: descriptor.Input.Schema}})
		engine.descriptors[key] = descriptor
		engine.events.attemptStarted(descriptor)
		delete(engine.record.RetryAt, key)
		if err := engine.save(engine.ctx, false); err != nil {
			return err
		}
		engine.active[key] = true
		go func() {
			invoker := ProcessStepInvoker{CommandTemplate: PythonRunnerCommand(engine.config.Interpreter), ProcessLimit: 1}
			outcomes, err := invoker.InvokeSteps(engine.ctx, StepInvocationBatch{Steps: []StepInvocation{{Descriptor: descriptor, Timeout: policy.timeout}}, MaxConcurrency: 1})
			outcome := StepInvocationOutcome{Status: stepInvocationStatusInfraFailed}
			if len(outcomes) == 1 {
				outcome = outcomes[0]
			}
			engine.outcomes <- coordinatorOutcome{Key: key, Descriptor: descriptor, Outcome: outcome, Err: err}
		}()
	}
	return nil
}

func (engine *localCoordinator) validateHistory() error {
	seen := map[string]bool{}
	for _, key := range engine.record.Settlements {
		if seen[key] || engine.commands[key] == nil || engine.record.Results[key] == nil {
			return fmt.Errorf("invalid replay settlement sequence")
		}
		seen[key] = true
		var result coordinatorpb.InvokeResult
		body := engine.record.Results[key]
		if err := validateJSONAgainstSchema(string(schema.CoordinatorSchemaJSON), body); err != nil {
			return fmt.Errorf("invalid recorded activity result")
		}
		if err := protojson.Unmarshal(body, &result); err != nil {
			return err
		}
		step := engine.step(key)
		if result.GetInvocationKey() != key || step == nil || len(step.Attempts) == 0 {
			return fmt.Errorf("invalid recorded result identity")
		}
		if output := result.GetOutput(); output != nil {
			publication := step.Attempts[len(step.Attempts)-1].Output
			if step.Status != StatusSucceeded || publication == nil || output.GetHash() != publication.Manifest.Hash || output.GetKey() != publication.Manifest.Key {
				return fmt.Errorf("recorded result/publication disagreement")
			}
		} else if result.GetFailure() == nil || step.Status != StatusFailed || int(result.GetFailure().GetAttempts()) != len(step.Attempts) {
			return fmt.Errorf("invalid recorded failure")
		}
	}
	if len(seen) != len(engine.record.Results) {
		return fmt.Errorf("replay has an unordered result")
	}
	seen = map[string]bool{}
	for turn, key := range engine.record.Deliveries {
		if seen[key] || engine.record.Results[key] == nil || turn >= len(engine.record.Turns) || !engine.record.Turns[turn].Closed {
			return fmt.Errorf("invalid replay delivery sequence")
		}
		seen[key] = true
		if int(engine.commands[key].GetTurn()) > turn {
			return fmt.Errorf("replay delivers an unissued activity")
		}
	}
	if len(engine.record.Turns) != len(engine.record.Deliveries)+1 {
		return fmt.Errorf("replay turn/delivery disagreement")
	}
	for i, turn := range engine.record.Turns {
		if i < len(engine.record.Turns)-1 && !turn.Closed {
			return fmt.Errorf("replay has an unclosed earlier turn")
		}
	}
	return nil
}

func (engine *localCoordinator) accept(body []byte) error {
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil {
		return err
	}
	command, err := parseCoordinatorCommand(canonicalBody)
	if err != nil {
		return err
	}
	if int(command.GetTurn()) != engine.turn || engine.quiescent {
		return &coordinatorDivergenceError{Reason: "command outside its delivery turn"}
	}
	if engine.replaying && engine.turn < len(engine.replayTurns) {
		turn := engine.replayTurns[engine.turn]
		if engine.issued < len(turn.Commands) {
			if !bytes.Equal(canonicalBody, turn.Commands[engine.issued]) {
				return &coordinatorDivergenceError{Reason: "canonical command differs from recorded prefix"}
			}
			engine.issued++
			return nil
		}
		if turn.Closed {
			return &coordinatorDivergenceError{Reason: "new command before recorded turn boundary"}
		}
	}
	// Extra commands are allowed only after the entire accepted history is reissued.
	if engine.replaying && (engine.turn < len(engine.replayTurns)-1 || engine.delivered < len(engine.replayDeliveries)) {
		return &coordinatorDivergenceError{Reason: "new command ahead of recorded suffix"}
	}
	key := command.GetInvocationKey()
	if engine.commands[key] != nil {
		return fmt.Errorf("duplicate coordinator invocation key")
	}
	for previous := range engine.commands {
		existing := engine.commands[previous]
		if existing.GetSymbolRef() == command.GetSymbolRef() && (previous == existing.GetSymbolRef() || key == command.GetSymbolRef()) {
			return fmt.Errorf("repeated symbols require explicit keys on all uses")
		}
	}
	if err := engine.register(command); err != nil {
		return err
	}
	engine.commands[key] = command
	engine.manifest.Steps = append(engine.manifest.Steps, runjournal.Step{NodeID: key, Status: StatusPending, Attempts: []runjournal.Attempt{}})
	engine.record.Turns[engine.turn].Commands = append(engine.record.Turns[engine.turn].Commands, canonicalBody)
	engine.issued++
	return engine.save(engine.ctx, false)
}

func (engine *localCoordinator) closeTurn() error {
	if engine.quiescent {
		return &coordinatorDivergenceError{Reason: "duplicate turn boundary"}
	}
	if engine.replaying && engine.turn < len(engine.replayTurns) && engine.issued < len(engine.replayTurns[engine.turn].Commands) {
		return &coordinatorDivergenceError{Reason: "turn ended before recorded command suffix"}
	}
	engine.record.Turns[engine.turn].Closed = true
	engine.quiescent = true
	if engine.replaying && engine.turn == len(engine.replayTurns)-1 && engine.delivered == len(engine.replayDeliveries) {
		engine.replaying = false
	}
	return engine.save(engine.ctx, false)
}

func (engine *localCoordinator) deliver(writer io.Writer) error {
	if !engine.quiescent {
		return nil
	}
	key := ""
	replay := engine.delivered < len(engine.replayDeliveries)
	if replay {
		key = engine.replayDeliveries[engine.delivered]
	} else {
		already := map[string]bool{}
		for _, key := range engine.record.Deliveries {
			already[key] = true
		}
		for _, ready := range engine.record.Settlements {
			if !already[ready] {
				key = ready
				break
			}
		}
	}
	if key == "" {
		return nil
	}
	var result coordinatorpb.InvokeResult
	if err := protojson.Unmarshal(engine.record.Results[key], &result); err != nil {
		return err
	}
	engine.turn++
	engine.issued = 0
	engine.delivered++
	engine.quiescent = false
	result.Turn = pointer(uint32(engine.turn))
	if !replay {
		engine.record.Deliveries = append(engine.record.Deliveries, key)
		engine.record.Turns = append(engine.record.Turns, coordinatorTurn{Commands: []json.RawMessage{}})
		// Persist delivery BEFORE releasing the result to CPython.
		if err := engine.save(context.WithoutCancel(engine.ctx), false); err != nil {
			return err
		}
	}
	body, err := coordinatorProtoJSON(&result)
	if err != nil {
		return err
	}
	if err := validateJSONAgainstSchema(string(schema.CoordinatorSchemaJSON), body); err != nil {
		return err
	}
	_, err = writer.Write(append(body, '\n'))
	return err
}

func (engine *localCoordinator) complete(body []byte, descriptor StepInvocationDescriptor) error {
	if engine.replaying && (engine.turn < len(engine.replayTurns)-1 || engine.issued < len(engine.replayTurns[engine.turn].Commands) || engine.delivered < len(engine.replayDeliveries)) {
		return &coordinatorDivergenceError{Reason: "early completion before recorded suffix"}
	}
	for key := range engine.commands {
		if engine.record.Results[key] == nil {
			return fmt.Errorf("coordinator completed with outstanding activities")
		}
	}
	if len(engine.record.Deliveries) != len(engine.commands) {
		return fmt.Errorf("coordinator completed before consuming activity results")
	}
	var complete coordinatorpb.CoordinatorComplete
	if err := protojson.Unmarshal(body, &complete); err != nil {
		return err
	}
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil {
		return err
	}
	if len(engine.record.Final) > 0 && !bytes.Equal(canonicalBody, engine.record.Final) {
		return &coordinatorDivergenceError{Reason: "final output differs"}
	}
	verification, stop := context.WithTimeout(context.WithoutCancel(engine.ctx), 5*time.Second)
	output, err := resolveOutputArtifact(verification, engine.store, descriptor, engine.index)
	stop()
	if err != nil {
		return fmt.Errorf("coordinator final output verification failed: %w", err)
	}
	if complete.Output.GetKey() != output.Published.Manifest.Key || complete.Output.GetHash() != output.Published.Manifest.Hash {
		return fmt.Errorf("coordinator final publication identity mismatch")
	}
	key := runResultKey(engine.manifest.ProjectKey, engine.manifest.RunID)
	if _, err := engine.store.Put(engine.ctx, key, output.Body, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return err
	}
	engine.record.Final = canonicalBody
	engine.record.Turns[engine.turn].Closed = true
	engine.manifest.Status = StatusSucceeded
	engine.manifest.Result = &runjournal.DataArtifact{Key: key.String(), Hash: canonical.DigestBytes(output.Body), ContentType: jsonContentType, Schema: descriptor.Output.Schema}
	return engine.save(context.WithoutCancel(engine.ctx), false)
}

func (engine *localCoordinator) loop(descriptor StepInvocationDescriptor) error {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	defer inputWriter.Close()
	defer outputReader.Close()
	processCtx, cancel := context.WithCancel(engine.ctx)
	defer cancel()
	processDone := make(chan error, 1)
	go func() {
		err := taskprocess.RunDuplex(processCtx, []string{engine.config.Interpreter, "-I", "-m", "massive.runner", "--coordinator"}, "", nil, inputReader, outputWriter, os.Stderr)
		inputReader.Close()
		outputWriter.Close()
		processDone <- err
	}()
	// Always reap the coordinator before reconciling its outstanding activities.
	defer func() { cancel(); inputWriter.Close(); outputReader.Close(); <-processDone }()
	frames := make(chan []byte)
	readErrors := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(outputReader)
		scanner.Buffer(make([]byte, 4096), coordinatorFrameLimit)
		for scanner.Scan() {
			body := append([]byte(nil), scanner.Bytes()...)
			select {
			case frames <- body:
			case <-processCtx.Done():
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = fmt.Errorf("coordinator interrupted: EOF before terminal message")
		}
		readErrors <- err
	}()
	if _, err := inputWriter.Write(append(append([]byte(nil), engine.record.Start...), '\n')); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := engine.dispatch(); err != nil {
			return err
		}
		if err := engine.deliver(inputWriter); err != nil {
			return err
		}
		select {
		case <-engine.ctx.Done():
			return engine.ctx.Err()
		case report := <-engine.outcomes:
			if err := engine.settle(report); err != nil {
				return err
			}
		case <-ticker.C:
		case err := <-readErrors:
			return err
		case body := <-frames:
			if len(body) == 0 || !utf8.Valid(body) || !json.Valid(body) {
				return fmt.Errorf("invalid coordinator NDJSON frame")
			}
			if err := validateJSONAgainstSchema(string(schema.CoordinatorSchemaJSON), body); err != nil {
				return fmt.Errorf("invalid coordinator protocol frame: %w", err)
			}
			var tag struct {
				Kind string `json:"kind"`
				Turn int    `json:"turn"`
			}
			if err := json.Unmarshal(body, &tag); err != nil {
				return err
			}
			if tag.Turn != engine.turn {
				return &coordinatorDivergenceError{Reason: "unexpected delivery turn"}
			}
			switch tag.Kind {
			case "InvokeCommand":
				if err := engine.accept(body); err != nil {
					return err
				}
			case "CoordinatorQuiescent":
				if err := engine.closeTurn(); err != nil {
					return err
				}
			case "CoordinatorComplete":
				return engine.complete(body, descriptor)
			case "CoordinatorFail":
				if engine.replaying {
					return &coordinatorDivergenceError{Reason: "coordinator stopped before replay completed"}
				}
				var failure coordinatorpb.CoordinatorFail
				if err := protojson.Unmarshal(body, &failure); err != nil {
					return err
				}
				if failure.FatalMessage != nil {
					return &FatalRunError{Failure: runjournal.FatalFailure{Message: failure.GetFatalMessage(), Type: failure.GetFatalType(), NodeID: "coordinator"}}
				}
				return fmt.Errorf("coordinator failed: %s", failure.GetDiagnostic())
			default:
				return fmt.Errorf("unexpected coordinator protocol variant")
			}
		}
	}
}
