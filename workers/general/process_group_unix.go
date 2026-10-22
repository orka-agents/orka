//go:build unix

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// runCommand runs cmd in its own process group and kills the whole group when
// ctx ends, for as long as Wait runs. That includes the time after the group
// leader has exited while descendants, such as a browser, still hold its
// output pipes open, which exec.CommandContext no longer watches. Whatever the
// leader leaves behind is stopped once Wait returns.
//
// stopped reports that ctx's end is what stopped the command: either it never
// started, or the leader was still running when the group was killed. A
// leader that had already exited on its own keeps its own result.
func runCommand(ctx context.Context, cmd *exec.Cmd) (stopped bool, err error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	group := cmd.Process.Pid
	var leaderRunning atomic.Bool
	killed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(killed)
		// Wait reaps the leader as soon as it exits, so it still exists only
		// if it was running. Its exit status cannot tell: a shell may report
		// its killed child and exit normally.
		leaderRunning.Store(syscall.Kill(group, 0) == nil)
		_ = syscall.Kill(-group, syscall.SIGKILL)
	})
	err = cmd.Wait()
	if !stop() {
		// The kill has started; wait for it to record what it found.
		<-killed
	}
	_ = syscall.Kill(-group, syscall.SIGKILL)
	return leaderRunning.Load(), err
}
