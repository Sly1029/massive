//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Use a real terminal device so auto mode's compact rendering and stdout
// isolation are exercised through the shipped CLI, without terminal substitutes.
func TestRunAutoProgressUsesARealTTY(t *testing.T) {
	binary, python, repository := progressCLI(t)
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	var stderr bytes.Buffer
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(&stderr, master); close(drained) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "run", filepath.Join(repository, "conformance", "workflows", "python-linear", "workflow.py"), "--project", "test/progress", "--store", progressStore(t), "--run-id", "tty", "--input", `{"value":1}`, "--json")
	command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
	var stdout bytes.Buffer
	command.Stdout, command.Stderr = &stdout, slave
	err = command.Run()
	_ = slave.Close()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		_ = master.Close()
		<-drained
		t.Fatal("terminal output did not drain")
	}
	if err != nil {
		t.Fatalf("TTY run: %v\n%s", err, &stderr)
	}
	if !strings.Contains(stderr.String(), "\x1b[2K") || !strings.Contains(stderr.String(), "add_one attempt 1") || !strings.Contains(stderr.String(), "total wall time") {
		t.Fatalf("missing compact terminal progress: %q", stderr.String())
	}
	assertProgressJSON(t, stdout.Bytes(), "tty", "succeeded", `{"value":2}`)
}
