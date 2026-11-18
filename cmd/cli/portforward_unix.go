//go:build linux || darwin || freebsd || netbsd || openbsd

/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// readsProcFS is whether this system's processes are read from /proc rather
// than with ps.
const readsProcFS = runtime.GOOS == "linux"

// processStartMark returns when process pid started, or "" when it cannot be
// read. A later process that reuses the PID has a different mark. On Linux it
// is the start time in clock ticks since boot from /proc; elsewhere it is ps's
// lstart, to the second.
func processStartMark(pid int) string {
	if pid <= 0 {
		return ""
	}
	if !readsProcFS {
		return psStartMark(pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The command name in parentheses can hold spaces and parentheses, so
	// fields are counted after the last ')'. The first is field 3, state, and
	// starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

// psStartMark reads when process pid started with ps, or "" when it cannot.
func psStartMark(pid int) string {
	ps := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	// lstart is in local time and the locale's format; fix both so the mark
	// does not change with the shell's settings.
	ps.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := ps.Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// portForwardProcessRunning reports whether the cached PID is still the
// process the CLI started: a process of the current user, running the kubectl
// command line the CLI started, created when that process was.
func portForwardProcessRunning(cached *portForwardCache) bool {
	if cached.ProcessStart == "" {
		return false
	}
	uid, args, ok := processOwnerAndArgs(cached.PID)
	// The start time is read last: if the PID was reused after the command
	// line was read, the mark is the new process's and does not match.
	return ok && isOwnPortForward(uid, args, cached) && processStartMark(cached.PID) == cached.ProcessStart
}

// processOwnerAndArgs returns the user ID and command line of process pid.
func processOwnerAndArgs(pid int) (int, []string, bool) {
	if !readsProcFS {
		return psOwnerAndArgs(pid)
	}
	// Read the command line before the owner: if the PID is reused in
	// between, the owner is the new process's and the check fails.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return 0, nil, false
	}
	info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return 0, nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, nil, false
	}
	return int(stat.Uid), strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), true
}

// psOwnerAndArgs reads the user ID and command line of process pid with ps.
// -ww stops ps from cutting the command line to the terminal width. The
// command line is split on spaces, which keeps the forward's arguments intact.
func psOwnerAndArgs(pid int) (int, []string, bool) {
	out, err := exec.Command("ps", "-ww", "-o", "uid=", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, nil, false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, nil, false
	}
	uid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, nil, false
	}
	return uid, fields[1:], true
}
