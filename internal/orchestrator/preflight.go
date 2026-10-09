package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	scratch, err := os.MkdirTemp("", "massive-preflight-*")
	if err != nil {
		return "", fmt.Errorf("create preflight directory: %w", err)
	}
	defer os.RemoveAll(scratch)
	root := filepath.Join(scratch, "source")
	if err := os.Mkdir(root, 0o755); err != nil {
		return "", fmt.Errorf("create preflight directory: %w", err)
	}
	// Embedded bodies are small and were verified when installed. Under
	// object-store-v0 the pod checks the same published object the runner will
	// import, so an absent archive can never pass as an empty project.
	archive, size := io.Reader(bytes.NewReader(source.Body)), int64(len(source.Body))
	if source.Body == nil {
		file, written, refusal, err := fetchSourceArchive(ctx, store, packageHash, source.Digest, filepath.Join(scratch, "source.tar"))
		if refusal != nil {
			return fail(refusal)
		}
		if err != nil {
			return "", err
		}
		defer file.Close()
		archive, size = file, written
	}
	if err := sourceidentity.ExtractArchive(archive, size, packageHash, root); err != nil {
		return fail(err)
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

// fetchSourceArchive streams a pinned object-store archive into a scratch
// file, as the Python runner does. It bounds the read by the largest valid
// source archive and checks the digest before any entry is read. A refusal
// (missing, denied, oversized, or mismatched) cannot succeed on retry; other
// errors can.
func fetchSourceArchive(ctx context.Context, store datastore.Datastore, packageHash, digest, path string) (file *os.File, size int64, refusal, err error) {
	reader, info, err := store.Open(ctx, datastore.MustKey(sourceArchiveKey(packageHash, digest)))
	// Like the runner, treat a denied read as missing: without s3:ListBucket,
	// S3 reports an unpublished key as AccessDenied.
	if errors.Is(err, datastore.ErrNotFound) || errors.Is(err, datastore.ErrAccessDenied) {
		return nil, 0, fmt.Errorf("source archive %s for package %s is not readable from the datastore (%v); publish the bundle with `massive publish`, or grant the pod read access to it", digest, packageHash, err), nil
	}
	if err != nil {
		return nil, 0, nil, fmt.Errorf("open source archive for preflight: %w", err)
	}
	defer reader.Close()
	tooLarge := func(bytes int64) error {
		return fmt.Errorf("datastore source archive for package %s is %d bytes, above the %d-byte limit of any source package", packageHash, bytes, int64(sourceidentity.MaxArchiveBytes))
	}
	if info.Size > sourceidentity.MaxArchiveBytes {
		return nil, 0, tooLarge(info.Size), nil
	}
	file, err = os.Create(path)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("create source archive scratch file: %w", err)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, sourceidentity.MaxArchiveBytes+1))
	switch {
	case err != nil:
		err = fmt.Errorf("read source archive for preflight: %w", err)
	case written > sourceidentity.MaxArchiveBytes:
		refusal = tooLarge(written)
	case written != info.Size:
		err = fmt.Errorf("read %d of the %d bytes of source archive %s", written, info.Size, digest)
	case "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest:
		refusal = fmt.Errorf("datastore source archive for package %s does not match its pinned digest %s", packageHash, digest)
	}
	if err == nil && refusal == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil || refusal != nil {
		_ = file.Close()
		return nil, 0, refusal, err
	}
	return file, written, nil, nil
}
