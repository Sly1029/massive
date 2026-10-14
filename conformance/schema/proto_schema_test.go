package schema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/coordinatorpb"
	"github.com/Sly1029/massive/conformance/schema/failurepb"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/conformance/schema/runtimepb"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestProtoSchemasCompile(t *testing.T) {
	runProtoc(t, "workflow-plan.proto", "bundle-manifest.proto", "step-invocation.proto", "materialization.proto", "coordinator.proto", "item-failure.proto")
}

func TestStepInvocationJSONRoundTrip(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", "fixtures", "descriptors", "linear-chain", "descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}

	var descriptor runtimepb.StepInvocationDescriptor
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(input, &descriptor); err != nil {
		t.Fatal(err)
	}
	output, err := (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(&descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonFieldTree(t, input), jsonFieldTree(t, output)) {
		t.Fatalf("step invocation JSON field tree changed after protojson round trip\ninput:\n%s\n\noutput:\n%s", input, output)
	}
}

func TestWorkflowPlanJSONRoundTrip(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", "fixtures", "plans", "linear-chain", "workflow-plan.json"))
	if err != nil {
		t.Fatal(err)
	}

	var plan planpb.WorkflowPlan
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(input, &plan); err != nil {
		t.Fatal(err)
	}

	output, err := protojson.Marshal(&plan)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(jsonFieldTree(t, input), jsonFieldTree(t, output)) {
		t.Fatalf("workflow plan JSON field tree changed after protojson round trip\ninput:\n%s\n\noutput:\n%s", input, output)
	}
}

func runProtoc(t *testing.T, args ...string) {
	t.Helper()

	protoc, err := exec.LookPath("protoc")
	if err != nil {
		t.Skip("protoc not found in PATH; install protobuf to run the schema compile check")
	}
	if _, err := exec.LookPath("protoc-gen-go"); err != nil {
		t.Skip("protoc-gen-go not found in PATH; run: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest")
	}

	outDir := t.TempDir()
	protocArgs := []string{
		"-I", ".",
		"--go_out=" + outDir,
		"--go_opt=paths=source_relative",
	}
	protocArgs = append(protocArgs, args...)

	cmd := exec.Command(protoc, protocArgs...)
	cmd.Dir = "."
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("protoc %v failed: %v\n%s", args, err, output)
	}
}

func jsonFieldTree(t *testing.T, data []byte) any {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var fieldTree any
	if err := decoder.Decode(&fieldTree); err != nil {
		t.Fatal(err)
	}
	return fieldTree
}

func TestCoordinatorAndSharedFailureProjection(t *testing.T) {
	// The fixture is a real v3 invocation descriptor, shared with runner tests.
	body, err := os.ReadFile(filepath.Join("..", "fixtures", "descriptors", "linear-chain", "descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var descriptor runtimepb.StepInvocationDescriptor
	if err := protojson.Unmarshal(body, &descriptor); err != nil {
		t.Fatal(err)
	}
	kind, version := "CoordinatorStart", uint32(1)
	message := &coordinatorpb.CoordinatorStart{Kind: &kind, ProtocolVersion: &version, Descriptor_: &descriptor}
	wire, err := protojson.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded coordinatorpb.CoordinatorStart
	if err := protojson.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	roundtrip, err := protojson.Marshal(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonFieldTree(t, wire), jsonFieldTree(t, roundtrip)) {
		t.Fatal("coordinator projection changed")
	}
	attempts, diagnostic, failureKind := uint32(2), "bounded failure", "error"
	failure := &failurepb.ItemFailure{Attempts: &attempts, Diagnostic: &diagnostic, Kind: &failureKind}
	encoded, err := protojson.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{"attempts": json.Number("2"), "diagnostic": "bounded failure", "kind": "error"}
	if !reflect.DeepEqual(jsonFieldTree(t, encoded), expected) {
		t.Fatal("promoted failure fields changed")
	}
}
