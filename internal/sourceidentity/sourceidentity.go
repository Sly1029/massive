package sourceidentity

import (
	"encoding/json"
	"fmt"

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
