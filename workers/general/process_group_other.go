//go:build !unix

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import "os/exec"

// stopCommandProcessGroup keeps the default cancellation, which stops only
// the command itself.
func stopCommandProcessGroup(*exec.Cmd) {}
