package orchestrator

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/sourceidentity"
)

func preflightPython(t *testing.T) string {
	t.Helper()
	python := filepath.Join(repoRootForTest(t), "packages", "python", ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(repoRootForTest(t), "packages", "python", ".venv", "Scripts", "python.exe")
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatal("install the Python SDK environment with uv sync --project packages/python")
	}
	return python
}

// publishedArchive writes a real source package with one body of the given
// size, publishes its archive at the object-store key, and returns its pins.
func publishedArchive(t *testing.T, storeRoot string, bodySize int) (packageHash string, archive SourceArchive) {
	t.Helper()
	source := t.TempDir()
	files := []SourcePackageFile{}
	for name, size := range map[string]int{"workflow.py": 0, "data.bin": bodySize} {
		body := []byte("value = 1\n")
		if size > 0 {
			body = make([]byte, size)
			if _, err := rand.Read(body); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(source, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, SourcePackageFile{Path: name, Hash: canonical.DigestBytes(body)})
	}
	identity := make([]sourceidentity.File, 0, len(files))
	for _, file := range files {
		identity = append(identity, sourceidentity.File(file))
	}
	sort.Slice(identity, func(i, j int) bool { return canonical.LessUTF16(identity[i].Path, identity[j].Path) })
	packageHash, err := sourceidentity.Digest(identity)
	if err != nil {
		t.Fatal(err)
	}
	body, err := BuildSourceArchive(source, files)
	if err != nil {
		t.Fatal(err)
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	archive = SourceArchive{Digest: canonical.DigestBytes(body)}
	if _, err := store.Put(context.Background(), datastore.MustKey(sourceArchiveKey(packageHash, archive.Digest)), body, datastore.PutOptions{ContentType: SourceArchiveContentType}); err != nil {
		t.Fatal(err)
	}
	return packageHash, archive
}

func preflightDescriptor(packageHash string) StepInvocationDescriptor {
	descriptor := StepInvocationDescriptor{NodeID: "step"}
	descriptor.SourcePackage.PackageHash = packageHash
	descriptor.Output.ManifestKey = "projects/p/runs/r/steps/step/1/output-manifest.json"
	return descriptor
}

// A legal package can approach MaxArchiveBytes. The pod streams it through
// scratch files, so the heap does not grow with the archive.
func TestPodPreflightStreamsLargeArchives(t *testing.T) {
	storeRoot := t.TempDir()
	const bodySize = 64 << 20
	packageHash, archive := publishedArchive(t, storeRoot, bodySize)
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	request := environment.Request{Python: preflightPython(t), ControlPlaneVersion: "0.0.0-dev"}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := isolatedPreflight(context.Background(), store, request, preflightDescriptor(packageHash), archive); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > bodySize/8 {
		t.Fatalf("preflight allocated %d bytes for a %d-byte archive", allocated, bodySize)
	}
}

// The declared size is checked before any byte is read: a wrong multi-GB
// object at the pinned key is refused without downloading it.
func TestPodPreflightRefusesOversizedArchivesBeforeReading(t *testing.T) {
	storeRoot := t.TempDir()
	packageHash, archive := publishedArchive(t, storeRoot, 1)
	key := sourceArchiveKey(packageHash, archive.Digest)
	// A sparse file reports the size without allocating its bytes.
	if err := os.Truncate(filepath.Join(storeRoot, filepath.FromSlash(key)), 4<<30); err != nil {
		t.Fatal(err)
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	request := environment.Request{Python: preflightPython(t), ControlPlaneVersion: "0.0.0-dev"}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = isolatedPreflight(context.Background(), store, request, preflightDescriptor(packageHash), archive)
	runtime.ReadMemStats(&after)
	var preflight *PreflightError
	if !errors.As(err, &preflight) || !strings.Contains(err.Error(), "above the") {
		t.Fatalf("oversized archive error = %v, want a non-retryable preflight error", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("refusing an oversized archive allocated %d bytes", allocated)
	}
}
