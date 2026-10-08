package sourceidentity

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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
	_, _, err := walkArchive(bytes.NewReader(archive), int64(len(archive)), expectedHash, nil)
	return err
}

// ExtractArchive applies VerifyArchive's checks to size bytes read from input
// while writing each file beneath directory. Bodies stream through their hash,
// so memory stays bounded by tar blocks rather than by package size. On error
// directory may hold partial files; callers discard it.
func ExtractArchive(input io.Reader, size int64, expectedHash, directory string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("open extraction directory: %w", err)
	}
	defer root.Close()
	_, _, err = walkArchive(input, size, expectedHash, root)
	return err
}

// VerifyLanguageArchive is VerifyArchive plus the limits of the runner that
// will extract the archive, so a build fails instead of every pod.
func VerifyLanguageArchive(archive []byte, expectedHash, language string) error {
	files, size, err := walkArchive(bytes.NewReader(archive), int64(len(archive)), expectedHash, nil)
	if err != nil {
		return err
	}
	if language == "typescript" && (files > TypeScriptMaxFiles || size > TypeScriptMaxBytes) {
		return fmt.Errorf("TypeScript source package has %d files and %d bytes; the TypeScript runner accepts at most %d files and %d bytes, so reduce the package or use the Python runner", files, size, TypeScriptMaxFiles, TypeScriptMaxBytes)
	}
	return nil
}

// countingReader records how many archive bytes archive/tar has consumed; tar
// reads exactly the blocks it parses, so the count locates entry boundaries.
type countingReader struct {
	reader io.Reader
	read   int64
}

func (counter *countingReader) Read(buffer []byte) (int, error) {
	n, err := counter.reader.Read(buffer)
	counter.read += int64(n)
	return n, err
}

// walkArchive verifies an archive of exactly size bytes and, with a root,
// writes each file beneath it as the entry streams past.
func walkArchive(input io.Reader, size int64, expectedHash string, root *os.Root) (int, int64, error) {
	counter := &countingReader{reader: input}
	reader := tar.NewReader(counter)
	files := make([]File, 0)
	seen := map[string]bool{}
	totalSize := int64(0)
	dataEnd := int64(0)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// archive/tar accepts a missing terminator and stops before trailing
			// data. The portable runners require two zero blocks and no hidden
			// trailing content. dataEnd excludes the last file's block padding;
			// tar has already checked the end blocks it consumed.
			end := dataEnd + (512-dataEnd%512)%512
			if size-end < 1024 {
				return 0, 0, errors.New("source archive is missing its two zero end blocks")
			}
			if size-end > MaxEndPadding {
				return 0, 0, errors.New("source archive has more end padding than one tar record")
			}
			trailing, err := io.ReadAll(io.LimitReader(counter, MaxEndPadding+1))
			if err != nil {
				return 0, 0, fmt.Errorf("read source archive: %w", err)
			}
			if len(bytes.Trim(trailing, "\x00")) != 0 {
				return 0, 0, errors.New("source archive has trailing data after its files")
			}
			if counter.read != size {
				return 0, 0, fmt.Errorf("source archive is %d bytes, not the expected %d", counter.read, size)
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
		hash := sha256.New()
		if err := copyEntry(root, header.Name, reader, hash); err != nil {
			return 0, 0, err
		}
		dataEnd = counter.read
		files = append(files, File{Path: header.Name, Hash: "sha256:" + hex.EncodeToString(hash.Sum(nil))})
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

// copyEntry streams one body through its hash and, with a root, into a new
// file beneath it; the root confines every path.
func copyEntry(root *os.Root, name string, body io.Reader, hash io.Writer) error {
	if root == nil {
		if _, err := io.Copy(hash, body); err != nil {
			return fmt.Errorf("read source archive entry %q: %w", name, err)
		}
		return nil
	}
	if directory := path.Dir(name); directory != "." {
		if err := root.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("extract source archive entry %q: %w", name, err)
		}
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("extract source archive entry %q: %w", name, err)
	}
	_, err = io.Copy(io.MultiWriter(file, hash), body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("extract source archive entry %q: %w", name, err)
	}
	return nil
}
