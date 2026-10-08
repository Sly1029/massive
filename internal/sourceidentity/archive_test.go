package sourceidentity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
)

func TestVerifyArchiveRejectsUnsafeAndMismatchedSource(t *testing.T) {
	content := []byte("value = 1\n")
	expected, err := Digest([]File{{Path: "workflow.py", Hash: canonical.DigestBytes(content)}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		paths []string
		kind  byte
		hash  string
		valid bool
	}{
		{"regular", []string{"workflow.py"}, tar.TypeReg, expected, true},
		{"different-identity", []string{"workflow.py"}, tar.TypeReg, "sha256:" + strings.Repeat("0", 64), false},
		{"traversal", []string{"../workflow.py"}, tar.TypeReg, expected, false},
		{"absolute", []string{"/workflow.py"}, tar.TypeReg, expected, false},
		{"backslash", []string{`dir\workflow.py`}, tar.TypeReg, expected, false},
		{"duplicate", []string{"workflow.py", "workflow.py"}, tar.TypeReg, expected, false},
		{"symlink", []string{"workflow.py"}, tar.TypeSymlink, expected, false},
		{"hardlink", []string{"workflow.py"}, tar.TypeLink, expected, false},
		{"directory", []string{"workflow.py"}, tar.TypeDir, expected, false},
		{"empty", nil, tar.TypeReg, expected, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			for _, path := range test.paths {
				header := &tar.Header{Name: path, Typeflag: test.kind, Mode: 0o644}
				if test.kind == tar.TypeReg {
					header.Size = int64(len(content))
				}
				if err := writer.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
				if test.kind == tar.TypeReg {
					if _, err := writer.Write(content); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			err := VerifyArchive(archive.Bytes(), test.hash)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, verification error=%v", test.valid, err)
			}
			if test.valid {
				for name, changed := range map[string][]byte{
					"missing-end":      archive.Bytes()[:len(archive.Bytes())-1024],
					"one-end-block":    archive.Bytes()[:len(archive.Bytes())-512],
					"trailing-data":    append(append([]byte(nil), archive.Bytes()...), []byte("hidden")...),
					"concatenated-tar": append(append([]byte(nil), archive.Bytes()...), archive.Bytes()...),
				} {
					if err := VerifyArchive(changed, expected); err == nil {
						t.Errorf("%s archive was accepted", name)
					}
				}
			}
		})
	}
}

func TestVerifyArchiveBoundsDeclaredSizeBeforeReadingBody(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "huge.py", Typeflag: tar.TypeReg, Size: MaxBytes + 1}); err != nil {
		t.Fatal(err)
	}
	// Intentionally no body: validation must reject the declared size rather
	// than attempt to read it or report a truncated body.
	if err := VerifyArchive(archive.Bytes(), "unused"); err == nil || !strings.Contains(err.Error(), "limits") {
		t.Fatalf("oversized header verification: %v", err)
	}
}

func TestSharedSourceLimitArchives(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "fixtures", "source-limits")
	data, err := os.ReadFile(filepath.Join(root, "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Limits struct {
			Files int `json:"files"`
			Bytes int `json:"bytes"`
		} `json:"limits"`
		Cases []struct {
			Archive     string `json:"archive"`
			PackageHash string `json:"packageHash"`
			ArchiveHash string `json:"archiveHash"`
			Go          string `json:"go"`
			TypeScript  string `json:"typescript"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Limits.Files != MaxFiles || fixture.Limits.Bytes != MaxBytes {
		t.Fatalf("shared limits %+v differ from the Go verifier", fixture.Limits)
	}
	for _, vector := range fixture.Cases {
		t.Run(vector.Archive, func(t *testing.T) {
			compressed, err := os.Open(filepath.Join(root, vector.Archive))
			if err != nil {
				t.Fatal(err)
			}
			defer compressed.Close()
			reader, err := gzip.NewReader(compressed)
			if err != nil {
				t.Fatal(err)
			}
			archive, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if canonical.DigestBytes(archive) != vector.ArchiveHash || len(archive) > MaxArchiveBytes {
				t.Fatalf("fixture archive digest or size changed")
			}
			err = VerifyArchive(archive, vector.PackageHash)
			switch vector.Go {
			case "accept":
				if err != nil {
					t.Fatal(err)
				}
			case "reject-limits":
				if err == nil || !strings.Contains(err.Error(), "limits") {
					t.Fatalf("limit error = %v", err)
				}
			default:
				t.Fatalf("unknown expectation %q", vector.Go)
			}
			// Target compilation applies the TypeScript runner's smaller cap so
			// an oversized TypeScript package fails the build, not every pod.
			if vector.TypeScript != "reject-typescript-cap" {
				t.Fatalf("unknown TypeScript expectation %q", vector.TypeScript)
			}
			if vector.Go == "accept" {
				if err := VerifyLanguageArchive(archive, vector.PackageHash, "python"); err != nil {
					t.Fatal(err)
				}
				if err := VerifyLanguageArchive(archive, vector.PackageHash, "typescript"); err == nil || !strings.Contains(err.Error(), "Python runner") {
					t.Fatalf("TypeScript cap error = %v", err)
				}
			}
		})
	}
}

func TestVerifyArchiveBoundsEndPadding(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	body := []byte("value = 1\n")
	if err := writer.WriteHeader(&tar.Header{Name: "workflow.py", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	hash, err := Digest([]File{{Path: "workflow.py", Hash: canonical.DigestBytes(body)}})
	if err != nil {
		t.Fatal(err)
	}
	// One full tar record of padding is accepted; anything longer could make
	// an archive exceed MaxArchiveBytes, which runners use as a download bound.
	record := append(archive.Bytes()[:1024], make([]byte, MaxEndPadding)...)
	if err := VerifyArchive(record, hash); err != nil {
		t.Fatalf("record-padded archive: %v", err)
	}
	if err := VerifyArchive(append(record, make([]byte, 512)...), hash); err == nil || !strings.Contains(err.Error(), "end padding") {
		t.Fatalf("overpadded archive error = %v", err)
	}
}
