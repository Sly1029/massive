package controlplane

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/Sly1029/massive/conformance/schema/materializationpb"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/deployment"
	"github.com/Sly1029/massive/internal/materialization"
	"github.com/Sly1029/massive/internal/sourceidentity"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target/argo"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestPortableArgoCompilationSurvivesCheckoutRemoval(t *testing.T) {
	frontend := portableFrontend(t, "raise RuntimeError('compilation must not import this module')\n")
	inputs, err := PrepareArgo(frontend)
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	local, err := BundleArgo(ArgoBundleRequest{
		Frontend: frontend, OutputDirectory: output, ProfileName: "portable",
		ArtifactStoreBinding: "artifacts", Namespace: "workflows",
		ServiceAccountName: "runner", WorkflowTemplateName: "portable",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only uploaded bytes survive. Neither the receiving compiler nor target
	// lowering can accidentally read source or invoke the frontend.
	if err := os.RemoveAll(frontend.PackageRoot); err != nil {
		t.Fatal(err)
	}
	remote, err := CompileArgo(*inputs, portableProfile())
	if err != nil {
		t.Fatal(err)
	}
	if remote.Plan.PlanHash != local.PlanHash || remote.Bundle.Manifest.GetBundleHash() != local.BundleHash {
		t.Fatal("local and receiving-side compilation differ")
	}
	for _, file := range remote.Bundle.Files {
		body, err := os.ReadFile(filepath.Join(output, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, file.Bytes) {
			t.Fatalf("local and portable file %s differ", file.Path)
		}
	}
	manifest := materializationManifest(t, remote)
	if remote.Deployment.SchemaVersion != 1 || remote.Deployment.MaterializationHash != manifest.GetManifestHash() {
		t.Fatal("deployment did not bind the materialization manifest")
	}
	if manifest.GetEnvironments()[0].GetVerification() != pb.ContainerVerification_PINNED_REFERENCE_ONLY {
		t.Fatal("offline compilation claimed registry verification")
	}

	// The existing standalone target compiler can consume the newly emitted
	// artifacts without source files, Python, or a server-specific adapter.
	rebuilt := t.TempDir()
	command := exec.Command("go", "run", "../../cmd/massive-compiler", "bundle-argo",
		"--plan", filepath.Join(output, "massive-plan.json"),
		"--deployment", filepath.Join(output, "deployment-spec.json"),
		"--materialization", filepath.Join(output, "materialization-spec.json"),
		"--runtime-assets", filepath.Join(output, "runtime-assets"), "--out", rebuilt)
	if body, err := command.CombinedOutput(); err != nil {
		t.Fatalf("standalone compiler: %v\n%s", err, body)
	}
	body, err := os.ReadFile(filepath.Join(rebuilt, "bundle-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, remote.Bundle.ManifestJSON) {
		t.Fatal("standalone compiler changed the bundle")
	}
}

func TestPortableArgoRejectsInvalidMaterializationInputs(t *testing.T) {
	frontend := portableFrontend(t, "value = 1\n")
	inputs, err := PrepareArgo(frontend)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(*pb.MaterializationSpec)
	}{
		{"absent-version", func(s *pb.MaterializationSpec) { s.SchemaVersion = nil }},
		{"future-version", func(s *pb.MaterializationSpec) { s.SchemaVersion = proto.Uint32(1) }},
		{"wrong-hash", func(s *pb.MaterializationSpec) { s.SpecHash = proto.String("sha256:" + strings.Repeat("0", 64)) }},
		{"missing-environment", func(s *pb.MaterializationSpec) { s.Environments = nil }},
		{"duplicate-environment", func(s *pb.MaterializationSpec) { s.Environments = append(s.Environments, s.Environments[0]) }},
		{"absent-mode", func(s *pb.MaterializationSpec) { s.Environments[0].Mode = nil }},
		{"image-override", func(s *pb.MaterializationSpec) {
			s.Environments[0].GetExistingContainer().Image = proto.String("runner:latest")
		}},
		{"platform-override", func(s *pb.MaterializationSpec) {
			s.Environments[0].GetExistingContainer().Platform = proto.String("linux/arm64")
		}},
		{"missing-source", func(s *pb.MaterializationSpec) { s.SourceArchives = nil }},
		{"wrong-archive-digest", func(s *pb.MaterializationSpec) {
			s.SourceArchives[0].ArchiveHash = proto.String("sha256:" + strings.Repeat("0", 64))
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var value pb.MaterializationSpec
			if err := protojson.Unmarshal(inputs.MaterializationSpec, &value); err != nil {
				t.Fatal(err)
			}
			test.edit(&value)
			if test.name != "wrong-hash" {
				value.SpecHash = nil
				unhashed, err := materialization.MarshalCanonical(&value)
				if err != nil {
					t.Fatal(err)
				}
				value.SpecHash = proto.String(canonical.DigestBytes(unhashed))
			}
			body, err := materialization.MarshalCanonical(&value)
			if err != nil {
				t.Fatal(err)
			}
			changed := *inputs
			changed.MaterializationSpec = body
			if _, err := CompileArgo(changed, portableProfile()); err == nil || !strings.Contains(err.Error(), "materialization") {
				t.Fatalf("invalid input accepted: %v", err)
			}
		})
	}
	for _, mutation := range []string{"missing", "extra", "corrupt", "same-source-different-archive"} {
		t.Run(mutation, func(t *testing.T) {
			changed := *inputs
			changed.SourceArchives = map[string][]byte{}
			for hash, archive := range inputs.SourceArchives {
				switch mutation {
				case "missing":
				case "corrupt":
					changed.SourceArchives[hash] = []byte("not a source archive")
				case "same-source-different-archive":
					changed.SourceArchives[hash] = append(append([]byte(nil), archive...), make([]byte, 512)...)
				default:
					changed.SourceArchives[hash] = archive
					changed.SourceArchives["sha256:"+strings.Repeat("0", 64)] = archive
				}
			}
			if _, err := CompileArgo(changed, portableProfile()); err == nil {
				t.Fatal("invalid archive input accepted")
			}
		})
	}
	t.Run("unknown-field", func(t *testing.T) {
		changed := *inputs
		changed.MaterializationSpec = bytes.Replace(inputs.MaterializationSpec, []byte("{"), []byte(`{"buildCommand":"untrusted",`), 1)
		if _, err := CompileArgo(changed, portableProfile()); err == nil {
			t.Fatal("unknown materializer field accepted")
		}
	})
}

func TestMaterializationIdentitySeparatesSourceEnvironmentAndDeployment(t *testing.T) {
	firstInputs, err := PrepareArgo(portableFrontend(t, "value = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := CompileArgo(*firstInputs, portableProfile())
	if err != nil {
		t.Fatal(err)
	}
	profile := portableProfile()
	profile.Target.Namespace = "production"
	profile.Target.ServiceAccountName = "production-runner"
	deployed, err := CompileArgo(*firstInputs, profile)
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.PlanHash != deployed.Plan.PlanHash || first.Deployment.MaterializationHash != deployed.Deployment.MaterializationHash || first.Deployment.DeploymentHash == deployed.Deployment.DeploymentHash {
		t.Fatal("deployment policy leaked into plan or materialization identity")
	}
	secondInputs, err := PrepareArgo(portableFrontend(t, "value = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileArgo(*secondInputs, portableProfile())
	if err != nil {
		t.Fatal(err)
	}
	firstManifest, secondManifest := materializationManifest(t, first), materializationManifest(t, second)
	if firstManifest.GetEnvironments()[0].GetRealizationHash() != secondManifest.GetEnvironments()[0].GetRealizationHash() {
		t.Fatal("source-only edit invalidated the shared environment identity")
	}
	if first.Plan.PlanHash == second.Plan.PlanHash || firstManifest.GetManifestHash() == secondManifest.GetManifestHash() || first.Deployment.DeploymentHash == second.Deployment.DeploymentHash {
		t.Fatal("source edit was not bound into plan/materialization/deployment identity")
	}
	// A deployment cannot omit materialization inputs.
	if _, err := argo.Compile(first.Plan.CanonicalJSON, first.Deployment, argo.RuntimeAssets{SourceArchives: firstInputs.SourceArchives}); err == nil {
		t.Fatal("v1 deployment accepted missing materialization spec")
	}
	wrongBinding, _, err := deployment.New(first.Plan.PlanHash, portableProfile(), second.Deployment.MaterializationHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := argo.Compile(first.Plan.CanonicalJSON, wrongBinding, argo.RuntimeAssets{
		SourceArchives: firstInputs.SourceArchives, MaterializationSpec: firstInputs.MaterializationSpec,
	}); err == nil || !strings.Contains(err.Error(), "does not match deployment") {
		t.Fatalf("cross-materialization binding accepted: %v", err)
	}
}

func TestMaterializationRejectsIncapableAndAmbiguousEnvironments(t *testing.T) {
	inputs, err := PrepareArgo(portableFrontend(t, "value = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileArgo(*inputs, portableProfile())
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"local", "mutable-image", "missing-platform", "conflicting-alias"} {
		t.Run(mutation, func(t *testing.T) {
			value := proto.Clone(compiled.Plan.Plan).(*planpb.WorkflowPlan)
			environment := value.Environments[0]
			switch mutation {
			case "local":
				environment.Runtime = &planpb.EnvironmentRequirement_Node{Node: &planpb.NodeRequirement{
					Version: proto.String("22"), PackageManager: proto.String("npm"), Lockfile: proto.String("package-lock.json"),
				}}
			case "mutable-image":
				environment.GetContainer().Image = proto.String("runner:latest")
			case "missing-platform":
				environment.GetContainer().Platform = nil
			case "conflicting-alias":
				duplicate := proto.Clone(environment).(*planpb.EnvironmentRequirement)
				duplicate.GetContainer().Command = []string{"different-command"}
				value.Environments = append(value.Environments, duplicate)
			}
			if _, err := materialization.ForPlan(value, inputs.SourceArchives); err == nil {
				t.Fatal("unsupported or conflicting environment accepted")
			}
		})
	}
	value := proto.Clone(compiled.Plan.Plan).(*planpb.WorkflowPlan)
	value.Environments = append(value.Environments, proto.Clone(value.Environments[0]).(*planpb.EnvironmentRequirement))
	if _, err := materialization.ForPlan(value, inputs.SourceArchives); err == nil {
		t.Fatal("duplicate compiled environment requirements accepted")
	}
}

func materializationManifest(t *testing.T, compiled *ArgoCompilation) *pb.MaterializationManifest {
	t.Helper()
	for _, file := range compiled.Bundle.Files {
		if file.Path == "materialization-manifest.json" {
			var manifest pb.MaterializationManifest
			if err := protojson.Unmarshal(file.Bytes, &manifest); err != nil {
				t.Fatal(err)
			}
			hash, err := canonical.DigestJSONWithRootMemberExcluded(file.Bytes, "manifestHash")
			if err != nil || hash != manifest.GetManifestHash() {
				t.Fatalf("manifest self-hash is invalid: %v", err)
			}
			// Both protobuf encodings carry the same explicit contract.
			wire, err := proto.Marshal(&manifest)
			if err != nil {
				t.Fatal(err)
			}
			var decoded pb.MaterializationManifest
			if err := proto.Unmarshal(wire, &decoded); err != nil || !proto.Equal(&manifest, &decoded) {
				t.Fatalf("manifest protobuf round trip failed: %v", err)
			}
			return &manifest
		}
	}
	t.Fatal("bundle is missing materialization manifest")
	return nil
}

func portableProfile() deployment.Profile {
	return deployment.Profile{
		Name: "portable", ArtifactStoreBinding: "artifacts",
		Target: deployment.Target{Kind: "argo", Namespace: "workflows", ServiceAccountName: "runner", WorkflowTemplateName: "portable"},
	}
}

// A real packaged filesystem fixture with a conformance graph. Its source is
// deliberately not evaluated: this suite exercises the data-only compiler.
func portableFrontend(t *testing.T, source string) *FrontendResult {
	t.Helper()
	body, err := os.ReadFile("../../conformance/fixtures/specs/python-linear/workflow-spec.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := spec.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for id, pkg := range value.SourcePackages {
		files := make([]sourceidentity.File, 0, len(pkg.Files))
		for index := range pkg.Files {
			file := &pkg.Files[index]
			file.Hash = canonical.DigestBytes([]byte(source))
			path := filepath.Join(root, file.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			files = append(files, sourceidentity.File{Path: file.Path, Hash: file.Hash})
		}
		pkg.PackageHash, err = sourceidentity.Digest(files)
		if err != nil {
			t.Fatal(err)
		}
		value.SourcePackages[id] = pkg
	}
	body, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	value.SpecHash, err = spec.RecomputedSpecHash(body)
	if err != nil {
		t.Fatal(err)
	}
	body, err = canonical.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	value, err = spec.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	return &FrontendResult{Spec: value, Canonical: body, PackageRoot: root}
}
