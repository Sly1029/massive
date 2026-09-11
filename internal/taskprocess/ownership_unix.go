//go:build linux || darwin

package taskprocess

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

type ownership struct{ command *exec.Cmd }

func own(command *exec.Cmd) (*ownership, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &ownership{command: command}, nil
}

func (owner *ownership) start(context.Context) error { return owner.command.Start() }

func (owner *ownership) kill() error {
	if owner.command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-owner.command.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}

func (owner *ownership) close() { _ = owner.kill() }
