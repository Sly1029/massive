package argo

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/internal/target"
)

var updateGolden = flag.Bool("update", false, "rewrite golden bundle fixtures under conformance/fixtures/bundles")

// goldenBundleCases are keyed by graph-catalog case ids. Both compile against the
// same argo target so the goldens exercise a linear chain and a diamond fan-in.
var goldenBundleCases = []string{"linear-chain", "diamond"}

func TestGoldenBundlesMatch(t *testing.T) {
	for _, caseName := range goldenBundleCases {
		t.Run(caseName, func(t *testing.T) {
			bundle := compileFixtureBundle(t, caseName, argoTarget)
			dir := goldenBundleDir(caseName)

			files := bundleFiles(bundle)
			if *updateGolden {
				writeGoldenBundle(t, dir, files)
				return
			}

			for name, want := range files {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("read golden %s (run `go test ./internal/target/argo -update`): %v", name, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("golden %s/%s mismatch\n--- got ---\n%s\n--- want ---\n%s", caseName, name, want, got)
				}
			}
		})
	}
}

// TestGoldenBundlesAreByteStable proves determinism: compiling twice yields
// byte-identical artifacts and manifest.
func TestGoldenBundlesAreByteStable(t *testing.T) {
	for _, caseName := range goldenBundleCases {
		t.Run(caseName, func(t *testing.T) {
			first := bundleFiles(compileFixtureBundle(t, caseName, argoTarget))
			second := bundleFiles(compileFixtureBundle(t, caseName, argoTarget))
			if len(first) != len(second) {
				t.Fatalf("artifact count differs across compiles: %d vs %d", len(first), len(second))
			}
			for name, firstBytes := range first {
				if !bytes.Equal(firstBytes, second[name]) {
					t.Fatalf("%s is not byte-stable across compiles", name)
				}
			}
		})
	}
}

func bundleFiles(bundle *target.Bundle) map[string][]byte {
	files := make(map[string][]byte, len(bundle.Artifacts)+1)
	for _, artifact := range bundle.Artifacts {
		files[artifact.Path] = artifact.Bytes
	}
	files[target.BundleManifestPath] = bundle.ManifestJSON
	return files
}

func goldenBundleDir(caseName string) string {
	return filepath.Join("..", "..", "..", "conformance", "fixtures", "bundles", caseName, "argo")
}

func writeGoldenBundle(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
