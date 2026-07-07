package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
)

func TestDescriptorsValidateAndMatchLinearGolden(t *testing.T) {
	compiled := compileFixture(t, "linear-chain")
	storeRoot := t.TempDir()
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	invoker := &functionalStepInvoker{storeRoot: storeRoot}

	result, err := Run(context.Background(), RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         "acme/security-workflows",
		RunID:             "run-descriptor-0001",
		SourcePackageRoot: sourceRoot,
		StepInvoker:       invoker,
	}, []byte("20"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", result.Status)
	}
	if len(invoker.descriptors) != 3 {
		t.Fatalf("captured descriptors = %d, want 3", len(invoker.descriptors))
	}

	validateDescriptorSchema(t, invoker.descriptors[0])
	actual := normalizeDescriptorJSON(t, mustMarshalCanonical(t, invoker.descriptors[0]), "run-descriptor-0001", storeRoot)
	golden := normalizeDescriptorJSON(t, readRepoFile(t, "conformance", "fixtures", "descriptors", "linear-chain", "descriptor.json"), "run-linear-chain-0001", "/tmp/massive-conformance-store")
	if !bytes.Equal(actual, golden) {
		t.Fatalf("descriptor mismatch\nactual:   %s\nexpected: %s", actual, golden)
	}
}

func TestTamperedOutputFailsHashValidation(t *testing.T) {
	compiled := compileFixture(t, "linear-chain")
	storeRoot := t.TempDir()
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	invoker := &functionalStepInvoker{storeRoot: storeRoot}

	_, err := Run(context.Background(), RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         "acme/security-workflows",
		RunID:             "run-tamper-0001",
		SourcePackageRoot: sourceRoot,
		StepInvoker:       invoker,
		Hooks: RunHooks{
			AfterStepInvocation: func(_ context.Context, descriptor StepInvocationDescriptor) error {
				if descriptor.NodeID != "double" {
					return nil
				}
				return os.WriteFile(filepath.Join(storeRoot, filepath.FromSlash(descriptor.Output.Artifact.Key)), []byte("41"), 0o644)
			},
		},
	}, []byte("20"))
	if err == nil {
		t.Fatal("Run succeeded after output tampering")
	}
	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("error = %T, want RunError", err)
	}
	if !strings.Contains(runErr.Diagnostic, "hash mismatch") {
		t.Fatalf("diagnostic = %q, want hash mismatch", runErr.Diagnostic)
	}
	if runErr.Result == nil || runErr.Result.Status != StatusFailed {
		t.Fatalf("result = %#v, want failed result", runErr.Result)
	}
}

type functionalStepInvoker struct {
	storeRoot   string
	descriptors []StepInvocationDescriptor
}

func (i *functionalStepInvoker) InvokeSteps(ctx context.Context, batch StepInvocationBatch) ([]StepInvocationOutcome, error) {
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: i.storeRoot})
	if err != nil {
		return nil, err
	}

	outcomes := make([]StepInvocationOutcome, 0, len(batch.Steps))
	for _, step := range batch.Steps {
		descriptor := step.Descriptor
		i.descriptors = append(i.descriptors, descriptor)
		inputObject, err := store.Get(ctx, datastore.MustKey(descriptor.Input.Artifact.Key))
		if err != nil {
			return nil, err
		}
		output, err := runFixtureStep(descriptor.NodeID, inputObject.Body)
		if err != nil {
			return nil, err
		}
		if _, err := store.Put(ctx, datastore.MustKey(descriptor.Output.Artifact.Key), output, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
			return nil, err
		}
		outcomes = append(outcomes, StepInvocationOutcome{
			NodeID:             descriptor.NodeID,
			Attempt:            descriptor.Attempt,
			Status:             StatusSucceeded,
			ExpectedOutputHash: canonical.DigestBytes(output),
		})
	}
	return outcomes, nil
}

func runFixtureStep(nodeID string, inputBytes []byte) ([]byte, error) {
	var input any
	if err := json.Unmarshal(inputBytes, &input); err != nil {
		return nil, err
	}

	var output any
	switch nodeID {
	case "double":
		output = input.(float64) * 2
	case "increment":
		output = input.(float64) + 1
	case "label":
		output = "value:41"
	default:
		return nil, errors.New("unknown fixture step " + nodeID)
	}
	return marshalCanonicalJSON(output)
}

func compileFixture(t *testing.T, name string) *plan.CompileResult {
	t.Helper()

	specData := readRepoFile(t, "conformance", "fixtures", "specs", name, "workflow-spec.json")
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func validateDescriptorSchema(t *testing.T, descriptor StepInvocationDescriptor) {
	t.Helper()

	workspace := t.TempDir()
	descriptorPath := filepath.Join(workspace, "descriptor.json")
	if err := os.WriteFile(descriptorPath, mustMarshalCanonical(t, descriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(workspace, "validate_descriptor.ts")
	script := `import { parseStepInvocationDescriptorText } from "` + filepath.ToSlash(filepath.Join(repoRootForTest(t), "packages", "sdk", "src", "runner", "descriptor.ts")) + `";
await parseStepInvocationDescriptorText(await Deno.readTextFile(Deno.args[0]));
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("deno", "run", "--config", filepath.Join(repoRootForTest(t), "deno.json"), "--allow-read", scriptPath, descriptorPath)
	cmd.Dir = repoRootForTest(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("descriptor schema validation failed: %v\n%s", err, output)
	}
}

var (
	descriptorDigestRefPattern  = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
	descriptorDigestPathPattern = regexp.MustCompile(`sha256-[0-9a-f]{64}`)
)

func normalizeDescriptorJSON(t *testing.T, data []byte, runID string, storeRoot string) []byte {
	t.Helper()

	normalized := string(data)
	normalized = strings.ReplaceAll(normalized, runID, "run-linear-chain-0001")
	normalized = strings.ReplaceAll(normalized, storeRoot, "/tmp/massive-conformance-store")
	normalized = strings.ReplaceAll(normalized, SourceDirectoryContentType, "application/zstd")
	normalized = descriptorDigestRefPattern.ReplaceAllString(normalized, "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	normalized = descriptorDigestPathPattern.ReplaceAllString(normalized, "sha256-0000000000000000000000000000000000000000000000000000000000000000")
	canonicalJSON, err := canonical.CanonicalizeJSON([]byte(normalized))
	if err != nil {
		t.Fatal(err)
	}
	return canonicalJSON
}

func mustMarshalCanonical(t *testing.T, value any) []byte {
	t.Helper()

	body, err := marshalCanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func readRepoFile(t *testing.T, parts ...string) []byte {
	t.Helper()

	path := filepath.Join(append([]string{repoRootForTest(t)}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func repoRootForTest(t *testing.T) string {
	t.Helper()

	root, err := repoRootFrom(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
