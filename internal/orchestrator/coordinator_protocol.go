package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	schema "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/conformance/schema/coordinatorpb"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const coordinatorFrameLimit = 1 << 20

var coordinatorExplicitKey = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// CoordinatorDefinition is metadata, not a compiled GraphIR plan.
type CoordinatorDefinition struct {
	Canonical                 []byte
	Symbol                    *planpb.SymbolEntry
	Source                    *planpb.SourcePackageRef
	Defaults                  *planpb.ExecutionContract
	Environment               *planpb.EnvironmentRequirement
	Schemas                   []*planpb.SchemaEntry
	Files                     []SourcePackageFile
	InputSchema, OutputSchema string
	DeadlineSeconds           uint32
}

func ParseCoordinatorDefinition(body []byte) (*CoordinatorDefinition, error) {
	var wire struct {
		Kind            string              `json:"kind"`
		ProtocolVersion int                 `json:"protocolVersion"`
		Symbol          json.RawMessage     `json:"symbol"`
		Source          json.RawMessage     `json:"source"`
		Defaults        json.RawMessage     `json:"defaults"`
		Environment     json.RawMessage     `json:"environment"`
		Schemas         []json.RawMessage   `json:"schemas"`
		Files           []SourcePackageFile `json:"files"`
		InputSchema     string              `json:"inputSchema"`
		OutputSchema    string              `json:"outputSchema"`
		DeadlineSeconds uint32              `json:"deadlineSeconds"`
	}
	if err := validateJSONAgainstSchema(string(schema.CoordinatorSpecSchemaJSON), body); err != nil {
		return nil, fmt.Errorf("invalid coordinator definition: %w", err)
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	result := &CoordinatorDefinition{Symbol: &planpb.SymbolEntry{}, Source: &planpb.SourcePackageRef{}, Defaults: &planpb.ExecutionContract{}, Environment: &planpb.EnvironmentRequirement{}, Files: wire.Files, InputSchema: wire.InputSchema, OutputSchema: wire.OutputSchema, DeadlineSeconds: wire.DeadlineSeconds}
	for _, item := range []struct {
		body    []byte
		message proto.Message
	}{{wire.Symbol, result.Symbol}, {wire.Source, result.Source}, {wire.Defaults, result.Defaults}, {wire.Environment, result.Environment}} {
		if err := protojson.Unmarshal(item.body, item.message); err != nil {
			return nil, err
		}
	}
	for _, body := range wire.Schemas {
		var entry planpb.SchemaEntry
		if err := protojson.Unmarshal(body, &entry); err != nil {
			return nil, err
		}
		result.Schemas = append(result.Schemas, &entry)
	}
	var err error
	result.Canonical, err = canonical.CanonicalizeJSON(body)
	if err != nil {
		return nil, err
	}
	if result.Symbol.GetSymbolRef() != result.Symbol.GetPackageId()+":"+result.Symbol.GetModule()+"#"+result.Symbol.GetExport() || result.Symbol.GetPackageId() != result.Source.GetPackageId() {
		return nil, fmt.Errorf("coordinator symbol/source identity mismatch")
	}
	digest, err := recomputeSourcePackageHash(result.Files)
	if err != nil || digest != result.Source.GetPackageHash() {
		return nil, fmt.Errorf("coordinator source manifest identity mismatch")
	}
	return result, nil
}

func coordinatorProtoJSON(message proto.Message) ([]byte, error) {
	body, err := protojson.Marshal(message)
	if err != nil {
		return nil, err
	}
	return canonical.CanonicalizeJSON(body)
}

func parseCoordinatorCommand(body []byte) (*coordinatorpb.InvokeCommand, error) {
	if err := validateJSONAgainstSchema(string(schema.CoordinatorSchemaJSON), body); err != nil {
		return nil, fmt.Errorf("invalid coordinator frame")
	}
	var command coordinatorpb.InvokeCommand
	if err := protojson.Unmarshal(body, &command); err != nil {
		return nil, err
	}
	if command.GetKind() != "InvokeCommand" {
		return nil, fmt.Errorf("expected invocation command")
	}
	symbol := command.GetSymbol()
	ref := symbol.GetPackageId() + ":" + symbol.GetModule() + "#" + symbol.GetExport()
	key := command.GetInvocationKey()
	if command.GetSymbolRef() != ref || symbol.GetSymbolRef() != ref || !(key == ref || strings.HasPrefix(key, ref+"@")) || !ValidSafePathSegment(key) {
		return nil, fmt.Errorf("invalid coordinator invocation identity")
	}
	if key != ref && !coordinatorExplicitKey.MatchString(strings.TrimPrefix(key, ref+"@")) {
		return nil, fmt.Errorf("invalid explicit invocation key")
	}
	return &command, nil
}
