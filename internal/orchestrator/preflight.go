package orchestrator

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/sourceidentity"
)

// PreflightError reports that the executor's interpreter cannot run a node's
// archived project: findings, or project metadata the probe cannot read.
// Another attempt in the same image cannot succeed, so targets must not retry
// it. Cancellation and other infrastructure errors are never wrapped.
type PreflightError struct {
	NodeID string
	Err    error
}

func (e *PreflightError) Error() string {
	return fmt.Sprintf("isolated step %s cannot run in this image; rebuild it with the workflow's locked dependencies and Massive release: %v", e.NodeID, e.Err)
}

func (e *PreflightError) Unwrap() error { return e.Err }

// attemptEnvironmentKey sits beside the attempt's output manifest, so every
// attempt that passed preflight names the realization that ran it, even when
// its task then fails.
func attemptEnvironmentKey(outputManifestKey string) datastore.Key {
	return datastore.MustKey(strings.TrimSuffix(outputManifestKey, "output-manifest.json") + "environment.json")
}

// isolatedPreflight checks the executor's interpreter against the node's
// verified source archive before any author code runs and records the
// realization for this attempt. It returns the interpreter to pin.
func isolatedPreflight(ctx context.Context, store datastore.Datastore, request environment.Request, descriptor StepInvocationDescriptor, source SourceArchive) (string, error) {
	fail := func(err error) (string, error) { return "", &PreflightError{NodeID: descriptor.NodeID, Err: err} }
	packageHash := descriptor.SourcePackage.PackageHash
	// Embedded bodies were verified when installed. Under object-store-v0 the
	// pod checks the same published object the runner will import, so an
	// absent archive can never pass as an empty, undeclared project.
	archive := source.Body
	if archive == nil {
		object, err := store.Get(ctx, datastore.MustKey(sourceArchiveKey(packageHash, source.Digest)))
		// Like the runner, treat a denied read as missing: without
		// s3:ListBucket, S3 reports an unpublished key as AccessDenied.
		if errors.Is(err, datastore.ErrNotFound) || errors.Is(err, datastore.ErrAccessDenied) {
			return fail(fmt.Errorf("source archive %s for package %s is not readable from the datastore; publish the bundle with `massive publish`, or grant the pod read access to it", source.Digest, packageHash))
		}
		if err != nil {
			return "", fmt.Errorf("read source archive for preflight: %w", err)
		}
		if canonical.DigestBytes(object.Body) != source.Digest {
			return fail(fmt.Errorf("datastore source archive for package %s does not match its pinned digest %s", packageHash, source.Digest))
		}
		if err := sourceidentity.VerifyArchive(object.Body, packageHash); err != nil {
			return fail(err)
		}
		archive = object.Body
	}
	root, err := os.MkdirTemp("", "massive-preflight-*")
	if err != nil {
		return "", fmt.Errorf("create preflight directory: %w", err)
	}
	defer os.RemoveAll(root)
	if err := extractSourceArchive(archive, root); err != nil {
		return "", err
	}
	request.ProjectRoot = root
	report, err := environment.Check(ctx, request)
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A terminated or evicted pod must stay retryable.
		return "", ctxErr
	}
	var project *environment.ProjectError
	if errors.As(err, &project) {
		return fail(err)
	}
	if err != nil {
		return "", err
	}
	if err := report.Err(); err != nil {
		var findings *environment.PreflightError
		if errors.As(err, &findings) {
			// The extraction directory means nothing to an operator.
			findings.ProjectRoot = "source package " + descriptor.SourcePackage.PackageHash
		}
		return fail(err)
	}
	realized, err := environment.Store(ctx, store, report.Record)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(realized)
	if err != nil {
		return "", err
	}
	key := attemptEnvironmentKey(descriptor.Output.ManifestKey)
	if _, err := store.Put(ctx, key, body, datastore.PutOptions{ContentType: jsonContentType}); err != nil {
		return "", fmt.Errorf("record attempt environment: %w", err)
	}
	return report.Interpreter.Executable, nil
}

// extractSourceArchive writes an already verified source archive, which
// contains only regular files at normalized relative paths.
func extractSourceArchive(archive []byte, root string) error {
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || !safeArchivePath(header.Name) {
			return fmt.Errorf("source archive entry %q is not a regular file at a safe path", header.Name)
		}
		path := filepath.Join(root, filepath.FromSlash(header.Name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("read source archive entry %q: %w", header.Name, err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}
}
