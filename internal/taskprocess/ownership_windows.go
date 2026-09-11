package taskprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ownership struct {
	command *exec.Cmd
	job     windows.Handle
	mutex   sync.Mutex
}

func own(command *exec.Cmd) (*ownership, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create task job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure task job: %w", err)
	}
	// Suspend before any adapter code runs, then assign the job before resuming.
	// Assigning a running process would let its first child escape ownership.
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	return &ownership{command: command, job: job}, nil
}

func (owner *ownership) start(ctx context.Context) error {
	// Cancellation cannot close/recycle the handle while assignment uses it.
	owner.mutex.Lock()
	defer owner.mutex.Unlock()
	if err := owner.command.Start(); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(owner.command.Process.Pid))
	if err != nil {
		return fmt.Errorf("open suspended task: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(owner.job, process); err != nil {
		return fmt.Errorf("assign suspended task to job; ensure the host permits nested jobs: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// os/exec does not expose the initial thread handle. The suspended process
	// has executed no user code, so its sole thread is the thread to resume.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("enumerate suspended task thread: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(owner.command.Process.Pid) {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("open suspended task thread: %w", err)
		}
		defer windows.CloseHandle(thread)
		if _, err := windows.ResumeThread(thread); err != nil {
			return fmt.Errorf("resume owned task: %w", err)
		}
		return nil
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerate suspended task thread: %w", err)
	}
	return fmt.Errorf("suspended task has no initial thread; retry on a supported Windows host")
}

func (owner *ownership) kill() error {
	owner.mutex.Lock()
	defer owner.mutex.Unlock()
	owner.closeJob()
	// Also kill a process cancelled before it was assigned to the job.
	if owner.command.Process == nil {
		return os.ErrProcessDone
	}
	return owner.command.Process.Kill()
}

func (owner *ownership) closeJob() {
	if owner.job != 0 {
		_ = windows.CloseHandle(owner.job)
		owner.job = 0
	}
}

func (owner *ownership) close() {
	owner.mutex.Lock()
	defer owner.mutex.Unlock()
	owner.closeJob()
}
