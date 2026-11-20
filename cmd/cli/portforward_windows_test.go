//go:build windows

/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// On Windows the forward is recognised by the creation time recorded when the
// CLI started it, and only while that process runs.
func TestPortForwardProcessRunningOnWindows(t *testing.T) {
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	mark := processStartMark(pid)
	require.NotEmpty(t, mark)

	cached := &portForwardCache{PID: pid, Port: 4321, Namespace: "orka-system", Service: "orka", ProcessStart: mark}
	require.True(t, isPortForwardProcess(cached))
	require.False(t, isPortForwardProcess(&portForwardCache{PID: pid, ProcessStart: mark + "1"}))
	require.False(t, isPortForwardProcess(&portForwardCache{PID: pid}))
	require.False(t, isPortForwardProcess(&portForwardCache{PID: os.Getpid(), ProcessStart: mark}))

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	require.False(t, isPortForwardProcess(cached))
	require.Empty(t, processStartMark(0))
}
