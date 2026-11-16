//go:build windows

/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"math"
	"strconv"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// is still running.
const stillActive = 259

// processStartMark returns when running process pid was created, or "" when
// that cannot be read. While a process runs, no other process has its PID, so
// a running process with the recorded PID and creation time is the forward the
// CLI started.
func processStartMark(pid int) string {
	if pid <= 0 || int64(pid) > math.MaxUint32 {
		return ""
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil || code != stillActive {
		return ""
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return ""
	}
	return strconv.FormatInt(created.Nanoseconds(), 10)
}

// portForwardProcessRunning reports whether the process the CLI started for
// the cached forward is still running. Another process's command line cannot
// be read here with documented calls, so the forward is recognised by the
// creation time recorded when the CLI started it.
func portForwardProcessRunning(cached *portForwardCache) bool {
	return cached.ProcessStart != "" && processStartMark(cached.PID) == cached.ProcessStart
}
