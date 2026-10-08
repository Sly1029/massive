//go:build unix

package environment_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/environment"
)

// The probe is an owned task process: a descendant that keeps its stdout open
// is terminated with it, so preflight cannot hang on an inherited pipe.
func TestProbeDescendantsCannotHoldPreflightOpen(t *testing.T) {
	root := t.TempDir()
	python := filepath.Join(root, "python")
	if err := os.WriteFile(python, []byte("#!/bin/sh\nsleep 60 &\necho not-json\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := environment.Check(t.Context(), environment.Request{Python: python, ProjectRoot: root})
	if err == nil {
		t.Fatal("invalid probe output accepted")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("preflight waited %s for a descendant holding the probe's stdout", elapsed)
	}
}
