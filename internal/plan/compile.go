package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/spec"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	CompilerName    = "massive-go"
	CompilerVersion = "0.0.0"
)

type CompileResult struct {
	Plan          *planpb.WorkflowPlan
	CanonicalJSON []byte
	PlanHash      string
	SpecHash      string
	Schedule      Schedule
}

func Compile(workflowSpec *spec.WorkflowSpec, sourceJSON []byte) (*CompileResult, error) {
	schedule, err := BuildSchedule(workflowSpec.Graph)
	if err != nil {
		return nil, fmt.Errorf("build schedule: %w", err)
	}

	specHash, err := spec.RecomputedSpecHash(sourceJSON)
	if err != nil {
		return nil, err
	}

	schemaHashes, schemaEntries, err := compileSchemas(workflowSpec)
	if err != nil {
		return nil, err
	}
	environmentHashes, environments, err := compileEnvironments(workflowSpec)
	if err != nil {
		return nil, err
	}
	contractHashes, contracts, err := compileContracts(workflowSpec, environmentHashes)
	if err != nil {
		return nil, err
	}
	sourcePackages, err := compileSourcePackages(workflowSpec)
	if err != nil {
		return nil, err
	}

	// Fixture specs currently carry placeholder specHash values. The v0
	// compiler records the recomputed self-excluded value without hard-failing
	// on mismatch, so frontend rollout can converge without blocking WS-2.
	plan := &planpb.WorkflowPlan{
		SchemaVersion:  uint32Ptr(0),
		SpecHash:       stringPtr(specHash),
		Graph:          compileGraph(workflowSpec, schedule, schemaHashes, contractHashes),
		Schemas:        schemaEntries,
		Symbols:        compileSymbols(workflowSpec, schedule),
		SourcePackages: sourcePackages,
		Environments:   environments,
		Contracts:      contracts,
		Provenance: &planpb.CompilerProvenance{
			CompilerName:    stringPtr(CompilerName),
			CompilerVersion: stringPtr(CompilerVersion),
			SourceSpecHash:  stringPtr(specHash),
		},
	}

	planHash, err := hashPlan(plan)
	if err != nil {
		return nil, err
	}
	plan.PlanHash = stringPtr(planHash)

	canonicalJSON, err := MarshalCanonical(plan)
	if err != nil {
		return nil, err
	}

	return &CompileResult{
		Plan:          plan,
		CanonicalJSON: canonicalJSON,
		PlanHash:      planHash,
		SpecHash:      specHash,
		Schedule:      schedule,
	}, nil
}

func MarshalCanonical(plan *planpb.WorkflowPlan) ([]byte, error) {
	protoJSON, err := protojson.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow plan protojson: %w", err)
	}

	canonicalJSON, err := canonical.CanonicalizeJSON(protoJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workflow plan JSON: %w", err)
	}

	return canonicalJSON, nil
}

func hashPlan(plan *planpb.WorkflowPlan) (string, error) {
	// Hash a clone with planHash absent (self-exclusion rule in hashing.md)
	// so verifying an already-hashed plan never mutates the caller's message.
	unhashed, ok := proto.Clone(plan).(*planpb.WorkflowPlan)
	if !ok {
		return "", fmt.Errorf("clone workflow plan for hashing: unexpected message type %T", plan)
	}
	unhashed.PlanHash = nil

	protoJSON, err := protojson.Marshal(unhashed)
	if err != nil {
		return "", fmt.Errorf("marshal workflow plan for hash: %w", err)
	}

	hash, err := canonical.DigestJSON(protoJSON)
	if err != nil {
		return "", fmt.Errorf("hash workflow plan: %w", err)
	}

	return hash, nil
}

func compileGraph(workflowSpec *spec.WorkflowSpec, schedule Schedule, schemaHashes map[string]string, contractHashes map[string]string) *planpb.GraphIR {
	nodeByID := make(map[string]spec.GraphNode, len(workflowSpec.Graph.Nodes))
	for _, node := range workflowSpec.Graph.Nodes {
		nodeByID[node.ID] = node
	}

	nodes := make([]*planpb.GraphNode, 0, len(schedule.NodeOrder))
	for _, nodeID := range schedule.NodeOrder {
		node := nodeByID[nodeID]
		compiled := &planpb.GraphNode{
			Id:   stringPtr(node.ID),
			Kind: stringPtr(node.Kind),
		}
		if node.Kind == spec.NodeKindStep {
			compiled.InputSchema = stringPtr(schemaHashes[node.InputSchema])
			compiled.OutputSchema = stringPtr(schemaHashes[node.OutputSchema])
			compiled.SymbolRef = stringPtr(node.SymbolRef)
			compiled.ContractRef = stringPtr(contractHashes[node.ContractRef])
			compiled.MergeInputs = append(compiled.MergeInputs, node.MergeInputs...)
		}
		nodes = append(nodes, compiled)
	}

	edges := make([]*planpb.GraphEdge, 0, len(workflowSpec.Graph.Edges))
	for _, edge := range workflowSpec.Graph.Edges {
		edges = append(edges, &planpb.GraphEdge{From: stringPtr(edge.From), To: stringPtr(edge.To)})
	}

	return &planpb.GraphIR{
		WorkflowName: stringPtr(workflowSpec.Workflow.Name),
		InputSchema:  stringPtr(schemaHashes[workflowSpec.Workflow.InputSchema]),
		OutputSchema: stringPtr(schemaHashes[workflowSpec.Workflow.OutputSchema]),
		StartNode:    stringPtr(workflowSpec.Graph.Start),
		EndNode:      stringPtr(workflowSpec.Graph.End),
		Nodes:        nodes,
		Edges:        edges,
	}
}

func compileSchemas(workflowSpec *spec.WorkflowSpec) (map[string]string, []*planpb.SchemaEntry, error) {
	oldToNew := make(map[string]string, len(workflowSpec.Schemas))
	byHash := make(map[string]string, len(workflowSpec.Schemas))
	for oldHash, rawSchema := range workflowSpec.Schemas {
		canonicalJSON, err := canonical.CanonicalizeJSON(rawSchema)
		if err != nil {
			return nil, nil, fmt.Errorf("canonicalize schema %s: %w", oldHash, err)
		}
		newHash := canonical.DigestBytes(canonicalJSON)
		oldToNew[oldHash] = newHash
		byHash[newHash] = string(canonicalJSON)
	}

	oldHashes := sortedKeys(workflowSpec.Schemas)
	entries := make([]*planpb.SchemaEntry, 0, len(oldHashes))
	for _, oldHash := range oldHashes {
		hash := oldToNew[oldHash]
		entries = append(entries, &planpb.SchemaEntry{
			Hash:          stringPtr(hash),
			CanonicalJson: stringPtr(byHash[hash]),
		})
	}

	return oldToNew, entries, nil
}

func compileEnvironments(workflowSpec *spec.WorkflowSpec) (map[string]string, []*planpb.MaterializedEnvironment, error) {
	oldToNew := make(map[string]string, len(workflowSpec.Environments))
	for oldRef, environment := range workflowSpec.Environments {
		hash, err := hashJSONValue(environment)
		if err != nil {
			return nil, nil, fmt.Errorf("hash environment %s: %w", oldRef, err)
		}
		oldToNew[oldRef] = hash
	}

	oldRefs := sortedKeys(workflowSpec.Environments)
	entries := make([]*planpb.MaterializedEnvironment, 0, len(oldRefs))
	for _, oldRef := range oldRefs {
		newRef := oldToNew[oldRef]
		environment := workflowSpec.Environments[oldRef]
		// The plan carries the authored environment kind so backends can gate on
		// it. For the container escape hatch the image *is* the runtime, so the
		// materialized environment records the image; dependency materialization
		// for other kinds (WS-9) lands here later as a local runtime artifact.
		materialized := &planpb.MaterializedEnvironment{
			EnvRef:   stringPtr(newRef),
			SpecHash: stringPtr(newRef),
			Kind:     stringPtr(environment.Kind),
		}
		if environment.Kind == spec.EnvironmentKindContainer {
			materialized.Container = &planpb.ContainerRuntime{Image: stringPtr(environment.Image)}
		}
		entries = append(entries, materialized)
	}

	sort.Slice(entries, func(i, j int) bool { return canonical.LessUTF16(entries[i].GetEnvRef(), entries[j].GetEnvRef()) })
	return oldToNew, entries, nil
}

func compileContracts(workflowSpec *spec.WorkflowSpec, environmentHashes map[string]string) (map[string]string, []*planpb.ExecutionContract, error) {
	compiledByOldRef := make(map[string]*planpb.ExecutionContract, len(workflowSpec.Contracts))
	oldToNew := make(map[string]string, len(workflowSpec.Contracts))

	oldRefs := sortedKeys(workflowSpec.Contracts)
	for _, oldRef := range oldRefs {
		contract := workflowSpec.Contracts[oldRef]
		compiled := &planpb.ExecutionContract{
			EnvironmentRef: stringPtr(environmentHashes[contract.EnvironmentRef]),
		}
		// The compiler records the contract exactly as authored; resource
		// defaults are authoring-layer policy, not compiler policy.
		if contract.Resources != nil {
			resources := &planpb.ResourceRequirements{}
			if contract.Resources.CPU != "" {
				resources.Cpu = stringPtr(contract.Resources.CPU)
			}
			if contract.Resources.Memory != "" {
				resources.Memory = stringPtr(contract.Resources.Memory)
			}
			compiled.Resources = resources
		}
		for _, secret := range contract.Secrets {
			compiled.Secrets = append(compiled.Secrets, &planpb.SecretRef{Name: stringPtr(secret.Name), Ref: stringPtr(secret.Ref)})
		}
		if contract.Network != nil {
			compiled.Network = &planpb.NetworkPolicy{
				Egress: stringPtr(contract.Network.Egress),
				Hosts:  append([]string{}, contract.Network.Hosts...),
			}
		}

		hash, err := hashPlanMessage(compiled)
		if err != nil {
			return nil, nil, fmt.Errorf("hash contract %s: %w", oldRef, err)
		}
		compiled.ContractRef = stringPtr(hash)
		compiledByOldRef[oldRef] = compiled
		oldToNew[oldRef] = hash
	}

	entries := make([]*planpb.ExecutionContract, 0, len(compiledByOldRef))
	for _, oldRef := range oldRefs {
		entries = append(entries, compiledByOldRef[oldRef])
	}
	sort.Slice(entries, func(i, j int) bool {
		return canonical.LessUTF16(entries[i].GetContractRef(), entries[j].GetContractRef())
	})

	return oldToNew, entries, nil
}

func compileSymbols(workflowSpec *spec.WorkflowSpec, schedule Schedule) []*planpb.SymbolEntry {
	nodeByID := make(map[string]spec.GraphNode, len(workflowSpec.Graph.Nodes))
	for _, node := range workflowSpec.Graph.Nodes {
		nodeByID[node.ID] = node
	}

	entries := make([]*planpb.SymbolEntry, 0, len(workflowSpec.Symbols))
	seen := make(map[string]bool, len(workflowSpec.Symbols))
	for _, stepID := range schedule.StepIDs {
		node := nodeByID[stepID]
		if seen[node.SymbolRef] {
			continue
		}
		symbol := workflowSpec.Symbols[node.SymbolRef]
		entries = append(entries, &planpb.SymbolEntry{
			SymbolRef: stringPtr(node.SymbolRef),
			PackageId: stringPtr(symbol.PackageID),
			Language:  stringPtr(symbol.Language),
			Module:    stringPtr(symbol.Module),
			Export:    stringPtr(symbol.Export),
		})
		seen[node.SymbolRef] = true
	}

	for _, symbolRef := range sortedKeys(workflowSpec.Symbols) {
		if seen[symbolRef] {
			continue
		}
		symbol := workflowSpec.Symbols[symbolRef]
		entries = append(entries, &planpb.SymbolEntry{
			SymbolRef: stringPtr(symbolRef),
			PackageId: stringPtr(symbol.PackageID),
			Language:  stringPtr(symbol.Language),
			Module:    stringPtr(symbol.Module),
			Export:    stringPtr(symbol.Export),
		})
	}
	return entries
}

func compileSourcePackages(workflowSpec *spec.WorkflowSpec) ([]*planpb.SourcePackageRef, error) {
	packageIDs := sortedKeys(workflowSpec.SourcePackages)
	entries := make([]*planpb.SourcePackageRef, 0, len(packageIDs))
	for _, packageID := range packageIDs {
		sourcePackage := workflowSpec.SourcePackages[packageID]
		manifestHash, err := hashSourceManifest(sourcePackage)
		if err != nil {
			return nil, err
		}
		packagePathHash := strings.TrimPrefix(sourcePackage.PackageHash, "sha256:")
		archiveKey := sourcePackage.Artifact
		if archiveKey == "" {
			archiveKey = "packages/sha256-" + packagePathHash + "/source.tar.zst"
		}
		entries = append(entries, &planpb.SourcePackageRef{
			PackageId:   stringPtr(sourcePackage.PackageID),
			Language:    stringPtr(sourcePackage.Language),
			PackageHash: stringPtr(sourcePackage.PackageHash),
			Manifest: &planpb.ArtifactRef{
				Key:         stringPtr("packages/sha256-" + packagePathHash + "/source-manifest.json"),
				Hash:        stringPtr(manifestHash),
				ContentType: stringPtr("application/json"),
			},
			SourceArchive: &planpb.ArtifactRef{
				Key:         stringPtr(archiveKey),
				Hash:        stringPtr(sourcePackage.PackageHash),
				ContentType: stringPtr("application/zstd"),
			},
		})
	}
	return entries, nil
}

func hashSourceManifest(sourcePackage spec.SourcePackage) (string, error) {
	manifest := struct {
		Files []spec.SourcePackageFile `json:"files"`
	}{
		Files: sourcePackage.Files,
	}
	hash, err := hashJSONValue(manifest)
	if err != nil {
		return "", fmt.Errorf("hash source manifest for package %s: %w", sourcePackage.PackageID, err)
	}
	return hash, nil
}

func hashJSONValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal JSON value: %w", err)
	}
	hash, err := canonical.DigestJSON(data)
	if err != nil {
		return "", err
	}
	return hash, nil
}

func hashPlanMessage(message proto.Message) (string, error) {
	data, err := protojson.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("marshal plan field tree: %w", err)
	}
	return canonical.DigestJSON(data)
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return canonical.LessUTF16(keys[i], keys[j]) })
	return keys
}

func stringPtr(value string) *string {
	return &value
}

func uint32Ptr(value uint32) *uint32 {
	return &value
}
