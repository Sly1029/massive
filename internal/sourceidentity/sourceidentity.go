package sourceidentity

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/Sly1029/massive/internal/canonical"
)

type File struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

type hashingSpec struct {
	Algorithm        string `json:"algorithm"`
	Canonicalization string `json:"canonicalization"`
	Recipe           string `json:"recipe"`
	RecipeVersion    uint32 `json:"recipeVersion"`
}

type hashInput struct {
	Files         []File      `json:"files"`
	Hashing       hashingSpec `json:"hashing"`
	Kind          string      `json:"kind"`
	SchemaVersion uint32      `json:"schemaVersion"`
}

// Digest returns the source-package-v1 identity over ordered normalized paths
// and their exact byte hashes.
func Digest(files []File) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("source package files must not be empty")
	}
	for index, file := range files {
		segments := strings.Split(file.Path, "/")
		if file.Path == "" || strings.Contains(file.Path, "\\") || strings.HasPrefix(file.Path, "/") || path.Clean(file.Path) != file.Path || hasInvalidSegment(segments) {
			return "", fmt.Errorf("source package file %d path %q is not a normalized relative path", index, file.Path)
		}
		if !validSHA256Ref(file.Hash) {
			return "", fmt.Errorf("source package file %d hash must be sha256:<64 lowercase hex>", index)
		}
		if index > 0 && !canonical.LessUTF16(files[index-1].Path, file.Path) {
			return "", fmt.Errorf("source package files must have unique paths in UTF-16 code-unit order")
		}
	}
	data, err := json.Marshal(hashInput{
		Files: files,
		Hashing: hashingSpec{
			Algorithm:        "sha256",
			Canonicalization: "canonical-json-v0",
			Recipe:           "source-package",
			RecipeVersion:    1,
		},
		Kind:          "SourcePackageHashInput",
		SchemaVersion: 0,
	})
	if err != nil {
		return "", fmt.Errorf("marshal source package hash input: %w", err)
	}
	return canonical.DigestJSON(data)
}

func hasInvalidSegment(segments []string) bool {
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func validSHA256Ref(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	hexValue := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(hexValue)
	return err == nil && len(decoded) == 32 && strings.ToLower(hexValue) == hexValue
}
