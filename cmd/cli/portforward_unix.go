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

// processStartMark is not recorded here: the owner and command line of a
// process identify the forward.
func processStartMark(int) string { return "" }

// portForwardProcessRunning reports whether the cached PID is a process of the
// current user running the kubectl port-forward the CLI started.
func portForwardProcessRunning(cached *portForwardCache) bool {
	uid, args, ok := processOwnerAndArgs(cached.PID)
	return ok && isOwnPortForward(uid, args, cached)
}

// processOwnerAndArgs returns the user ID and command line of process pid.
func processOwnerAndArgs(pid int) (int, []string, bool) {
	if runtime.GOOS != "linux" {
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
