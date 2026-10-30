//go:build unix && !linux

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import "syscall"

// leaderAlive reports whether pid still exists. Without procfs this cannot
// tell an exited, unreaped leader from a running one; the window is the
// moment between its exit and Wait reaping it.
func leaderAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
