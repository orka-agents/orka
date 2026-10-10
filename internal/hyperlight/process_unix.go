//go:build !windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package hyperlight

import (
	"os/exec"
	"syscall"
	"time"
)

const waitDelay = time.Second

// killProcessGroupOnCancel puts hluk in a process group of its own and kills
// the group when the context ends, so a guest that never returns cannot
// outlive its run.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			if killErr := cmd.Process.Kill(); killErr != nil && killErr != syscall.ESRCH {
				return killErr
			}
		}
		return nil
	}
	cmd.WaitDelay = waitDelay
}

// runAs runs hluk as the credential's user, groups included.
func runAs(cmd *exec.Cmd, credential *Credential) error {
	if credential == nil {
		return nil
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: credential.UID, Gid: credential.GID,
		Groups: append([]uint32{}, credential.Groups...),
	}
	return nil
}
