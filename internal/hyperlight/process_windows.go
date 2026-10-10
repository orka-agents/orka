//go:build windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package hyperlight

import (
	"errors"
	"os"
	"os/exec"
)

// ownedBySelf has no owner to compare on Windows; ACLs guard the cache.
func ownedBySelf(os.FileInfo) bool { return true }

func runAs(_ *exec.Cmd, credential *Credential) error {
	if credential != nil {
		return errors.New("running hluk as another user is not supported on Windows")
	}
	return nil
}

func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
