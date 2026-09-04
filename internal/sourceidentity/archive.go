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

// VerifyArchive derives source-package-v1 identity from exact tar entry bytes.
// Both target compilation and remote execution use this same trust boundary.
func VerifyArchive(archive []byte, expectedHash string) error {
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
				return errors.New("source archive is missing its two zero end blocks")
			}
			if len(bytes.Trim(archive[end:], "\x00")) != 0 {
				return errors.New("source archive has trailing data after its files")
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		if header.Format != tar.FormatUSTAR || header.Typeflag != tar.TypeReg || seen[header.Name] {
			return fmt.Errorf("source archive contains invalid entry %q", header.Name)
		}
		seen[header.Name] = true
		if len(files) >= 1024 || header.Size > 50*1024*1024-totalSize {
			return errors.New("source archive exceeds source package limits")
		}
		totalSize += header.Size
		body, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("read source archive entry %q: %w", header.Name, err)
		}
		dataEnd = len(archive) - input.Len()
		files = append(files, File{Path: header.Name, Hash: canonical.DigestBytes(body)})
	}
	sort.Slice(files, func(i, j int) bool { return canonical.LessUTF16(files[i].Path, files[j].Path) })
	// Digest validates normalized, unique paths before computing identity.
	actual, err := Digest(files)
	if err != nil {
		return fmt.Errorf("derive source archive identity: %w", err)
	}
	if actual != expectedHash {
		return fmt.Errorf("source archive identity %s does not match plan package hash %s", actual, expectedHash)
	}
	return nil
}
