package plan

import (
	"bytes"
	"encoding/json"
	"fmt"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/encoding/protojson"
)

// ValidateExecution checks plan-level execution semantics at every reader,
// including callers passing a typed plan directly to a target. A valid hash
// does not prove that execution requirements are meaningful.
func ValidateExecution(p *planpb.WorkflowPlan) error {
	graph := p.GetGraph()
	if graph == nil {
		return fmt.Errorf("workflow plan is missing graph; rebuild the plan")
	}
	if graph.DeadlineSeconds == nil && graph.GetExitHook() == nil {
		return nil
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.WorkflowSpecSchemaJSON))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("workflow-spec.schema.json", document); err != nil {
		return err
	}
	if graph.DeadlineSeconds != nil {
		schema, err := compiler.Compile("workflow-spec.schema.json#/properties/workflow/properties/deadlineSeconds")
		if err != nil {
			return err
		}
		if err := schema.Validate(json.Number(fmt.Sprint(graph.GetDeadlineSeconds()))); err != nil {
			return fmt.Errorf("workflow plan graph.deadlineSeconds violates the workflow deadline schema; rebuild the plan: %w", err)
		}
	}
	hook := graph.GetExitHook()
	if hook == nil {
		return nil
	}
	body, err := protojson.Marshal(hook)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return err
	}
	schema, err := compiler.Compile("workflow-spec.schema.json#/$defs/exitHookNode")
	if err != nil {
		return err
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("workflow plan graph.exitHook violates the exit hook schema; rebuild the plan: %w", err)
	}
	// Project the hook's semantic dependencies onto the authoring validator so
	// its fixed schemas, identity, references, and once-only rule have one owner.
	var node spec.GraphNode
	if err := json.Unmarshal(body, &node); err != nil {
		return err
	}
	authored := &spec.WorkflowSpec{
		Graph:   spec.Graph{ExitHook: &node},
		Schemas: map[string]json.RawMessage{}, Symbols: map[string]spec.Symbol{}, Contracts: map[string]spec.ExecutionContract{},
	}
	for _, entry := range p.GetSchemas() {
		authored.Schemas[entry.GetHash()] = json.RawMessage(entry.GetCanonicalJson())
	}
	for _, entry := range p.GetSymbols() {
		authored.Symbols[entry.GetSymbolRef()] = spec.Symbol{PackageID: entry.GetPackageId(), Language: entry.GetLanguage(), Module: entry.GetModule(), Export: entry.GetExport()}
	}
	for _, entry := range p.GetContracts() {
		contract := spec.ExecutionContract{EnvironmentRef: entry.GetEnvironmentRef()}
		if retry := entry.GetRetry(); retry != nil {
			contract.Retry = &spec.RetryPolicy{MaxAttempts: retry.GetMaxAttempts()}
		}
		authored.Contracts[entry.GetContractRef()] = contract
	}
	nodes := map[string]spec.GraphNode{}
	for _, entry := range graph.GetNodes() {
		nodes[entry.GetId()] = spec.GraphNode{ID: entry.GetId()}
	}
	if diagnostics := spec.ValidateExitHook(authored, nodes); len(diagnostics) != 0 {
		return fmt.Errorf("workflow plan has an invalid exit hook; rebuild the plan: %w", &spec.DiagnosticsError{Diagnostics: diagnostics})
	}
	symbol := authored.Symbols[node.SymbolRef]
	packageExists := false
	for _, entry := range p.GetSourcePackages() {
		if entry.GetPackageId() == symbol.PackageID && entry.GetLanguage() == symbol.Language {
			packageExists = true
		}
	}
	if !packageExists {
		return fmt.Errorf("workflow plan graph.exitHook symbol references an unknown source package; rebuild the plan")
	}
	environmentExists := false
	for _, entry := range p.GetEnvironments() {
		if entry.GetEnvRef() == authored.Contracts[node.ContractRef].EnvironmentRef {
			environmentExists = true
		}
	}
	if !environmentExists {
		return fmt.Errorf("workflow plan graph.exitHook contract references an unknown environment; rebuild the plan")
	}
	return nil
}
