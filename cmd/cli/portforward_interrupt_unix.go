//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

func isolateFromTerminalInterrupt(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
