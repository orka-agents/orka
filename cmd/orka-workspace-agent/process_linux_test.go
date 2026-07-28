//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestChildProcessReaperPreservesManagedChildExitStatus(t *testing.T) {
	reaper := newSynchronousChildProcessReaper()
	cmd := exec.Command("sh", "-c", "exit 23")
	if err := reaper.startCommand(cmd); err != nil {
		t.Fatalf("start managed child: %v", err)
	}
	pid := cmd.Process.Pid
	reaper.listChildren = onlyChildProcess(pid)
	waitForLinuxProcessState(t, pid, "Z")

	if _, err := reaper.reapAdoptedChildren(); err != nil {
		t.Fatalf("reap adopted children: %v", err)
	}

	err := reaper.waitCommand(cmd)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("wait managed child error = %v, want exec.ExitError", err)
	}
	if exitErr.ExitCode() != 23 {
		t.Fatalf("managed child exit code = %d, want 23", exitErr.ExitCode())
	}
}

func TestWorkspaceChildProcessReaperReapsAdoptedDescendantAsPID1(t *testing.T) {
	if os.Getpid() != 1 {
		t.Skip("workspace child reaper is enabled only when the agent is PID 1")
	}
	pidFile := filepath.Join(t.TempDir(), "descendant-pid")
	cmd := exec.Command(
		"sh",
		"-c",
		fmt.Sprintf("sleep 1 >/dev/null 2>&1 & printf '%%s' \"$!\" > %q; exit 37", pidFile),
	)
	if err := startCommand(cmd); err != nil {
		t.Fatalf("start managed root command: %v", err)
	}
	err := waitCommand(cmd)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 37 {
		t.Fatalf("managed root command wait error = %v, want exit code 37", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read adopted descendant PID: %v", err)
	}
	descendantPID, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("parse adopted descendant PID %q: %v", data, err)
	}
	state, parentID, _, ok := processStat(descendantPID)
	if !ok || state == "Z" || parentID != os.Getpid() {
		t.Fatalf(
			"descendant %d state=%q parent=%d present=%t, want live child of PID 1",
			descendantPID,
			state,
			parentID,
			ok,
		)
	}
	waitForLinuxProcessGone(t, descendantPID)
}

func TestChildProcessReaperReapsUnmanagedZombie(t *testing.T) {
	reaper := newSynchronousChildProcessReaper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start unmanaged child: %v", err)
	}
	pid := cmd.Process.Pid
	reaper.listChildren = onlyChildProcess(pid)
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	waitForLinuxProcessState(t, pid, "Z")

	reapUntilNoRunningChildren(t, reaper)
	if _, err := cmd.Process.Wait(); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("wait after external reap error = %v, want ECHILD", err)
	}
	reaped = true
}

func reapUntilNoRunningChildren(t *testing.T, reaper *childProcessReaper) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		remaining, err := reaper.reapAdoptedChildren()
		if err != nil {
			t.Fatalf("reap unmanaged child: %v", err)
		}
		if remaining == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("unmanaged child remained running past reap deadline")
}

func onlyChildProcess(pid int) func(int) ([]int, error) {
	return func(int) ([]int, error) {
		return []int{pid}, nil
	}
}

func newSynchronousChildProcessReaper() *childProcessReaper {
	reaper := newChildProcessReaper(true)
	// Prevent tests from leaking a process-wide SIGCHLD loop; they drive scans
	// synchronously so each assertion has deterministic ownership.
	reaper.startOnce.Do(func() {})
	return reaper
}

func waitForLinuxProcessState(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, _, _, ok := processStat(pid)
		if ok && state == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	state, _, _, ok := processStat(pid)
	t.Fatalf("process %d state = %q (present=%t), want %q", pid, state, ok, want)
}

func waitForLinuxProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("process %d still present after reap: %v", pid, err)
	}
}
