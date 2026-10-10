//go:build windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package hyperlight

import "os/exec"

func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
