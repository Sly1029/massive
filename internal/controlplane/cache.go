package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/irversion"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/runjournal"
	"github.com/Sly1029/massive/internal/sourceidentity"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/taskprocess"
)

//go:embed fingerprint.py
var fingerprintScript string

// Cache formats are local optimizations, separate from portable identities.
const localCacheVersion = 1
const pythonFrontendProtocol = "emit-output-v1"

type localFingerprint struct {
	Files       []sourceidentity.File `json:"files"`
	Environment string                `json:"environment"`
}

type cachedPreflight struct {
	Report *environment.Report `json:"report"`
	Record json.RawMessage     `json:"record"`
}

type cachedEmission struct {
	Spec     json.RawMessage `json:"spec"`
	Plan     json.RawMessage `json:"plan"`
	PlanHash string          `json:"planHash"`
}

type cacheEntry struct {
	Key   string          `json:"key"`
	Hash  string          `json:"hash"`
	Value json.RawMessage `json:"value"`
}

// readCache treats missing, partial, obsolete or corrupt bytes as a miss. The
// value's digest and input address are verified before contract parsing.
func readCache(ctx context.Context, store datastore.Datastore, key datastore.Key, value any) bool {
	object, err := store.Get(ctx, key)
	if err != nil {
		return false
	}
	var entry cacheEntry
	if json.Unmarshal(object.Body, &entry) != nil || entry.Key != key.String() || canonical.DigestBytes(entry.Value) != entry.Hash {
		return false
	}
	return json.Unmarshal(entry.Value, value) == nil
}

func writeCache(ctx context.Context, store datastore.Datastore, key datastore.Key, value any) error {
	body, err := canonical.Marshal(value)
	if err != nil {
		return err
	}
	entry, err := canonical.Marshal(cacheEntry{Key: key.String(), Hash: canonical.DigestBytes(body), Value: body})
	if err != nil {
		return err
	}
	// The local datastore installs complete bodies by atomic rename. Replacing
	// an entry also repairs corruption; concurrent writers publish valid bytes.
	_, err = store.Put(ctx, key, entry, datastore.PutOptions{ContentType: "application/json"})
	return err
}

var controlPlaneIdentity = sync.OnceValues(func() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
})

func fingerprint(ctx context.Context, python, root string) (*localFingerprint, error) {
	var output bytes.Buffer
	err := taskprocess.RunTo(ctx, []string{python, "-I", "-c", fingerprintScript, root}, root, nil, &output, io.Discard)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, nil
	} // unsupported interpreters take the full path
	var result *localFingerprint
	if json.Unmarshal(output.Bytes(), &result) != nil {
		return nil, nil
	}
	return result, nil
}

// EmitLocal reuses validated Python preparation only in a freshly fingerprinted
// environment. Other frontends and unsupported mutable environments take Emit's
// normal path. Inputs, runtime bindings and scheduling flags do not affect emission.
func EmitLocal(ctx context.Context, entry, storeRoot string, noCache bool) (*FrontendResult, error) {
	path, _, _ := strings.Cut(entry, "#")
	if noCache || filepath.Ext(path) != ".py" {
		result, err := Emit(ctx, entry, environment.Execution)
		if result != nil {
			result.Cache = &runjournal.Cache{}
		}
		return result, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(absolute)
	request, err := PythonEnvironment()
	if err != nil {
		return nil, err
	}
	request.ProjectRoot, request.Scope = root, environment.Execution
	before, err := fingerprint(ctx, request.Python, root)
	if err != nil {
		return nil, err
	}
	if before == nil {
		result, err := Emit(ctx, entry, environment.Execution)
		if result != nil {
			result.Cache = &runjournal.Cache{}
		}
		return result, err
	}
	sourceHash, err := sourceidentity.Digest(before.Files)
	if err != nil {
		return nil, err
	}
	storeRoot, err = resolveStore(storeRoot)
	if err != nil {
		return nil, err
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		return nil, err
	}
	binaryHash, err := controlPlaneIdentity()
	if err != nil {
		return nil, err
	}
	// Hash environment variables without persisting values. Besides import-time
	// inputs this binds uv's allowlisted resolution settings and config locations.
	variables := os.Environ()
	// Environ order is not semantic.
	slices.Sort(variables)
	// Process environment entries cannot contain NUL; preserve their exact
	// bytes, including values that are not UTF-8, in this local fingerprint.
	variablesHash := canonical.DigestBytes([]byte(strings.Join(variables, "\x00")))
	input, err := canonical.Marshal(map[string]any{
		"version": localCacheVersion, "request": request,
		"environment": before.Environment, "variables": variablesHash,
		"controlPlane": binaryHash,
	})
	if err != nil {
		return nil, err
	}
	preflightKey := cacheKey("preflight", canonical.DigestBytes(input))
	cache := &runjournal.Cache{}
	var report *environment.Report
	var saved cachedPreflight
	if readCache(ctx, store, preflightKey, &saved) && saved.Report != nil && saved.Report.Err() == nil {
		record, parseErr := environment.ParseRecord(saved.Record)
		if parseErr == nil && saved.Report.ProjectRoot == root {
			report = saved.Report
			report.Record = record
			cache.Preflight = true
		}
	}
	if report == nil {
		report, err = environment.Check(ctx, request)
		if err != nil {
			return nil, err
		}
		if err := report.Err(); err != nil {
			return nil, err
		}
	}
	emissionInput, err := canonical.Marshal(map[string]any{
		"preflight": preflightKey.String(), "source": sourceHash,
		"entry":            absolute + strings.TrimPrefix(entry, path),
		"realization":      report.Record.GetRealizationHash(),
		"frontendProtocol": pythonFrontendProtocol,
		"compiler":         plan.CompilerVersion, "ir": irversion.Current,
	})
	if err != nil {
		return nil, err
	}
	emissionKey := cacheKey("emission", canonical.DigestBytes(emissionInput))
	var result *FrontendResult
	var emission cachedEmission
	if readCache(ctx, store, emissionKey, &emission) {
		workflow, specErr := spec.Parse(emission.Spec)
		compiled, planErr := plan.VerifyCanonicalJSON(emission.Plan, emission.PlanHash)
		if specErr == nil && planErr == nil && compiled.GetSpecHash() == workflow.SpecHash && workflow.SourcePackages["python-main"].PackageHash == sourceHash {
			schedule, err := plan.BuildSchedule(workflow.Graph)
			if err != nil {
				return nil, err
			}
			result = &FrontendResult{Spec: workflow, Canonical: emission.Spec, PackageRoot: root, Environment: report,
				Compiled: &plan.CompileResult{Plan: compiled, CanonicalJSON: emission.Plan, PlanHash: emission.PlanHash, SpecHash: workflow.SpecHash, Schedule: schedule}}
			cache.Emission = true
		}
	}
	if result == nil {
		result, err = emit(ctx, entry, environment.Execution, report)
		if err != nil {
			return nil, err
		}
		result.Compiled, err = plan.Compile(result.Spec, result.Canonical)
		if err != nil {
			return nil, fmt.Errorf("compile workflow plan: %w", err)
		}
	}
	if cache.Preflight && cache.Emission {
		result.Cache = cache
		return result, nil
	}
	// Refuse publication (and retry the normal path) if source or environment
	// changed while probing/importing. A cache entry must describe one snapshot.
	after, err := fingerprint(ctx, request.Python, root)
	if err != nil {
		return nil, err
	}
	afterBytes, _ := json.Marshal(after)
	beforeBytes, _ := json.Marshal(before)
	if !bytes.Equal(afterBytes, beforeBytes) {
		result, err := Emit(ctx, entry, environment.Execution)
		if result != nil {
			result.Cache = &runjournal.Cache{}
		}
		return result, err
	}
	if !cache.Preflight {
		record, err := environment.MarshalRecord(report.Record)
		if err != nil {
			return nil, err
		}
		if err := writeCache(ctx, store, preflightKey, cachedPreflight{Report: report, Record: record}); err != nil {
			return nil, err
		}
	}
	if !cache.Emission {
		if err := writeCache(ctx, store, emissionKey, cachedEmission{Spec: result.Canonical, Plan: result.Compiled.CanonicalJSON, PlanHash: result.Compiled.PlanHash}); err != nil {
			return nil, err
		}
	}
	result.Cache = cache
	return result, nil
}

func cacheKey(kind, hash string) datastore.Key {
	return datastore.MustKey("cache/local-v1/" + kind + "/sha256-" + strings.TrimPrefix(hash, "sha256:") + ".json")
}
