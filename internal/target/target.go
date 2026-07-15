// Package target defines the backend-neutral contract for lowering a compiled
// WorkflowPlan into a deployable bundle.
//
// A Backend consumes a WorkflowPlan (never a WorkflowSpec) plus the requested
// target config and emits an ordered set of content artifacts and invariant
// results. This package assembles the canonical bundle manifest and writes the
// bundle to disk deterministically, so every backend gets stable emission,
// content hashing, and a manifest for free. Nothing Argo- or Kubernetes-specific
// lives here.
//
// The `local` target is orchestrator-driven: it executes a plan directly against
// the datastore (see internal/orchestrator) and emits no deploy bundle, so it is
// intentionally not a Backend in this registry.
//
// Adding a backend (Cloudflare Workers, Temporal, Vercel, ...): implement
// Backend against the plan, route emitted artifacts through BuildBundle, and let
// WriteBundle handle disk emission. See docs/spec/target-backends.md for the
// full contract.
package target

import (
	"encoding/json"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/plan"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Backend lowers a compiled plan into a deployable bundle for one target kind.
type Backend interface {
	// Kind is the target id this backend compiles (for example "argo"). It must
	// match the WorkflowSpec target request kind.
	Kind() string
	// Compile materializes a bundle from the plan. Backends build their content
	// artifacts and invariant results, then return target.BuildBundle(...) so the
	// manifest and bundle hash are assembled identically everywhere. A compile
	// that violates a hard invariant returns an error carrying the diagnostic.
	Compile(CompileInput) (*Bundle, error)
}

// CompileInput is everything a backend needs to compile one target. It is
// plan-driven and backend-neutral by design: the typed plan plus its canonical
// bytes and hash, and the resolved target request as a kind plus opaque
// canonical config bytes. Backends must not reach back into the WorkflowSpec,
// and this package never interprets TargetConfig — each backend decodes and
// validates its own.
type CompileInput struct {
	// Plan is the typed compiled plan. PlanJSON is its canonical JSON body (the
	// exact bytes written to the datastore) and PlanHash is its self-excluded
	// digest; both are threaded so a backend never re-marshals or re-hashes.
	Plan     *planpb.WorkflowPlan
	PlanJSON []byte
	PlanHash string
	// TargetKind selects the backend. TargetConfig is the target request's
	// canonical JSON config (every member except kind), opaque to this package
	// and decoded by the backend. The bundle hash covers it wholesale.
	TargetKind   string
	TargetConfig []byte
}

// VerifyPlanConsistency rejects a compile input whose plan bytes, hash, and
// typed plan disagree — the neutral guard the registry runs before any backend
// sees the input, so a bundle can never describe one plan in its YAML while
// massive-plan.json and its digest describe another. It reuses the compiler's
// own plan-hash rule (plan.VerifyCanonicalJSON): PlanJSON must already be
// canonical, its self-excluded digest must equal PlanHash, and the embedded
// planHash must match.
func VerifyPlanConsistency(input CompileInput) error {
	if input.Plan == nil {
		return fmt.Errorf("compile input has no plan")
	}
	parsed, err := plan.VerifyCanonicalJSON(input.PlanJSON, input.PlanHash)
	if err != nil {
		return fmt.Errorf("compile input plan is inconsistent: %w", err)
	}
	// The typed Plan is what a backend materializes its YAML from, while PlanJSON
	// is emitted verbatim as massive-plan.json. Compare them so a mutated typed
	// plan cannot ship a bundle whose YAML and datastore plan disagree.
	if !proto.Equal(input.Plan, parsed) {
		return fmt.Errorf("compile input typed plan does not match PlanJSON")
	}
	return nil
}

// Artifact is one emitted content file, keyed by a bundle-relative path.
type Artifact struct {
	Path        string
	Bytes       []byte
	ContentType string
	// Role is a short, backend-defined label recorded in the manifest (for
	// example "workflow-template" or "plan").
	Role string
}

// Validation is one invariant result recorded in the bundle manifest.
type Validation struct {
	Name       string
	Passed     bool
	Diagnostic string
}

// Bundle is a compiled, ready-to-write deploy bundle: the content artifacts plus
// the canonical bundle manifest describing them.
type Bundle struct {
	Artifacts    []Artifact
	Manifest     *planpb.TargetBundleManifest
	ManifestJSON []byte
}

// BundleManifestPath is the canonical manifest file name inside every bundle.
const BundleManifestPath = "bundle-manifest.json"

// BuildBundle assembles the canonical bundle manifest from the emitted content
// artifacts and returns a Bundle ready to write. Content artifacts are recorded
// in path order with their sha256; the manifest itself is not one of them. The
// bundle hash covers the plan hash, target request, compiler identity, and the
// ordered (path, hash) list of content artifacts, so identical inputs yield a
// byte-identical bundle.
func BuildBundle(input CompileInput, artifacts []Artifact, validations []Validation) (*Bundle, error) {
	ordered := append([]Artifact(nil), artifacts...)
	sortArtifacts(ordered)
	if err := rejectManifestCollision(ordered); err != nil {
		return nil, err
	}

	files := make([]*planpb.EmittedFile, 0, len(ordered))
	for _, artifact := range ordered {
		files = append(files, &planpb.EmittedFile{
			Path: strPtr(artifact.Path),
			Artifact: &planpb.ArtifactRef{
				Key:         strPtr(artifact.Path),
				Hash:        strPtr(canonical.DigestBytes(artifact.Bytes)),
				ContentType: strPtr(artifact.ContentType),
			},
			Role: strPtr(artifact.Role),
		})
	}

	provenance := &planpb.BundleProvenance{
		CompilerName:    strPtr(input.Plan.GetProvenance().GetCompilerName()),
		CompilerVersion: strPtr(input.Plan.GetProvenance().GetCompilerVersion()),
	}

	bundleHash, err := computeBundleHash(input, files, provenance)
	if err != nil {
		return nil, err
	}

	manifest := &planpb.TargetBundleManifest{
		SchemaVersion: uint32Ptr(0),
		Target:        strPtr(input.TargetKind),
		PlanHash:      strPtr(input.PlanHash),
		BundleHash:    strPtr(bundleHash),
		Files:         files,
		Validations:   toValidationResults(validations),
		Provenance:    provenance,
	}

	manifestJSON, err := marshalCanonicalProto(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal bundle manifest: %w", err)
	}

	return &Bundle{Artifacts: ordered, Manifest: manifest, ManifestJSON: manifestJSON}, nil
}

// computeBundleHash hashes the deterministic identity of the bundle. It excludes
// per-run validation outcomes (which are diagnostics, not identity) and the
// manifest's own bundle hash, and it is stable because the file list is sorted.
func computeBundleHash(input CompileInput, files []*planpb.EmittedFile, provenance *planpb.BundleProvenance) (string, error) {
	fileRefs := make([]map[string]string, 0, len(files))
	for _, file := range files {
		fileRefs = append(fileRefs, map[string]string{
			"path": file.GetPath(),
			"hash": file.GetArtifact().GetHash(),
		})
	}
	// Hash the target's canonical config wholesale rather than enumerating
	// backend-specific fields, so a future backend's config is automatically
	// covered without editing this neutral package.
	var configValue any
	if len(input.TargetConfig) > 0 {
		if err := json.Unmarshal(input.TargetConfig, &configValue); err != nil {
			return "", fmt.Errorf("decode target config for bundle hash: %w", err)
		}
	}
	identity := map[string]any{
		"planHash": input.PlanHash,
		"target": map[string]any{
			"kind":   input.TargetKind,
			"config": configValue,
		},
		"compilerName":    provenance.GetCompilerName(),
		"compilerVersion": provenance.GetCompilerVersion(),
		"files":           fileRefs,
	}
	return hashJSONValue(identity)
}

func toValidationResults(validations []Validation) []*planpb.ValidationResult {
	if len(validations) == 0 {
		return nil
	}
	results := make([]*planpb.ValidationResult, 0, len(validations))
	for _, validation := range validations {
		result := &planpb.ValidationResult{
			Name:   strPtr(validation.Name),
			Passed: boolPtr(validation.Passed),
		}
		if validation.Diagnostic != "" {
			result.Diagnostic = strPtr(validation.Diagnostic)
		}
		results = append(results, result)
	}
	return results
}

func marshalCanonicalProto(message proto.Message) ([]byte, error) {
	protoJSON, err := protojson.Marshal(message)
	if err != nil {
		return nil, err
	}
	return canonical.CanonicalizeJSON(protoJSON)
}

func hashJSONValue(value any) (string, error) {
	data, err := marshalJSON(value)
	if err != nil {
		return "", err
	}
	return canonical.DigestJSON(data)
}
