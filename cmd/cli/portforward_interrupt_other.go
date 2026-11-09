//go:build !unix && !windows

package main

import "os/exec"

// isolateFromTerminalInterrupt does nothing where process groups are
// unavailable.
func isolateFromTerminalInterrupt(*exec.Cmd) {}
