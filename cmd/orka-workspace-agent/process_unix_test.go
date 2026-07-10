//go:build !windows

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
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRunExecTimeoutKillsDescendantHoldingPipes(t *testing.T) {
	const returnDeadline = 2 * time.Second
	timeoutCtx := newTriggeredDeadlineContext()

	got, elapsed := runExecWithTrackedChild(
		t,
		timeoutCtx,
		`sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; wait "$child"`,
		30*time.Second,
		returnDeadline,
		timeoutCtx.expire,
	)

	if got.ExitCode != 124 {
		t.Fatalf("exitCode = %d, want 124; response=%#v", got.ExitCode, got)
	}
	if elapsed >= returnDeadline {
		t.Fatalf("runExec elapsed = %s, want less than %s", elapsed, returnDeadline)
	}
}

func TestRunExecWaitDelayBoundsDescendantHeldPipes(t *testing.T) {
	returnDeadline := workspaceExecWaitDelay + 2*time.Second

	got, elapsed := runExecWithTrackedChild(
		t,
		context.Background(),
		`sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; exit 0`,
		10*time.Second,
		returnDeadline,
		nil,
	)

	if got.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want leader exit status 0; response=%#v", got.ExitCode, got)
	}
	if elapsed >= returnDeadline {
		t.Fatalf("runExec elapsed = %s, want less than %s", elapsed, returnDeadline)
	}
}

func TestRunExecCleansUpUnixDescendantAfterLeaderExit(t *testing.T) {
	const returnDeadline = 2 * time.Second

	got, elapsed := runExecWithTrackedChild(
		t,
		context.Background(),
		`sleep 30 >/dev/null 2>&1 & child=$!; printf '%s\n' "$child" > "$1"; exit 0`,
		10*time.Second,
		returnDeadline,
		nil,
	)

	if got.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0; response=%#v", got.ExitCode, got)
	}
	if elapsed >= returnDeadline {
		t.Fatalf("runExec elapsed = %s, want less than %s", elapsed, returnDeadline)
	}
}

func TestKillExecProcessGroupReportsCompletedProcess(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0")
	configureExecCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("run completed command: %v", err)
	}
	if err := killExecProcessGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill completed process group error = %v, want os.ErrProcessDone", err)
	}
}

func runExecWithTrackedChild(
	t *testing.T,
	ctx context.Context,
	script string,
	commandTimeout time.Duration,
	returnDeadline time.Duration,
	onReady func(),
) (execResponse, time.Duration) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	childPID := 0
	defer func() {
		if childPID > 0 {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()

	server := newWorkspaceAgentServer()
	done := make(chan execResponse, 1)
	started := time.Now()
	go func() {
		done <- server.runExec(
			ctx,
			execRequest{
				Command: []string{
					"/bin/sh",
					"-c",
					script,
					"orka-workspace-agent-test",
					pidFile,
				},
			},
			normalizedExecRequest{workDir: dir, timeout: commandTimeout, maxOutput: 1024},
		)
	}()

	childPID = waitForPIDFile(t, pidFile, 5*time.Second)
	measureFrom := started
	if onReady != nil {
		measureFrom = time.Now()
		onReady()
	}
	var got execResponse
	select {
	case got = <-done:
	case <-time.After(returnDeadline):
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf(
			"runExec remained blocked %s with a descendant holding command resources",
			returnDeadline,
		)
	}
	elapsed := time.Since(measureFrom)
	waitForProcessExit(t, childPID, time.Second)
	childPID = 0
	return got, elapsed
}

type triggeredDeadlineContext struct {
	done chan struct{}
	once sync.Once
}

func newTriggeredDeadlineContext() *triggeredDeadlineContext {
	return &triggeredDeadlineContext{done: make(chan struct{})}
}

func (*triggeredDeadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *triggeredDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *triggeredDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (*triggeredDeadlineContext) Value(any) any { return nil }

func (c *triggeredDeadlineContext) expire() {
	c.once.Do(func() { close(c.done) })
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read child PID file: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child PID file %s was not created within %s", path, timeout)
	return 0
}

func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exited, err := processExited(pid)
		if err != nil {
			t.Fatalf("check child process %d: %v", pid, err)
		}
		if exited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d still exists %s after runExec returned", pid, timeout)
}

func processExited(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}

	stat, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if readErr == nil {
		closeParen := strings.LastIndexByte(string(stat), ')')
		if closeParen >= 0 {
			fields := strings.Fields(string(stat[closeParen+1:]))
			if len(fields) > 0 && fields[0] == "Z" {
				return true, nil
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	}
	return false, nil
}
