// Package containerimage owns the immutable image-reference rule shared by
// frontend-spec validation and environment materialization.
package containerimage

import (
	"errors"
	"fmt"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/distribution/reference"
)

// ValidatePinned validates OCI/Docker reference syntax and requires a SHA-256
// digest. Parsing must not silently normalize the declaration used for identity.
// This performs no registry lookup and does not attest to image availability.
func ValidatePinned(image string) error {
	parsed, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("container image must be an immutable image digest reference: %w", err)
	}
	pinned, ok := parsed.(reference.Canonical)
	if !ok || !canonical.IsSHA256Ref(pinned.Digest().String()) {
		return errors.New("container image must be an immutable image digest reference using sha256")
	}
	return nil
}
