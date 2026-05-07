//go:build windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"os/exec"
)

func newLimitedCodeExecCommand(ctx context.Context, _ CodeExecutionRequest, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func applyCodeExecPlatformHardening(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
