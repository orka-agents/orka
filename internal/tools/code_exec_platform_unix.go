//go:build !windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

const codeExecWaitDelay = time.Second

func newLimitedCodeExecCommand(ctx context.Context, req CodeExecutionRequest, name string, args ...string) *exec.Cmd {
	limits := codeExecLocalLimitsForRequest(req)
	limitScript := fmt.Sprintf("ulimit -t %d\nulimit -v %d 2>/dev/null || true\nulimit -u %d 2>/dev/null || true\nexec \"$@\"",
		limits.CPUSeconds,
		limits.MemoryKB,
		limits.MaxProcesses,
	)
	cmdArgs := make([]string, 0, 4+len(args))
	cmdArgs = append(cmdArgs, "-c", limitScript, "orka-code-exec", name)
	cmdArgs = append(cmdArgs, args...)
	return exec.CommandContext(ctx, "/bin/sh", cmdArgs...)
}

func applyCodeExecPlatformHardening(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if err == syscall.ESRCH {
				return nil
			}
			if killErr := cmd.Process.Kill(); killErr != nil && killErr != syscall.ESRCH {
				return killErr
			}
		}
		return nil
	}
	cmd.WaitDelay = codeExecWaitDelay
}
