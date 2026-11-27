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

func TestProcessOwnerAndArgs(t *testing.T) {
	t.Setenv("COLUMNS", "20")
	namespace, service := strings.Repeat("n", 63), strings.Repeat("s", 63)
	command := portForwardCommand(kubeconfigContext{path: "/tmp/kc", name: "prod"}, namespace, service, 4321)
	pid := startFakePortForward(t, command).Process.Pid
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
				return ok && uid == os.Getuid() && isPortForwardCommand(args, command)
			}, 5*time.Second, 50*time.Millisecond)
		})
	}
}

func TestProcessStartMark(t *testing.T) {
	if runtime.GOOS != "linux" {
		requirePS(t)
	}
	own := processStartMark(os.Getpid())
	require.NotEmpty(t, own)
	require.Equal(t, own, processStartMark(os.Getpid()))
	require.Empty(t, processStartMark(0))
	require.Empty(t, processStartMark(-1))

	first := startFakePortForward(t, []string{"kubectl", "first"})
	firstMark := processStartMark(first.Process.Pid)
	require.NotEmpty(t, firstMark)
	if runtime.GOOS == "linux" {
		time.Sleep(100 * time.Millisecond)
	} else {
		time.Sleep(1100 * time.Millisecond)
	}
	second := startFakePortForward(t, []string{"kubectl", "second"})
	secondMark := processStartMark(second.Process.Pid)
	require.NotEmpty(t, secondMark)
	require.NotEqual(t, firstMark, secondMark)
	require.Equal(t, firstMark, processStartMark(first.Process.Pid))

	require.NoError(t, first.Process.Kill())
	_ = first.Wait()
	require.Empty(t, processStartMark(first.Process.Pid))
}

func TestPSStartMark(t *testing.T) {
	requirePS(t)
	if runtime.GOOS == "linux" {
		if err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid())).Run(); err != nil {
			t.Skipf("ps does not support lstart: %v", err)
		}
	}
	own := psStartMark(os.Getpid())
	require.NotEmpty(t, own)
	for _, tz := range []string{"UTC", "Asia/Kolkata", "America/Los_Angeles"} {
		t.Setenv("TZ", tz)
		require.Equal(t, own, psStartMark(os.Getpid()), "the start time changed with TZ=%s", tz)
	}

	cmd := startFakePortForward(t, []string{"kubectl", "exits"})
	pid := cmd.Process.Pid
	require.NotEmpty(t, psStartMark(pid))
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	require.Empty(t, psStartMark(pid))
}

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
