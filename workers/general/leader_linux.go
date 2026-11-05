//go:build linux

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"fmt"
	"os"
)

// leaderAlive reports whether pid is running, as opposed to gone or exited but
// not yet reaped. kill(pid, 0) cannot tell a zombie from a running process;
// the state field of /proc/<pid>/stat can. It follows the parenthesised
// command name, which may itself contain spaces and parentheses.
func leaderAlive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return false
	}
	switch stat[end+2] {
	case 'Z', 'X', 'x':
		return false
	}
	return true
}
