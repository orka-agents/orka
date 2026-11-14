//go:build linux || darwin || freebsd || netbsd || openbsd

/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requirePS skips a test that reads processes with ps when ps cannot run here.
// Only Linux may lack a usable ps (busybox rejects -ww and -p): the CLI reads
// /proc there and never runs ps. The probe runs ps itself, so a broken
// psOwnerAndArgs fails the tests rather than skipping them.
func requirePS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps is not installed")
	}
	if runtime.GOOS == "linux" {
		probe := exec.Command("ps", "-ww", "-o", "uid=", "-o", "command=", "-p", strconv.Itoa(os.Getpid()))
		if err := probe.Run(); err != nil {
			t.Skipf("ps does not support -ww, -o and -p: %v", err)
		}
	}
}

// Both ways of reading a process find the fake forward's owner and arguments,
// even when COLUMNS is narrower than the command line and the longest
// namespace and Service names make it longer than a single -w allows.
func TestProcessOwnerAndArgs(t *testing.T) {
	t.Setenv("COLUMNS", "20")
	namespace, service := strings.Repeat("n", 63), strings.Repeat("s", 63)
	pid := startFakePortForward(t, namespace, service, 4321).Process.Pid
	for name, tc := range map[string]struct {
		read   func(int) (int, []string, bool)
		usesPS bool
	}{
		"processOwnerAndArgs": {read: processOwnerAndArgs, usesPS: runtime.GOOS != "linux"},
		"psOwnerAndArgs":      {read: psOwnerAndArgs, usesPS: true},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.usesPS {
				requirePS(t)
			}
			require.Eventually(t, func() bool {
				uid, args, ok := tc.read(pid)
				return ok && uid == os.Getuid() && isPortForwardCommand(args, 4321, namespace, service)
			}, 5*time.Second, 50*time.Millisecond)
		})
	}
}

// The owner comes from the process, not from the CLI: /proc and ps agree on
// the owner of PID 1, which normally belongs to another user.
func TestProcessOwnerComesFromTheProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux reads the owner from /proc")
	}
	requirePS(t)
	procUID, _, ok := processOwnerAndArgs(1)
	if !ok {
		t.Skip("PID 1 is not readable")
	}
	psUID, _, ok := psOwnerAndArgs(1)
	require.True(t, ok)
	require.Equal(t, psUID, procUID)
}
