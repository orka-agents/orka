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
		// Only a leader that was still running counts as stopped here. Its
		// exit status cannot tell: a shell may report its killed child and
		// exit normally. Nor can its existence alone: an exited leader stays
		// a zombie until Wait reaps it.
		leaderRunning.Store(leaderAlive(group))
		killGroupAndLeader(cmd, group)
	})
	err = cmd.Wait()
	if !stop() {
		// The kill has started; wait for it to record what it found.
		<-killed
	}
	killGroupAndLeader(cmd, group)
	return leaderRunning.Load(), err
}

// killGroupAndLeader kills the command's process group and, separately, the
// leader by PID: a leader that moved itself into another group would otherwise
// survive the group kill and keep Wait blocked past the delivery window.
func killGroupAndLeader(cmd *exec.Cmd, group int) {
	_ = syscall.Kill(-group, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
