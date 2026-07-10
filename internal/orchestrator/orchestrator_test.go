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
)

func TestDescriptorsValidateAndMatchLinearGolden(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, manifests := compileConsistentFixture(t, "linear-chain", sourceRoot)
	invoker := &functionalStepInvoker{storeRoot: storeRoot}

	result, err := Run(context.Background(), RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         "acme/security-workflows",
		RunID:             "run-descriptor-0001",
		SourcePackageRoot: sourceRoot,
		SourceManifests:   manifests,
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

	// Normalization below zeroes every digest, which would hide a regression
	// where packageHash and sourceArchive.hash collapse to the same value.
	// Assert their distinct provenance on the un-normalized descriptor.
	planPackageHash := compiled.Plan.GetSourcePackages()[0].GetPackageHash()
	descriptor := invoker.descriptors[0]
	if descriptor.SourcePackage.PackageHash != planPackageHash {
		t.Fatalf("descriptor packageHash = %s, want plan packageHash %s", descriptor.SourcePackage.PackageHash, planPackageHash)
	}
	archiveBody := getObject(t, storeRoot, descriptor.SourcePackage.SourceArchive.Key)
	if wantHash := canonical.DigestBytes(archiveBody.Body); descriptor.SourcePackage.SourceArchive.Hash != wantHash {
		t.Fatalf("descriptor sourceArchive.hash = %s, want stored body digest %s", descriptor.SourcePackage.SourceArchive.Hash, wantHash)
	}
	if descriptor.SourcePackage.SourceArchive.Hash == descriptor.SourcePackage.PackageHash {
		t.Fatal("sourceArchive.hash must differ from packageHash under the v0 pointer artifact shape")
	}

	validateDescriptorSchema(t, descriptor)
	actual := normalizeDescriptorJSON(t, mustMarshalCanonical(t, descriptor), "run-descriptor-0001", storeRoot)
	golden := normalizeDescriptorJSON(t, readRepoFile(t, "conformance", "fixtures", "descriptors", "linear-chain", "descriptor.json"), "run-linear-chain-0001", "/tmp/massive-conformance-store")
	if !bytes.Equal(actual, golden) {
		t.Fatalf("descriptor mismatch\nactual:   %s\nexpected: %s", actual, golden)
	}
}

func TestTamperedOutputFailsHashValidation(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, manifests := compileConsistentFixture(t, "linear-chain", sourceRoot)
	invoker := &functionalStepInvoker{storeRoot: storeRoot}

	_, err := Run(context.Background(), RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         "acme/security-workflows",
		RunID:             "run-tamper-0001",
		SourcePackageRoot: sourceRoot,
		SourceManifests:   manifests,
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

func TestSourceSnapshotIsDeterministicAcrossRuns(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, manifests := compileConsistentFixture(t, "linear-chain", sourceRoot)
	planPackageHash := compiled.Plan.GetSourcePackages()[0].GetPackageHash()

	run := func(runID string) StepInvocationDescriptor {
		invoker := &functionalStepInvoker{storeRoot: storeRoot}
		result, err := Run(context.Background(), RunConfig{
			Plan:              compiled.Plan,
			DatastoreRoot:     storeRoot,
			ProjectID:         "acme/security-workflows",
			RunID:             runID,
			SourcePackageRoot: sourceRoot,
			SourceManifests:   manifests,
			StepInvoker:       invoker,
		}, []byte("20"))
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != StatusSucceeded {
			t.Fatalf("status = %s, want succeeded", result.Status)
		}
		return invoker.descriptors[0]
	}

	// The snapshot is content-addressed by (store, package hash), so both runs
	// resolve the same immutable directory.
	snapshotFile := filepath.Join(storeRoot, ".snapshots", strings.Replace(planPackageHash, "sha256:", "sha256-", 1), "workflow.ts")

	first := run("run-determinism-0001")
	firstInfo, err := os.Stat(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	firstBody := getObject(t, storeRoot, first.SourcePackage.SourceArchive.Key).Body

	second := run("run-determinism-0002")
	secondInfo, err := os.Stat(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	secondBody := getObject(t, storeRoot, second.SourcePackage.SourceArchive.Key).Body

	if first.SourcePackage.SourceArchive.Key != second.SourcePackage.SourceArchive.Key {
		t.Fatalf("archive keys differ across runs: %s vs %s", first.SourcePackage.SourceArchive.Key, second.SourcePackage.SourceArchive.Key)
	}
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatalf("pointer artifacts differ across runs:\nfirst:  %s\nsecond: %s", firstBody, secondBody)
	}
	for label, descriptor := range map[string]StepInvocationDescriptor{"first": first, "second": second} {
		body := getObject(t, storeRoot, descriptor.SourcePackage.SourceArchive.Key).Body
		if want := canonical.DigestBytes(body); descriptor.SourcePackage.SourceArchive.Hash != want {
			t.Fatalf("%s run: descriptor sourceArchive.hash = %s, want stored body digest %s", label, descriptor.SourcePackage.SourceArchive.Hash, want)
		}
	}
	// Reuse must not repopulate: the snapshot is created exactly once.
	if !firstInfo.ModTime().Equal(secondInfo.ModTime()) {
		t.Fatalf("snapshot was rewritten between runs (mod times %v vs %v)", firstInfo.ModTime(), secondInfo.ModTime())
	}
}

func TestHostileRunIDRejectedBeforeSideEffects(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, manifests := compileConsistentFixture(t, "linear-chain", sourceRoot)

	for _, hostile := range []string{"../escape", "../../etc", "a/b", "..", "foo/../bar"} {
		t.Run(hostile, func(t *testing.T) {
			_, err := Run(context.Background(), RunConfig{
				Plan:              compiled.Plan,
				DatastoreRoot:     storeRoot,
				ProjectID:         "acme/security-workflows",
				RunID:             hostile,
				SourcePackageRoot: sourceRoot,
				SourceManifests:   manifests,
				StepInvoker:       &functionalStepInvoker{storeRoot: storeRoot},
			}, []byte("20"))
			if err == nil {
				t.Fatalf("Run accepted hostile run id %q", hostile)
			}
			var invalid *InvalidRunInputError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %T (%v), want *InvalidRunInputError", err, err)
			}
			if invalid.Field != "run id" {
				t.Fatalf("error field = %q, want run id", invalid.Field)
			}
			assertNoRunSideEffects(t, storeRoot)
		})
	}
}

func TestHostilePackageHashRejectedBeforeSideEffects(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, manifests := compileConsistentFixture(t, "linear-chain", sourceRoot)
	// A package hash with traversal components must be rejected before it is
	// interpolated into a snapshot path or datastore key.
	hostile := "sha256:../../../../../../etc/passwd"
	compiled.Plan.GetSourcePackages()[0].PackageHash = &hostile

	_, err := Run(context.Background(), RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         "acme/security-workflows",
		RunID:             "run-badhash-0001",
		SourcePackageRoot: sourceRoot,
		SourceManifests:   manifests,
		StepInvoker:       &functionalStepInvoker{storeRoot: storeRoot},
	}, []byte("20"))
	if err == nil {
		t.Fatal("Run accepted a traversal package hash")
	}
	var invalid *InvalidRunInputError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %T (%v), want *InvalidRunInputError", err, err)
	}
	if invalid.Field != "source package hash" {
		t.Fatalf("error field = %q, want source package hash", invalid.Field)
	}
	assertNoRunSideEffects(t, storeRoot)
}

// assertNoRunSideEffects checks that a rejected run wrote nothing: no snapshot
// tree, no project run artifacts, and no traversal escape outside the store.
func assertNoRunSideEffects(t *testing.T, storeRoot string) {
	t.Helper()

	for _, sub := range []string{".snapshots", "projects"} {
		if _, statErr := os.Stat(filepath.Join(storeRoot, sub)); !os.IsNotExist(statErr) {
			t.Fatalf("rejected run left %s behind under the store (stat err %v)", sub, statErr)
		}
	}
	// A ".snapshots" sibling of the store would signal a traversal escape.
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(storeRoot), ".snapshots")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected run created a .snapshots path outside the store (stat err %v)", statErr)
	}
}

func TestPopulateSnapshotRejectsSymlinkEscape(t *testing.T) {
	// A source file that is a symlink pointing outside the source root must be
	// rejected even when its (followed) content matches the manifest hash, so
	// only the containment guard — not the drift check — can be doing the work.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.ts")
	if err := os.WriteFile(secret, []byte("export const secret = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(sourceDir, "workflow.ts")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	content, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	files := []SourcePackageFile{{Path: "workflow.ts", Hash: canonical.DigestBytes(content)}}

	err = populateSnapshot(sourceDir, t.TempDir(), files)
	if err == nil {
		t.Fatal("populateSnapshot followed a symlink outside the source root")
	}
	if !strings.Contains(err.Error(), "outside the source package root") {
		t.Fatalf("error = %v, want outside-root rejection", err)
	}
}

func TestPackageHashValidationRejectsUnsafeRefs(t *testing.T) {
	safe := "sha256:" + strings.Repeat("a", 64)
	if !validSHA256Ref(safe) {
		t.Fatalf("rejected valid ref %q", safe)
	}
	for _, bad := range []string{
		"",
		"sha256:" + strings.Repeat("a", 63), // too short
		"sha256:" + strings.Repeat("A", 64), // upper-case hex
		"sha256-" + strings.Repeat("a", 64), // wrong separator
		"sha256:../../../../etc/passwd" + strings.Repeat("a", 36),
		"sha256:" + strings.Repeat("a", 64) + "/x", // trailing segment
	} {
		if validSHA256Ref(bad) {
			t.Fatalf("accepted unsafe package hash %q", bad)
		}
	}
}

func TestSourcePackageHashGoldenVector(t *testing.T) {
	// Non-circular golden vector: a fixed manifest with literal file hashes and
	// the expected package hash computed once from the TS hashSourcePackage
	// construction (packages/sdk/src/compile.ts) and hard-coded here and in
	// packages/sdk/test/source-package-hash.test.ts. The e2e tests derive the
	// package hash via this same Go function, so this constant is what keeps the
	// Go and TS constructions honest against each other.
	// TODO: promote this vector into conformance/fixtures/hashing once the
	// frozen contract fixtures are opened for additions.
	files := []SourcePackageFile{
		{Path: "src/a.ts", Hash: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
		{Path: "src/b.ts", Hash: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
		{Path: "src/nested/c.ts", Hash: "sha256:3333333333333333333333333333333333333333333333333333333333333333"},
	}
	const want = "sha256:88780f05b7195a396acac9aa6ddbea16445f275dfc10f32c94972beb59a711cb"

	got, err := recomputeSourcePackageHash(files)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("source package hash = %s, want %s", got, want)
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

func validateDescriptorSchema(t *testing.T, descriptor StepInvocationDescriptor) {
	t.Helper()

	repoRoot := repoRootForTest(t)
	workspace := t.TempDir()
	descriptorPath := filepath.Join(workspace, "descriptor.json")
	if err := os.WriteFile(descriptorPath, mustMarshalCanonical(t, descriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(workspace, "validate_descriptor.ts")
	script := `import { parseStepInvocationDescriptorText } from "` + filepath.ToSlash(filepath.Join(repoRoot, "packages", "sdk", "src", "runner", "descriptor.ts")) + `";
await parseStepInvocationDescriptorText(await Deno.readTextFile(Deno.args[0]));
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("deno", "run", "--config", filepath.Join(repoRoot, "deno.json"), "--allow-read="+strings.Join([]string{repoRoot, workspace}, ","), scriptPath, descriptorPath)
	cmd.Dir = repoRoot
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
