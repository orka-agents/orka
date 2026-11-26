/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ahpSessionMembers lists the live processes in a session, read from /proc.
func ahpSessionMembers(t *testing.T, sid int) []int {
	t.Helper()
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	require.NoError(t, err)
	var members []int
	for _, stat := range stats {
		raw, err := os.ReadFile(stat)
		if err != nil {
			continue
		}
		line := string(raw)
		fields := strings.Fields(line[strings.LastIndex(line, ")")+1:])
		if len(fields) < 4 || fields[0] == "Z" || fields[3] != strconv.Itoa(sid) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(stat, "/proc/"), "/stat"))
		if err == nil {
			members = append(members, pid)
		}
	}
	return members
}

// A terminal's session is stopped with nothing but sh: the shell and what it
// started in the background are killed, its PID file goes, and a process
// outside the session is left alone.
func TestAHPStopSessionScript(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "terminal.pid")
	shell := exec.Command("/bin/sh", "-c", `pidfile=$1; `+ahpTerminalRecordSession+`; sleep 60 & sleep 60`, "sh", pidFile)
	shell.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, shell.Start())
	t.Cleanup(func() { _ = syscall.Kill(-shell.Process.Pid, syscall.SIGKILL) })
	outside := exec.Command("sleep", "60")
	require.NoError(t, outside.Start())
	t.Cleanup(func() { _ = outside.Process.Kill(); _ = outside.Wait() })
	require.Eventually(t, func() bool { return len(ahpSessionMembers(t, shell.Process.Pid)) >= 3 }, 5*time.Second, 20*time.Millisecond,
		"the shell and both sleeps run in its session")

	out, err := exec.Command("/bin/sh", "-c", ahpStopSessionScript, "sh", pidFile, "stop").CombinedOutput()
	require.NoError(t, err, string(out))
	exited := make(chan error, 1)
	go func() { exited <- shell.Wait() }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell outlived the script")
	}
	require.Eventually(t, func() bool { return len(ahpSessionMembers(t, shell.Process.Pid)) == 0 }, 5*time.Second, 20*time.Millisecond,
		"a process the shell started outlived the script")
	require.NoFileExists(t, pidFile)
	require.NoError(t, outside.Process.Signal(syscall.Signal(0)), "a process outside the session is left alone")
}

// ahpRecordedSession starts a session leader that records itself as a terminal
// shell does, runs script after that, and returns it once its file is written.
func ahpRecordedSession(t *testing.T, pidFile, script string) *exec.Cmd {
	t.Helper()
	shell := exec.Command("/bin/sh", "-c", `pidfile=$1; `+ahpTerminalRecordSession+`; `+script, "sh", pidFile)
	shell.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, shell.Start())
	t.Cleanup(func() {
		for _, pid := range ahpSessionMembers(t, shell.Process.Pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = shell.Wait()
	})
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(pidFile)
		return err == nil && len(strings.Fields(string(raw))) == 2
	}, 5*time.Second, 20*time.Millisecond, "the shell recorded its session")
	return shell
}

// A leftover terminal whose recorded session ID a running terminal now holds
// was reused: its file goes, and the running terminal's session is left alone.
func TestAHPStopSessionScriptSparesAReusedSession(t *testing.T) {
	dir := t.TempDir()
	livePID, stalePID := filepath.Join(dir, "live.pid"), filepath.Join(dir, "stale.pid")
	shell := ahpRecordedSession(t, livePID, "sleep 60")
	recorded, err := os.ReadFile(livePID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(stalePID, recorded, 0o600))

	out, err := exec.Command("/bin/sh", "-c", ahpStopSessionScript, "sh", stalePID, "reap", livePID).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoFileExists(t, stalePID)
	require.GreaterOrEqual(t, len(ahpSessionMembers(t, shell.Process.Pid)), 2, "the running terminal's session is left alone")
}

// A leftover record whose session ID an unrelated session now holds, one that
// began after the recorded shell, is not reaped.
func TestAHPReapSparesASessionThatReusedTheID(t *testing.T) {
	stalePID := filepath.Join(t.TempDir(), "stale.pid")
	unrelated := exec.Command("/bin/sh", "-c", "sleep 60")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, unrelated.Start())
	t.Cleanup(func() { _ = syscall.Kill(-unrelated.Process.Pid, syscall.SIGKILL); _ = unrelated.Wait() })
	// The record names the same ID with another leader's start time.
	require.NoError(t, os.WriteFile(stalePID, []byte(strconv.Itoa(unrelated.Process.Pid)+" 1\n"), 0o600))

	out, err := exec.Command("/bin/sh", "-c", ahpStopSessionScript, "sh", stalePID, "reap").CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoFileExists(t, stalePID)
	require.NotEmpty(t, ahpSessionMembers(t, unrelated.Process.Pid), "the unrelated session is left alone")
}

// What a terminal's shell started can outlive the shell, still holding its
// session ID. Without the leader to prove the session, a leftover record is
// not reaped; a running terminal's is stopped.
func TestAHPStopSessionScriptWithoutTheLeader(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "terminal.pid")
	shell := ahpRecordedSession(t, pidFile, "sleep 60 & exit 0")
	require.NoError(t, shell.Wait())
	sid := shell.Process.Pid
	require.Eventually(t, func() bool { return len(ahpSessionMembers(t, sid)) == 1 }, 5*time.Second, 20*time.Millisecond,
		"the shell's child outlives it")
	recorded, err := os.ReadFile(pidFile)
	require.NoError(t, err)

	out, err := exec.Command("/bin/sh", "-c", ahpStopSessionScript, "sh", pidFile, "reap").CombinedOutput()
	require.NoError(t, err, string(out))
	require.Len(t, ahpSessionMembers(t, sid), 1, "a leftover record without its leader is not reaped")

	require.NoError(t, os.WriteFile(pidFile, recorded, 0o600))
	out, err = exec.Command("/bin/sh", "-c", ahpStopSessionScript, "sh", pidFile, "stop").CombinedOutput()
	require.NoError(t, err, string(out))
	require.Eventually(t, func() bool { return len(ahpSessionMembers(t, sid)) == 0 }, 5*time.Second, 20*time.Millisecond,
		"a running terminal's leftover processes are stopped")
}

// Disposing of a terminal and the end of its stream both stop its session, so
// two runs race on its file, and rm -f fails when the file goes between its
// check and its unlink. The run that lost still succeeds; one that cannot
// remove the file still fails.
func TestAHPStopSessionScriptLosesTheRaceForItsFile(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "terminal.pid")
	ahpRecordedSession(t, pidFile, "sleep 60")
	bin := t.TempDir()
	// This rm removes the file, as the other run does, then fails as rm -f
	// does when it loses the race.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rm"), []byte("#!/bin/sh\n/bin/rm -f \"$2\"\necho \"rm: can't remove '$2': No such file or directory\" >&2\nexit 1\n"), 0o755))
	stop := exec.Command("/bin/sh", "-c", ahpStopSessionScript, "sh", pidFile, "stop")
	stop.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := stop.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoFileExists(t, pidFile)

	stuck := filepath.Join(dir, "stuck.pid")
	ahpRecordedSession(t, stuck, "sleep 60")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rm"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	stop = exec.Command("/bin/sh", "-c", ahpStopSessionScript, "sh", stuck, "stop")
	stop.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	_, err = stop.CombinedOutput()
	require.Error(t, err, "a file that stays is a failed stop")
}
