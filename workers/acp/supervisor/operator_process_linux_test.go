//go:build linux

package supervisor

import (
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func startRealOperatorTestProcess(t *testing.T) *realOperatorProcess {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return &realOperatorProcess{cmd: cmd, uid: 20001, tag: 61020}
}

// After Wait reaps the leader, its PID and therefore the process group ID can
// be reused by an unrelated group, so a late control frame must never signal
// the group.
func TestOperatorGroupSignalsStopOnceTheLeaderIsReaped(t *testing.T) {
	process := startRealOperatorTestProcess(t)
	previous := signalOperatorGroup
	t.Cleanup(func() { signalOperatorGroup = previous })
	var signals atomic.Int32
	signalOperatorGroup = func(cmd *exec.Cmd, signal os.Signal) error {
		signals.Add(1)
		return previous(cmd, signal)
	}
	if err := process.Signal("TERM"); err != nil || signals.Load() != 1 {
		t.Fatalf("a running command must still be signaled: %v (signals %d)", err, signals.Load())
	}
	_, _, _ = process.Wait()
	if err := process.Signal("INT"); err != nil || signals.Load() != 1 {
		t.Fatalf("a reaped command's process group was signaled: %v (signals %d)", err, signals.Load())
	}
}

// Once cleanup retires an operation its tag group can be reused by a later
// operation, so a late Kill (for example from the input relay) must not scan
// for or kill that tag again.
func TestOperatorKillIsFencedAfterRetire(t *testing.T) {
	process := startRealOperatorTestProcess(t)
	previous := killOperatorTagProcesses
	t.Cleanup(func() { killOperatorTagProcesses = previous })
	var kills atomic.Int32
	killOperatorTagProcesses = func(int, int) bool {
		kills.Add(1)
		return true
	}
	server := &Server{}
	server.finishOperation(&operatorLease{process: process})
	if kills.Load() != 1 {
		t.Fatalf("cleanup scanned the tag %d times, want 1", kills.Load())
	}
	done := make(chan struct{})
	go func() {
		process.Kill()
		_ = process.Signal("KILL")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a late Kill blocked")
	}
	if kills.Load() != 1 {
		t.Fatalf("a Kill after cleanup scanned the released tag again (%d scans)", kills.Load())
	}
}
