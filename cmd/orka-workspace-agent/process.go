/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"os/exec"
	"time"
)

const workspaceExecWaitDelay = time.Second

func configureExecCommand(cmd *exec.Cmd) error {
	if err := applyExecPlatformCancellation(cmd); err != nil {
		return err
	}
	cmd.WaitDelay = workspaceExecWaitDelay
	return nil
}
