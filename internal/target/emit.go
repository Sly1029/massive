package target

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Sly1029/massive/internal/canonical"
)

// WriteBundle writes the bundle to dir deterministically: every content artifact
// in path order, then bundle-manifest.json. Directories are created as needed.
// Content is written byte-for-byte (no trailing newline), so a recompiled bundle
// is byte-identical to the one on disk.
func WriteBundle(dir string, bundle *Bundle) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create bundle directory %q: %w", dir, err)
	}

	for _, artifact := range bundle.Artifacts {
		if err := writeBundleFile(dir, artifact.Path, artifact.Bytes); err != nil {
			return err
		}
	}
	if err := writeBundleFile(dir, BundleManifestPath, bundle.ManifestJSON); err != nil {
		return err
	}
	return nil
}

func writeBundleFile(dir, relPath string, data []byte) error {
	if err := validateBundlePath(relPath); err != nil {
		return err
	}
	fullPath := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("create bundle subdirectory for %q: %w", relPath, err)
	}
	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return fmt.Errorf("write bundle file %q: %w", relPath, err)
	}
	return nil
}

// validateBundlePath enforces the datastore-layout key syntax on bundle-relative
// paths: forward slashes only, no leading slash, no empty/./.. segments, no
// backslashes. This keeps emitted keys safe and portable to the datastore.
func validateBundlePath(relPath string) error {
	if relPath == "" {
		return fmt.Errorf("bundle path is empty")
	}
	if strings.HasPrefix(relPath, "/") {
		return fmt.Errorf("bundle path %q must be relative", relPath)
	}
	if strings.ContainsRune(relPath, '\\') {
		return fmt.Errorf("bundle path %q must use forward slashes", relPath)
	}
	for _, segment := range strings.Split(relPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("bundle path %q has an empty or relative segment", relPath)
		}
	}
	return nil
}

func rejectManifestCollision(artifacts []Artifact) error {
	seen := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		if err := validateBundlePath(artifact.Path); err != nil {
			return err
		}
		if artifact.Path == BundleManifestPath {
			return fmt.Errorf("backend emitted a %q artifact; the manifest is reserved and written by the target package", BundleManifestPath)
		}
		if seen[artifact.Path] {
			return fmt.Errorf("backend emitted duplicate bundle path %q", artifact.Path)
		}
		seen[artifact.Path] = true
	}
	return nil
}

func sortArtifacts(artifacts []Artifact) {
	sort.Slice(artifacts, func(i, j int) bool { return canonical.LessUTF16(artifacts[i].Path, artifacts[j].Path) })
}

func marshalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal JSON value: %w", err)
	}
	return data, nil
}

func strPtr(value string) *string    { return &value }
func boolPtr(value bool) *bool       { return &value }
func uint32Ptr(value uint32) *uint32 { return &value }
