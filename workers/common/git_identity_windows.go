//go:build windows

package common

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const workspaceGitJobExitCode = 1

func gitCommandSysProcAttr() *syscall.SysProcAttr { return nil }

type workspaceGitJobState struct {
	mu       sync.Mutex
	handle   windows.Handle
	assigned bool
	closed   bool
}

var workspaceGitJobs sync.Map

func configureWorkspaceGitCommand(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create git process job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("configure git process job: %w", err)
	}
	workspaceGitJobs.Store(cmd, &workspaceGitJobState{handle: job})
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	cmd.Cancel = func() error {
		return terminateWorkspaceGitJob(cmd)
	}
	cmd.WaitDelay = workspaceGitWaitDelay
	return nil
}

func runWorkspaceGitCommand(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := assignWorkspaceGitJob(cmd); err != nil {
		_ = terminateWorkspaceGitJob(cmd)
		_ = cmd.Wait()
		return err
	}
	if err := resumeWorkspaceGitProcess(cmd.Process.Pid); err != nil {
		_ = terminateWorkspaceGitJob(cmd)
		_ = cmd.Wait()
		return fmt.Errorf("resume git process: %w", err)
	}
	return cmd.Wait()
}

func assignWorkspaceGitJob(cmd *exec.Cmd) error {
	state, ok := loadWorkspaceGitJob(cmd)
	if !ok || cmd.Process == nil {
		return fmt.Errorf("git process job is not configured")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || state.handle == 0 {
		return os.ErrProcessDone
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open git process for job assignment: %w", err)
	}
	defer windows.CloseHandle(process) //nolint:errcheck
	if err := windows.AssignProcessToJobObject(state.handle, process); err != nil {
		return fmt.Errorf("assign git process job: %w", err)
	}
	state.assigned = true
	return nil
}

func resumeWorkspaceGitProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot) //nolint:errcheck

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == uint32(pid) {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err == nil {
				_, resumeErr := windows.ResumeThread(thread)
				_ = windows.CloseHandle(thread)
				return resumeErr
			}
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return err
		}
	}
	return fmt.Errorf("git process %d has no resumable thread", pid)
}

func cleanupWorkspaceGitDescendants(cmd *exec.Cmd) {
	value, ok := workspaceGitJobs.LoadAndDelete(cmd)
	if !ok {
		return
	}
	state := value.(*workspaceGitJobState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || state.handle == 0 {
		return
	}
	state.closed = true
	_ = windows.CloseHandle(state.handle)
	state.handle = 0
}

func terminateWorkspaceGitJob(cmd *exec.Cmd) error {
	state, ok := loadWorkspaceGitJob(cmd)
	if ok {
		state.mu.Lock()
		if !state.closed && state.assigned && state.handle != 0 {
			err := windows.TerminateJobObject(state.handle, workspaceGitJobExitCode)
			state.mu.Unlock()
			if err == nil {
				return nil
			}
		} else {
			state.mu.Unlock()
		}
	}
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func loadWorkspaceGitJob(cmd *exec.Cmd) (*workspaceGitJobState, bool) {
	if cmd == nil {
		return nil, false
	}
	value, ok := workspaceGitJobs.Load(cmd)
	if !ok {
		return nil, false
	}
	return value.(*workspaceGitJobState), true
}
