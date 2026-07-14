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

func configureExecCommand(cmd *exec.Cmd) {
	applyExecPlatformCancellation(cmd)
	cmd.WaitDelay = workspaceExecWaitDelay
}
