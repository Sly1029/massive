//go:build !windows

package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func claimCoordinator(root, project, run string) (func(), error) {
	path := filepath.Join(root, ".coordinator-locks", project, run)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("coordinator run already owned: %w", err)
	}
	return func() { file.Close() }, nil
}
