package sourceidentity

import (
	"archive/tar"
	"bytes"
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
	if err := writer.WriteHeader(&tar.Header{Name: "huge.py", Typeflag: tar.TypeReg, Size: 51 * 1024 * 1024}); err != nil {
		t.Fatal(err)
	}
	// Intentionally no body: validation must reject the declared size rather
	// than attempt to read it or report a truncated body.
	if err := VerifyArchive(archive.Bytes(), "unused"); err == nil || !strings.Contains(err.Error(), "limits") {
		t.Fatalf("oversized header verification: %v", err)
	}
}
