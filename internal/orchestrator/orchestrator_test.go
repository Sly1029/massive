package orchestrator

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/artifact"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/spec"
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
	for _, descriptor := range invoker.descriptors {
		assertNoCredentialMaterial(t, mustMarshalCanonical(t, descriptor))
		assertLiveDescriptorValidAgainstFrozenSchema(t, descriptor)
	}
	manifest := getObject(t, storeRoot, result.ManifestKey)
	assertNoCredentialMaterial(t, manifest.Body)

	// Normalization below zeroes every digest, which would hide a regression
	// where packageHash and sourceArchive.hash collapse to the same value.
	// Assert their distinct provenance on the un-normalized descriptor.
	planPackageHash := compiled.Plan.GetSourcePackages()[0].GetPackageHash()
	descriptor := invoker.descriptors[0]
	if descriptor.SchemaVersion != 1 || descriptor.Encoding != "json-v1" {
		t.Fatalf("descriptor protocol = (%d, %q), want v1/json-v1", descriptor.SchemaVersion, descriptor.Encoding)
	}
	if descriptor.ProjectKey != NormalizeProjectKey("acme/security-workflows") {
		t.Fatalf("descriptor projectKey = %q", descriptor.ProjectKey)
	}
	if descriptor.Output.ManifestKey != runOutputManifestKey(descriptor.ProjectKey, descriptor.RunID, descriptor.NodeID, descriptor.Attempt).String() {
		t.Fatalf("descriptor output manifest key = %q", descriptor.Output.ManifestKey)
	}
	runManifest := readRunManifest(t, storeRoot, result.ProjectKey, result.RunID)
	outputsByNode := make(map[string]manifestPublishedArtifact, len(runManifest.Steps))
	for _, step := range runManifest.Steps {
		if len(step.Attempts) != 1 || step.Attempts[0].Output == nil {
			t.Fatalf("journal step %q attempts = %#v, want one published output", step.NodeID, step.Attempts)
		}
		outputsByNode[step.NodeID] = *step.Attempts[0].Output
	}
	for _, live := range invoker.descriptors {
		output, ok := outputsByNode[live.NodeID]
		if !ok {
			t.Fatalf("journal omitted output for descriptor node %q", live.NodeID)
		}
		if output.Manifest.Key != live.Output.ManifestKey || output.Manifest.ContentType != artifact.ManifestContentType {
			t.Fatalf("journal manifest ref for %q = %#v, want immutable published manifest", live.NodeID, output.Manifest)
		}
		if !strings.HasPrefix(output.Body.Key, "blobs/sha256/") || output.Body.ContentType != artifact.JSONContentType {
			t.Fatalf("journal body ref for %q = %#v, want content-addressed canonical JSON", live.NodeID, output.Body)
		}
		if output.Schema != live.Output.Schema {
			t.Fatalf("journal schema for %q = %q, descriptor output schema = %q", live.NodeID, output.Schema, live.Output.Schema)
		}
		manifestObject := getObject(t, storeRoot, output.Manifest.Key)
		if output.Manifest.Size != len(manifestObject.Body) {
			t.Fatalf("journal manifest size for %q = %d, stored size = %d", live.NodeID, output.Manifest.Size, len(manifestObject.Body))
		}
		bodyObject := getObject(t, storeRoot, output.Body.Key)
		if output.Body.Size != len(bodyObject.Body) {
			t.Fatalf("journal body size for %q = %d, stored size = %d", live.NodeID, output.Body.Size, len(bodyObject.Body))
		}
	}
	if descriptor.SourcePackage.PackageHash != planPackageHash {
		t.Fatalf("descriptor packageHash = %s, want plan packageHash %s", descriptor.SourcePackage.PackageHash, planPackageHash)
	}
	archiveBody := getObject(t, storeRoot, descriptor.SourcePackage.SourceArchive.Key)
	if descriptor.SourcePackage.SourceArchive.ContentType != SourceArchiveContentType || !strings.HasSuffix(descriptor.SourcePackage.SourceArchive.Key, "/source.tar") {
		t.Fatalf("source archive reference = %#v, want portable source.tar", descriptor.SourcePackage.SourceArchive)
	}
	if bytes.Contains(archiveBody.Body, []byte(storeRoot)) {
		t.Fatal("portable source archive contains local datastore path")
	}
	archive := tar.NewReader(bytes.NewReader(archiveBody.Body))
	header, err := archive.Next()
	if err != nil || header.Name != "workflow.ts" || header.Typeflag != tar.TypeReg {
		t.Fatalf("source archive first entry = %#v, %v; want regular workflow.ts", header, err)
	}
	if wantHash := canonical.DigestBytes(archiveBody.Body); descriptor.SourcePackage.SourceArchive.Hash != wantHash {
		t.Fatalf("descriptor sourceArchive.hash = %s, want stored body digest %s", descriptor.SourcePackage.SourceArchive.Hash, wantHash)
	}
	if descriptor.SourcePackage.SourceArchive.Hash == descriptor.SourcePackage.PackageHash {
		t.Fatal("sourceArchive.hash must differ from packageHash under the portable archive shape")
	}

	actual := normalizeDescriptorJSON(t, mustMarshalCanonical(t, descriptor), "run-descriptor-0001", storeRoot)
	golden := normalizeDescriptorJSON(t, readRepoFile(t, "conformance", "fixtures", "descriptors", "linear-chain", "descriptor.json"), "run-linear-chain-0001", "/tmp/massive-conformance-store")
	if !bytes.Equal(actual, golden) {
		t.Fatalf("descriptor mismatch\nactual:   %s\nexpected: %s", actual, golden)
	}
}

func assertLiveDescriptorValidAgainstFrozenSchema(t *testing.T, descriptor StepInvocationDescriptor) {
	t.Helper()

	descriptorPath := filepath.Join(t.TempDir(), "descriptor.json")
	if err := os.WriteFile(descriptorPath, mustMarshalCanonical(t, descriptor), 0o600); err != nil {
		t.Fatal(err)
	}
	root := repoRootForTest(t)
	parserURL := "file://" + filepath.ToSlash(filepath.Join(root, "packages", "sdk", "src", "runner", "descriptor.ts"))
	cmd := exec.Command(
		"deno", "eval", "--config", filepath.Join(root, "deno.json"),
		`const module = await import(Deno.args[0]); await module.parseStepInvocationDescriptorText(await Deno.readTextFile(Deno.args[1]));`,
		parserURL, descriptorPath,
	)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("live Go descriptor violates frozen schema: %v\n%s", err, output)
	}
}

func TestFrozenDescriptorSchemaRejectsRawProjectID(t *testing.T) {
	document := readRepoFile(t, "conformance", "fixtures", "descriptors", "linear-chain", "descriptor.json")
	document = bytes.Replace(
		document,
		[]byte(`"projectKey": "sha256-9999999999999999999999999999999999999999999999999999999999999999"`),
		[]byte(`"projectKey": "acme/security-workflows"`),
		1,
	)
	descriptorPath := filepath.Join(t.TempDir(), "descriptor.json")
	if err := os.WriteFile(descriptorPath, document, 0o600); err != nil {
		t.Fatal(err)
	}
	root := repoRootForTest(t)
	parserURL := "file://" + filepath.ToSlash(filepath.Join(root, "packages", "sdk", "src", "runner", "descriptor.ts"))
	cmd := exec.Command(
		"deno", "eval", "--config", filepath.Join(root, "deno.json"),
		`const module = await import(Deno.args[0]); await module.parseStepInvocationDescriptorText(await Deno.readTextFile(Deno.args[1]));`,
		parserURL, descriptorPath,
	)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("frozen descriptor schema accepted raw project id")
	}
	if !strings.Contains(string(output), "projectKey") {
		t.Fatalf("descriptor validation error = %q, want projectKey", output)
	}
}

func TestFrozenDescriptorSchemaConstrainsArtifactIdentitySegments(t *testing.T) {
	fixture := readRepoFile(t, "conformance", "fixtures", "descriptors", "linear-chain", "descriptor.json")
	root := repoRootForTest(t)
	parserURL := "file://" + filepath.ToSlash(filepath.Join(root, "packages", "sdk", "src", "runner", "descriptor.ts"))

	for field, original := range map[string]string{
		"runId":  "run-linear-chain-0001",
		"nodeId": "double",
	} {
		for _, value := range []string{"nested/value", ".", "..", strings.Repeat("a", 129)} {
			t.Run(field+" rejects "+value, func(t *testing.T) {
				document := replaceFixtureValue(t, fixture, `"`+field+`": "`+original+`"`, `"`+field+`": "`+value+`"`)
				descriptorPath := filepath.Join(t.TempDir(), "descriptor.json")
				if err := os.WriteFile(descriptorPath, document, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(
					"deno", "eval", "--config", filepath.Join(root, "deno.json"),
					`const module = await import(Deno.args[0]); await module.parseStepInvocationDescriptorText(await Deno.readTextFile(Deno.args[1]));`,
					parserURL, descriptorPath,
				)
				cmd.Dir = root
				output, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("frozen descriptor schema accepted unsafe %s %q", field, value)
				}
				if !strings.Contains(string(output), field) {
					t.Fatalf("descriptor validation error = %q, want %s", output, field)
				}
			})
		}
	}

	for field, original := range map[string]string{
		"runId":  "run-linear-chain-0001",
		"nodeId": "double",
	} {
		for _, value := range []string{"_step", ".hidden", strings.Repeat("a", 128)} {
			t.Run(field+" accepts "+value, func(t *testing.T) {
				document := replaceFixtureValue(t, fixture, `"`+field+`": "`+original+`"`, `"`+field+`": "`+value+`"`)
				descriptorPath := filepath.Join(t.TempDir(), "descriptor.json")
				if err := os.WriteFile(descriptorPath, document, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(
					"deno", "eval", "--config", filepath.Join(root, "deno.json"),
					`const module = await import(Deno.args[0]); await module.parseStepInvocationDescriptorText(await Deno.readTextFile(Deno.args[1]));`,
					parserURL, descriptorPath,
				)
				cmd.Dir = root
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("frozen descriptor schema rejected safe %s %q: %v\\n%s", field, value, err, output)
				}
			})
		}
	}
}

func replaceFixtureValue(t *testing.T, document []byte, old string, new string) []byte {
	t.Helper()
	if !bytes.Contains(document, []byte(old)) {
		t.Fatalf("fixture does not contain replacement target %q", old)
	}
	updated := bytes.Replace(document, []byte(old), []byte(new), 1)
	if bytes.Equal(updated, document) {
		t.Fatalf("fixture replacement %q -> %q did not change document", old, new)
	}
	return updated
}

func TestSafePathSegmentContractAgreesAcrossSchemasAndGo(t *testing.T) {
	type boundaryCase struct {
		value string
		valid bool
	}
	cases := []boundaryCase{
		{value: "safe_SEGMENT.@:#-Z", valid: true},
		{value: strings.Repeat("a", 128), valid: true},
		{value: "", valid: false},
		{value: ".", valid: false},
		{value: "..", valid: false},
		{value: "nested/value", valid: false},
		{value: `nested\\value`, valid: false},
		{value: strings.Repeat("a", 129), valid: false},
	}
	descriptorFixture := readRepoFile(t, "conformance", "fixtures", "descriptors", "linear-chain", "descriptor.json")
	workflowSpecFixture := readRepoFile(t, "conformance", "fixtures", "specs", "linear-chain", "workflow-spec.json")
	root := repoRootForTest(t)
	parserURL := "file://" + filepath.ToSlash(filepath.Join(root, "packages", "sdk", "src", "runner", "descriptor.ts"))

	for _, testCase := range cases {
		t.Run(fmt.Sprintf("%q", testCase.value), func(t *testing.T) {
			if got := validSafePathSegment(testCase.value); got != testCase.valid {
				t.Fatalf("Go safe path segment validation = %t, want %t", got, testCase.valid)
			}

			descriptor := replaceFixtureValue(t, descriptorFixture, `"runId": "run-linear-chain-0001"`, `"runId": "`+testCase.value+`"`)
			descriptorPath := filepath.Join(t.TempDir(), "descriptor.json")
			if err := os.WriteFile(descriptorPath, descriptor, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(
				"deno", "eval", "--config", filepath.Join(root, "deno.json"),
				`const module = await import(Deno.args[0]); await module.parseStepInvocationDescriptorText(await Deno.readTextFile(Deno.args[1]));`,
				parserURL, descriptorPath,
			)
			cmd.Dir = root
			_, err := cmd.CombinedOutput()
			if got := err == nil; got != testCase.valid {
				t.Fatalf("descriptor schema acceptance = %t, want %t", got, testCase.valid)
			}

			workflowSpec := replaceAllFixtureValues(t, workflowSpecFixture, `"__start"`, `"`+testCase.value+`"`)
			_, err = spec.Parse(workflowSpec)
			if got := err == nil; got != testCase.valid {
				t.Fatalf("WorkflowSpec schema acceptance = %t, want %t (error: %v)", got, testCase.valid, err)
			}
		})
	}
}

func replaceAllFixtureValues(t *testing.T, document []byte, old string, new string) []byte {
	t.Helper()
	if !bytes.Contains(document, []byte(old)) {
		t.Fatalf("fixture does not contain replacement target %q", old)
	}
	updated := bytes.ReplaceAll(document, []byte(old), []byte(new))
	if bytes.Equal(updated, document) {
		t.Fatalf("fixture replacement %q -> %q did not change document", old, new)
	}
	return updated
}

func TestRunOutputManifestKeyIncludesAttempt(t *testing.T) {
	projectKey := NormalizeProjectKey("acme/security-workflows")
	first := runOutputManifestKey(projectKey, "run-key-attempt", "double", 1)
	second := runOutputManifestKey(projectKey, "run-key-attempt", "double", 2)
	if first == second {
		t.Fatalf("attempt-specific manifest keys collide: %s", first)
	}
	if !strings.Contains(first.String(), "/double/1/output-manifest.json") {
		t.Fatalf("first attempt key = %q", first)
	}
	if !strings.Contains(second.String(), "/double/2/output-manifest.json") {
		t.Fatalf("second attempt key = %q", second)
	}
}

func TestTamperedOutputManifestFailsIntegrityValidation(t *testing.T) {
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
				return os.WriteFile(filepath.Join(storeRoot, filepath.FromSlash(descriptor.Output.ManifestKey)), []byte("41"), 0o644)
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
	if !strings.Contains(runErr.Diagnostic, "integrity") {
		t.Fatalf("diagnostic = %q, want integrity failure", runErr.Diagnostic)
	}
	if runErr.Result == nil || runErr.Result.Status != StatusFailed {
		t.Fatalf("result = %#v, want failed result", runErr.Result)
	}
}

func assertNoCredentialMaterial(t *testing.T, data []byte) {
	t.Helper()

	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode JSON credential-material assertion: %v", err)
	}
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, nested := range typed {
				normalized := strings.ToLower(key)
				for _, forbidden := range []string{"credential", "accesskey", "secret", "sessiontoken"} {
					if strings.Contains(normalized, forbidden) {
						t.Fatalf("serialized runner metadata contains credential-shaped field %q", key)
					}
				}
				visit(nested)
			}
		case []any:
			for _, nested := range typed {
				visit(nested)
			}
		}
	}
	visit(value)
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

	for _, hostile := range []string{"../escape", "../../etc", "a/b", ".", "..", "foo/../bar", strings.Repeat("a", 129)} {
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

func TestUnsafeRawPlanIdentityRejectedBeforeArtifactWrites(t *testing.T) {
	type mutation struct {
		field  string
		mutate func(*planpb.WorkflowPlan, string)
	}

	mutations := []mutation{
		{
			field:  "plan graph start node",
			mutate: func(plan *planpb.WorkflowPlan, value string) { plan.Graph.StartNode = &value },
		},
		{
			field:  "plan graph end node",
			mutate: func(plan *planpb.WorkflowPlan, value string) { plan.Graph.EndNode = &value },
		},
		{
			field:  "plan graph node id",
			mutate: func(plan *planpb.WorkflowPlan, value string) { plan.Graph.Nodes[1].Id = &value },
		},
		{
			field:  "plan graph edge from",
			mutate: func(plan *planpb.WorkflowPlan, value string) { plan.Graph.Edges[0].From = &value },
		},
		{
			field:  "plan graph edge to",
			mutate: func(plan *planpb.WorkflowPlan, value string) { plan.Graph.Edges[0].To = &value },
		},
		{
			field:  "plan graph node merge input",
			mutate: func(plan *planpb.WorkflowPlan, value string) { plan.Graph.Nodes[1].MergeInputs = []string{value} },
		},
	}

	for _, mutation := range mutations {
		for _, hostile := range []string{"nested/value", ".", "..", strings.Repeat("a", 129)} {
			t.Run(mutation.field+" rejects "+hostile, func(t *testing.T) {
				storeRoot := newStoreRoot(t)
				sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
				compiled, manifests := compileConsistentFixture(t, "linear-chain", sourceRoot)
				mutation.mutate(compiled.Plan, hostile)
				invoker := &functionalStepInvoker{storeRoot: storeRoot}

				_, err := Run(context.Background(), RunConfig{
					Plan:              compiled.Plan,
					DatastoreRoot:     storeRoot,
					ProjectID:         "acme/security-workflows",
					RunID:             "run-hostile-raw-plan",
					SourcePackageRoot: sourceRoot,
					SourceManifests:   manifests,
					StepInvoker:       invoker,
				}, []byte("20"))
				if err == nil {
					t.Fatalf("Run accepted unsafe %s %q", mutation.field, hostile)
				}
				var invalid *InvalidRunInputError
				if !errors.As(err, &invalid) {
					t.Fatalf("error = %T (%v), want *InvalidRunInputError", err, err)
				}
				if invalid.Field != mutation.field {
					t.Fatalf("error field = %q, want %q", invalid.Field, mutation.field)
				}
				if len(invoker.descriptors) != 0 {
					t.Fatalf("runner received %d descriptors after identifier rejection", len(invoker.descriptors))
				}
				assertNoRunSideEffects(t, storeRoot)
			})
		}
	}
}

func TestUnsafePlanNodeIDRejectedBeforeArtifactWrites(t *testing.T) {
	storeRoot := newStoreRoot(t)
	sourceRoot := filepath.Join(repoRootForTest(t), "internal", "orchestrator", "testdata", "linear-chain")
	compiled, manifests := compileConsistentFixture(t, "linear-chain", sourceRoot)

	for _, node := range compiled.Plan.GetGraph().GetNodes() {
		if node.GetId() == "double" {
			hostile := "nested/double"
			node.Id = &hostile
			break
		}
	}
	invoker := &functionalStepInvoker{storeRoot: storeRoot}

	_, err := Run(context.Background(), RunConfig{
		Plan:              compiled.Plan,
		DatastoreRoot:     storeRoot,
		ProjectID:         "acme/security-workflows",
		RunID:             "run-hostile-node-id",
		SourcePackageRoot: sourceRoot,
		SourceManifests:   manifests,
		StepInvoker:       invoker,
	}, []byte("20"))
	if err == nil {
		t.Fatal("Run accepted a plan graph node id with a path separator")
	}
	var invalid *InvalidRunInputError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %T (%v), want *InvalidRunInputError", err, err)
	}
	if invalid.Field != "plan graph node id" {
		t.Fatalf("error field = %q, want plan graph node id", invalid.Field)
	}
	if len(invoker.descriptors) != 0 {
		t.Fatalf("runner received %d descriptors after identifier rejection", len(invoker.descriptors))
	}
	assertNoRunSideEffects(t, storeRoot)
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
		manifestKey, err := datastore.ParseKey(descriptor.Output.ManifestKey)
		if err != nil {
			return nil, err
		}
		if _, err := artifact.PublishJSON(ctx, store, artifact.Destination{
			ManifestKey: manifestKey,
			Schema:      descriptor.Output.Schema,
		}, artifact.Producer{
			ProjectKey: descriptor.ProjectKey,
			PlanHash:   descriptor.PlanHash,
			RunID:      descriptor.RunID,
			NodeID:     descriptor.NodeID,
			Attempt:    descriptor.Attempt,
		}, output); err != nil {
			return nil, err
		}
		outcomes = append(outcomes, StepInvocationOutcome{
			NodeID:  descriptor.NodeID,
			Attempt: descriptor.Attempt,
			Status:  StatusSucceeded,
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

var (
	descriptorDigestRefPattern  = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
	descriptorDigestPathPattern = regexp.MustCompile(`sha256-[0-9a-f]{64}`)
)

func normalizeDescriptorJSON(t *testing.T, data []byte, runID string, storeRoot string) []byte {
	t.Helper()

	normalized := string(data)
	normalized = strings.ReplaceAll(normalized, runID, "run-linear-chain-0001")
	normalized = strings.ReplaceAll(normalized, storeRoot, "/tmp/massive-conformance-store")
	normalized = strings.ReplaceAll(normalized, SourceArchiveContentType, "application/vnd.massive.source-tar")
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
