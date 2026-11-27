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

const readsProcFS = runtime.GOOS == "linux"

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

func psStartMark(pid int) string {
	ps := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	ps.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := ps.Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

func portForwardProcessRunning(cached *portForwardCache) bool {
	if cached.ProcessStart == "" {
		return false
	}
	uid, args, ok := processOwnerAndArgs(cached.PID)
	return ok && isOwnPortForward(uid, args, cached) && processStartMark(cached.PID) == cached.ProcessStart
}

func processOwnerAndArgs(pid int) (int, []string, bool) {
	if !readsProcFS {
		return psOwnerAndArgs(pid)
	}
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
