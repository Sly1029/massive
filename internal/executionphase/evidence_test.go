package executionphase

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPhaseTransitionsPreserveTheKubeletMountedInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	if err := Write(path, Evidence{Phase: Hydrate}); err != nil {
		t.Fatal(err)
	}
	// Kubelet retains the mounted inode rather than resolving a renamed path.
	mounted, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer mounted.Close()
	for _, want := range []Evidence{{Phase: Author}, {Phase: Publish}, {Fatal: true}} {
		if err := Write(path, want); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, RecordBytes)
		if n, err := mounted.ReadAt(body, 0); err != nil || n != RecordBytes {
			t.Fatalf("mounted read: %d, %v", n, err)
		}
		got, err := Parse(string(body))
		if err != nil || got != want {
			t.Fatalf("mounted evidence = %+v, error = %v, want %+v", got, err, want)
		}
		if info, err := mounted.Stat(); err != nil || info.Size() != RecordBytes {
			t.Fatalf("termination record size: %v, %v", info, err)
		}
	}
}
