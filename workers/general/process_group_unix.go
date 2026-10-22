//go:build unix

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// runCommand runs cmd in its own process group and kills the whole group when
// ctx ends, for as long as Wait runs. That includes the time after the group
// leader has exited while descendants, such as a browser, still hold its
// output pipes open, which exec.CommandContext no longer watches.
func runCommand(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	group := cmd.Process.Pid
	stop := context.AfterFunc(ctx, func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	err := cmd.Wait()
	stop()
	if errOutputHeldOpen(err) {
		// The leader exited; stop what it left behind.
		_ = syscall.Kill(-group, syscall.SIGKILL)
	}
	return err
}
