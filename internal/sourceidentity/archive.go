package sourceidentity

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/Sly1029/massive/internal/canonical"
)

// Source packages hold application resources as well as modules. These bounds
// are shared by every runner that extracts an archive.
const (
	MaxFiles = 16384
	MaxBytes = 256 * 1024 * 1024
	// MaxEndPadding admits writers that pad the two end blocks to one
	// 10,240-byte tar record, as Python's tarfile does.
	MaxEndPadding = 10240
	// MaxArchiveBytes bounds every archive VerifyArchive accepts: each file
	// adds a header block and at most one block of padding to its body.
	MaxArchiveBytes = MaxFiles*1024 + MaxBytes + MaxEndPadding
)

// The TypeScript runner buffers whole archives in memory, so it accepts only
// smaller packages; target compilation rejects larger TypeScript packages.
const (
	TypeScriptMaxFiles = 1024
	TypeScriptMaxBytes = 50 * 1024 * 1024
)

// VerifyArchive derives source-package-v1 identity from exact tar entry bytes.
// Both target compilation and remote execution use this same trust boundary.
func VerifyArchive(archive []byte, expectedHash string) error {
	_, _, err := verifyArchive(archive, expectedHash)
	return err
}

// VerifyLanguageArchive is VerifyArchive plus the limits of the runner that
// will extract the archive, so a build fails instead of every pod.
func VerifyLanguageArchive(archive []byte, expectedHash, language string) error {
	files, size, err := verifyArchive(archive, expectedHash)
	if err != nil {
		return err
	}
	if language == "typescript" && (files > TypeScriptMaxFiles || size > TypeScriptMaxBytes) {
		return fmt.Errorf("TypeScript source package has %d files and %d bytes; the TypeScript runner accepts at most %d files and %d bytes, so reduce the package or use the Python runner", files, size, TypeScriptMaxFiles, TypeScriptMaxBytes)
	}
	return nil
}

func verifyArchive(archive []byte, expectedHash string) (int, int64, error) {
	input := bytes.NewReader(archive)
	reader := tar.NewReader(input)
	files := make([]File, 0)
	seen := map[string]bool{}
	totalSize := int64(0)
	dataEnd := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// archive/tar accepts a missing terminator and stops before trailing
			// data. The portable runners require two zero blocks and no hidden
			// trailing content. dataEnd excludes the last file's block padding.
			end := dataEnd + (512-dataEnd%512)%512
			if len(archive)-end < 1024 {
				return 0, 0, errors.New("source archive is missing its two zero end blocks")
			}
			if len(archive)-end > MaxEndPadding {
				return 0, 0, errors.New("source archive has more end padding than one tar record")
			}
			if len(bytes.Trim(archive[end:], "\x00")) != 0 {
				return 0, 0, errors.New("source archive has trailing data after its files")
			}
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("read source archive: %w", err)
		}
		if header.Format != tar.FormatUSTAR || header.Typeflag != tar.TypeReg || seen[header.Name] {
			return 0, 0, fmt.Errorf("source archive contains invalid entry %q", header.Name)
		}
		seen[header.Name] = true
		if len(files) >= MaxFiles || header.Size > MaxBytes-totalSize {
			return 0, 0, errors.New("source archive exceeds source package limits")
		}
		totalSize += header.Size
		body, err := io.ReadAll(reader)
		if err != nil {
			return 0, 0, fmt.Errorf("read source archive entry %q: %w", header.Name, err)
		}
		dataEnd = len(archive) - input.Len()
		files = append(files, File{Path: header.Name, Hash: canonical.DigestBytes(body)})
	}
	sort.Slice(files, func(i, j int) bool { return canonical.LessUTF16(files[i].Path, files[j].Path) })
	// Digest validates normalized, unique paths before computing identity.
	actual, err := Digest(files)
	if err != nil {
		return 0, 0, fmt.Errorf("derive source archive identity: %w", err)
	}
	if actual != expectedHash {
		return 0, 0, fmt.Errorf("source archive identity %s does not match plan package hash %s", actual, expectedHash)
	}
	return len(files), totalSize, nil
}
