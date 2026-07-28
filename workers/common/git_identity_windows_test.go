//go:build windows

package common

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	workspaceGitWindowsHelperModeEnv = "ORKA_WORKSPACE_GIT_WINDOWS_HELPER_MODE"
	workspaceGitWindowsPIDFileEnv    = "ORKA_WORKSPACE_GIT_WINDOWS_PID_FILE"
)

func TestWorkspaceGitJobKillsDescendantsOnCancellation(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := workspaceGitWindowsHelperCommand(ctx, "leader-wait", pidFile)
	if err := configureWorkspaceGitCommand(cmd); err != nil {
		t.Fatalf("configureWorkspaceGitCommand() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		err := runWorkspaceGitCommand(cmd)
		cleanupWorkspaceGitDescendants(cmd)
		done <- err
	}()

	childPID := waitForWorkspaceGitWindowsPID(t, pidFile)
	childProcess := openWorkspaceGitWindowsTestProcess(t, childPID)
	t.Cleanup(func() { closeWorkspaceGitWindowsTestProcess(childProcess) })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Windows git command did not return")
	}
	assertWorkspaceGitWindowsProcessExited(t, childProcess, childPID)
}

func TestWorkspaceGitJobCleansDescendantsAfterLeaderExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := workspaceGitWindowsHelperCommand(context.Background(), "leader-exit", pidFile)
	if err := configureWorkspaceGitCommand(cmd); err != nil {
		t.Fatalf("configureWorkspaceGitCommand() error = %v", err)
	}
	if err := runWorkspaceGitCommand(cmd); err != nil {
		cleanupWorkspaceGitDescendants(cmd)
		t.Fatalf("runWorkspaceGitCommand() error = %v", err)
	}
	childPID := waitForWorkspaceGitWindowsPID(t, pidFile)
	childProcess := openWorkspaceGitWindowsTestProcess(t, childPID)
	t.Cleanup(func() { closeWorkspaceGitWindowsTestProcess(childProcess) })
	cleanupWorkspaceGitDescendants(cmd)
	assertWorkspaceGitWindowsProcessExited(t, childProcess, childPID)
}

func TestWorkspaceGitWindowsHelper(t *testing.T) {
	mode := os.Getenv(workspaceGitWindowsHelperModeEnv)
	if mode == "" {
		t.Skip("helper process")
	}
	if mode == "child" {
		time.Sleep(30 * time.Second)
		return
	}

	pidFile := os.Getenv(workspaceGitWindowsPIDFileEnv)
	child := exec.Command(os.Args[0], "-test.run=^TestWorkspaceGitWindowsHelper$")
	child.Env = append(os.Environ(), workspaceGitWindowsHelperModeEnv+"=child")
	if err := child.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		t.Fatalf("write helper child PID: %v", err)
	}
	if mode == "leader-exit" {
		return
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("wait for helper child: %v", err)
	}
}

func workspaceGitWindowsHelperCommand(ctx context.Context, mode, pidFile string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkspaceGitWindowsHelper$")
	cmd.Env = append(
		os.Environ(),
		workspaceGitWindowsHelperModeEnv+"="+mode,
		workspaceGitWindowsPIDFileEnv+"="+pidFile,
	)
	return cmd
}

func waitForWorkspaceGitWindowsPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read helper child PID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper child PID was not written")
	return 0
}

func openWorkspaceGitWindowsTestProcess(t *testing.T, pid int) windows.Handle {
	t.Helper()
	process, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		t.Fatalf("open helper child process: %v", err)
	}
	return process
}

func assertWorkspaceGitWindowsProcessExited(t *testing.T, process windows.Handle, pid int) {
	t.Helper()
	event, err := windows.WaitForSingleObject(process, 5000)
	if err != nil {
		t.Fatalf("wait for helper child exit: %v", err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf("helper child process %d did not exit; wait result=%d", pid, event)
	}
}

func closeWorkspaceGitWindowsTestProcess(process windows.Handle) {
	if event, err := windows.WaitForSingleObject(process, 0); err == nil && event != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(process, workspaceGitJobExitCode)
	}
	_ = windows.CloseHandle(process)
}
