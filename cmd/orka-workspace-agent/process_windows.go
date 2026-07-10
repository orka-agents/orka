//go:build windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import "os/exec"

func applyExecPlatformCancellation(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

func cleanupExecDescendants(*exec.Cmd) {}
