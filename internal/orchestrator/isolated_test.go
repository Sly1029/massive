package orchestrator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/sourceidentity"
)

func TestRemoteSourceArchiveRetainsSemanticPackageIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workflow.py")
	original := []byte("value = 1\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	files := []SourcePackageFile{{Path: "workflow.py", Hash: canonical.DigestBytes(original)}}
	expected, err := sourceidentity.Digest([]sourceidentity.File{{Path: files[0].Path, Hash: files[0].Hash}})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := BuildSourceArchive(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceidentity.VerifyArchive(archive, expected); err != nil {
		t.Fatal(err)
	}

	changed := []byte("value = 2\n")
	if err := os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	changedArchive, err := BuildSourceArchive(root, []SourcePackageFile{{Path: "workflow.py", Hash: canonical.DigestBytes(changed)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceidentity.VerifyArchive(changedArchive, expected); err == nil {
		t.Fatal("runtime accepted source bytes from a different semantic package")
	}
}
