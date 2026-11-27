//go:build windows

/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"math"
	"strconv"

	"golang.org/x/sys/windows"
)

const stillActive = 259

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

func portForwardProcessRunning(cached *portForwardCache) bool {
	return cached.ProcessStart != "" && processStartMark(cached.PID) == cached.ProcessStart
}
