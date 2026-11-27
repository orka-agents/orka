//go:build !unix && !windows

package main

import "os/exec"

func isolateFromTerminalInterrupt(*exec.Cmd) {}
