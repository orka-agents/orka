//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// isolateFromTerminalInterrupt starts cmd in a new process group, for which
// Windows disables the console's Ctrl-C event.
func isolateFromTerminalInterrupt(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
