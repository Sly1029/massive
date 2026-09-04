package containerimage

import (
	"strings"
	"testing"
)

func TestPinnedImageReferenceGrammar(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("1", 64)
	for _, name := range []string{"runner", "example.invalid/runner", "localhost:5000/team/runner", "runner:release"} {
		if err := ValidatePinned(name + digest); err != nil {
			t.Errorf("valid reference %q rejected: %v", name, err)
		}
	}
	for _, image := range []string{
		"runner:latest", "Uppercase" + digest, "../runner" + digest,
		"runner\v" + digest, "https://registry/runner" + digest,
		"runner@sha256:bad", "runner@sha512:" + strings.Repeat("1", 128),
	} {
		if err := ValidatePinned(image); err == nil {
			t.Errorf("invalid reference %q accepted", image)
		}
	}
}
