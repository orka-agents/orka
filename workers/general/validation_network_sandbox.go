/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"os"
	"runtime"
)

var (
	validationNetworkSandboxInstall = installValidationNetworkSandbox
	validationNetworkSandboxExec    = execValidationNetworkSandbox
)

func runValidationNetworkSandbox(command []string) error {
	if len(command) == 0 || command[0] == "" {
		return fmt.Errorf("validation network sandbox requires a command")
	}

	// Seccomp filters apply to the calling thread. Keep this goroutine on the
	// filtered thread until exec replaces the process; descendants inherit the
	// filter and cannot create IPv4 or IPv6 sockets.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := validationNetworkSandboxInstall(); err != nil {
		return fmt.Errorf("install validation network sandbox: %w", err)
	}
	if err := validationNetworkSandboxExec(command[0], command, os.Environ()); err != nil {
		return fmt.Errorf("execute validation command in network sandbox: %w", err)
	}
	return nil
}
