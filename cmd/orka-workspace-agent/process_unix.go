//go:build !windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func applyExecPlatformCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killExecProcessGroup(cmd)
	}
}

func cleanupExecDescendants(cmd *exec.Cmd) {
	_ = killExecProcessGroup(cmd)
}

func killExecProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
