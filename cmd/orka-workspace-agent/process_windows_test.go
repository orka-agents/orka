//go:build windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

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
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	workspaceExecWindowsHelperModeEnv  = "ORKA_WORKSPACE_EXEC_WINDOWS_HELPER_MODE"
	workspaceExecWindowsPIDFileEnv     = "ORKA_WORKSPACE_EXEC_WINDOWS_PID_FILE"
	workspaceExecWindowsReleaseFileEnv = "ORKA_WORKSPACE_EXEC_WINDOWS_RELEASE_FILE"
)

func TestExecJobKillsDescendantsOnCancellation(t *testing.T) {
	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := newWorkspaceAgentServer()
	done := make(chan execResponse, 1)
	go func() {
		done <- server.runExec(
			ctx,
			workspaceExecWindowsHelperRequest("leader-wait", pidFile),
			normalizedExecRequest{workDir: workDir, timeout: 30 * time.Second, maxOutput: 1024},
		)
	}()

	childPID := waitForWorkspaceExecWindowsPID(t, pidFile)
	childProcess := openWorkspaceExecWindowsTestProcess(t, childPID)
	t.Cleanup(func() { closeWorkspaceExecWindowsTestProcess(childProcess) })
	cancel()
	select {
	case response := <-done:
		if response.ExitCode == 0 {
			t.Fatalf("cancelled exec exit code = 0, want nonzero; response=%#v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Windows exec command did not return")
	}
	assertWorkspaceExecWindowsProcessExited(t, childProcess, childPID)
}

func TestExecJobCleansDescendantsAfterLeaderExit(t *testing.T) {
	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "child.pid")
	server := newWorkspaceAgentServer()
	done := make(chan execResponse, 1)
	go func() {
		done <- server.runExec(
			context.Background(),
			workspaceExecWindowsHelperRequest("leader-exit", pidFile),
			normalizedExecRequest{workDir: workDir, timeout: 30 * time.Second, maxOutput: 1024},
		)
	}()

	childPID := waitForWorkspaceExecWindowsPID(t, pidFile)
	childProcess := openWorkspaceExecWindowsTestProcess(t, childPID)
	t.Cleanup(func() { closeWorkspaceExecWindowsTestProcess(childProcess) })
	if err := os.WriteFile(pidFile+".release", []byte("ready"), 0o600); err != nil {
		t.Fatalf("release leader-exit helper: %v", err)
	}
	select {
	case response := <-done:
		if response.ExitCode != 0 {
			t.Fatalf("leader-exit exec exit code = %d, want 0; response=%#v", response.ExitCode, response)
		}
	case <-time.After(workspaceExecWaitDelay + 5*time.Second):
		t.Fatal("Windows exec command did not return after its leader exited")
	}
	assertWorkspaceExecWindowsProcessExited(t, childProcess, childPID)
}

func TestCleanupExecDescendantsClosesConfiguredJobHandle(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^$")
	if err := configureExecCommand(cmd); err != nil {
		t.Fatalf("configureExecCommand() error = %v", err)
	}
	state, ok := loadExecJob(cmd)
	if !ok {
		t.Fatal("configured exec job state was not registered")
	}
	var creationFlags uint32
	if cmd.SysProcAttr != nil {
		creationFlags = cmd.SysProcAttr.CreationFlags
	}
	if creationFlags&windows.CREATE_SUSPENDED == 0 {
		t.Fatalf("exec creation flags = %#x, want CREATE_SUSPENDED", creationFlags)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	if err := windows.QueryInformationJobObject(
		state.handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
		nil,
	); err != nil {
		t.Fatalf("query exec job limits: %v", err)
	}
	if limits.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf(
			"exec job limit flags = %#x, want JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE",
			limits.BasicLimitInformation.LimitFlags,
		)
	}

	cleanupExecDescendants(cmd)
	if _, ok := loadExecJob(cmd); ok {
		t.Fatal("exec job state remains registered after cleanup")
	}
	state.mu.Lock()
	closed := state.closed
	handle := state.handle
	state.mu.Unlock()
	if !closed || handle != 0 {
		t.Fatalf("exec job cleanup state = {closed:%t handle:%d}, want closed handle 0", closed, handle)
	}

	cleanupExecDescendants(cmd)
}

func TestWorkspaceExecWindowsHelper(t *testing.T) {
	mode := os.Getenv(workspaceExecWindowsHelperModeEnv)
	if mode == "" {
		t.Skip("helper process")
	}
	if mode == "child" {
		time.Sleep(30 * time.Second)
		return
	}

	pidFile := os.Getenv(workspaceExecWindowsPIDFileEnv)
	child := exec.Command(os.Args[0], "-test.run=^TestWorkspaceExecWindowsHelper$")
	child.Env = append(os.Environ(), workspaceExecWindowsHelperModeEnv+"=child")
	if err := child.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		t.Fatalf("write helper child PID: %v", err)
	}
	if mode == "leader-exit" {
		releaseFile := os.Getenv(workspaceExecWindowsReleaseFileEnv)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(releaseFile); err == nil {
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read helper release file: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("helper release file was not written")
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("wait for helper child: %v", err)
	}
}

func workspaceExecWindowsHelperRequest(mode, pidFile string) execRequest {
	return execRequest{
		Command: []string{os.Args[0], "-test.run=^TestWorkspaceExecWindowsHelper$"},
		Env: map[string]string{
			workspaceExecWindowsHelperModeEnv:  mode,
			workspaceExecWindowsPIDFileEnv:     pidFile,
			workspaceExecWindowsReleaseFileEnv: pidFile + ".release",
		},
	}
}

func waitForWorkspaceExecWindowsPID(t *testing.T, path string) int {
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

func openWorkspaceExecWindowsTestProcess(t *testing.T, pid int) windows.Handle {
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

func assertWorkspaceExecWindowsProcessExited(t *testing.T, process windows.Handle, pid int) {
	t.Helper()
	event, err := windows.WaitForSingleObject(process, 5000)
	if err != nil {
		t.Fatalf("wait for helper child exit: %v", err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf("helper child process %d did not exit; wait result=%d", pid, event)
	}
}

func closeWorkspaceExecWindowsTestProcess(process windows.Handle) {
	if event, err := windows.WaitForSingleObject(process, 0); err == nil && event != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(process, workspaceExecJobExitCode)
	}
	_ = windows.CloseHandle(process)
}
