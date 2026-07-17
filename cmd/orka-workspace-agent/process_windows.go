//go:build windows

package main

import (
	"context"
	"fmt"
	"os/exec"
)

func configureCommandCancellation(cmd *exec.Cmd, _ bool, _, _ uint32) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

func commandProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

func terminateProcessGroup(_ int) error {
	return nil
}

func processGroupAlive(_ int) bool {
	return false
}

func validateControlAuthFile(_ string) error {
	return fmt.Errorf("workspace-agent control auth isolation is not supported on Windows")
}

func terminateAttachmentProcesses(_ context.Context) error {
	return fmt.Errorf("workspace-agent attachment process isolation is not supported on Windows")
}

func validatePrivateKeyFile(_ string) error {
	return fmt.Errorf("workspace-agent TLS key isolation is not supported on Windows")
}
