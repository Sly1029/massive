package schema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestProtoSchemasCompile(t *testing.T) {
	runProtoc(t, "workflow-plan.proto", "bundle-manifest.proto")
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

	outDir := t.TempDir()
	protocArgs := []string{
		"-I", ".",
		"--go_out=" + outDir,
		"--go_opt=paths=source_relative",
	}
	protocArgs = append(protocArgs, args...)

	cmd := exec.Command("/opt/homebrew/bin/protoc", protocArgs...)
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH")+":/Users/rohit/go/bin")
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
