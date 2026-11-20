//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// isolateFromTerminalInterrupt starts cmd in its own process group. Ctrl-C
// signals the terminal's foreground process group, which a child otherwise
// shares with the CLI.
func isolateFromTerminalInterrupt(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
