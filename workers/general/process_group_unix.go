//go:build unix

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"os/exec"
	"syscall"
)

// stopCommandProcessGroup runs the command in its own process group and
// stops the whole group, so children such as a browser cannot outlive it and
// keep its output pipes open.
func stopCommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
