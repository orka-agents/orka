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

func TestOperatorRetireReapsTheTagOnlyAfterProvenContainment(t *testing.T) {
	for _, proven := range []bool{true, false} {
		process := startRealOperatorTestProcess(t)
		previousKill, previousReap := killOperatorTagProcesses, reapOperatorTagProcesses
		killOperatorTagProcesses = func(int, int) bool { return proven }
		var reaped [][2]int
		reapOperatorTagProcesses = func(uid, tag int) { reaped = append(reaped, [2]int{uid, tag}) }
		got := process.Retire()
		_ = process.Kill()
		killOperatorTagProcesses, reapOperatorTagProcesses = previousKill, previousReap
		if got != proven {
			t.Fatalf("Retire() = %v, want %v", got, proven)
		}
		want := 0
		if proven {
			want = 1
		}
		if len(reaped) != want || (want == 1 && reaped[0] != [2]int{20001, 61020}) {
			t.Fatalf("proven=%v: reaped %v, want %d reap of uid 20001 tag 61020", proven, reaped, want)
		}
		_, _, _ = process.Wait()
	}
}

func TestOperatorWaitReleasesTheLeaderOnlyAfterReapingIt(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	release, err := startOperatorCommand(cmd)
	if err != nil {
		t.Skipf("cannot start a test command: %v", err)
	}
	t.Cleanup(release)
	var releases atomic.Int32
	var reapedAtRelease atomic.Bool
	process := &realOperatorProcess{cmd: cmd, releaseLeader: func() {
		releases.Add(1)
		reapedAtRelease.Store(cmd.ProcessState != nil)
		release()
	}}
	code, signaled, err := process.Wait()
	if code != 7 || signaled || err != nil {
		t.Fatalf("Wait = %d, %v, %v; want the command's exit status 7", code, signaled, err)
	}
	if releases.Load() != 1 || !reapedAtRelease.Load() {
		t.Fatalf("the leader was released %d time(s), reaped first: %v; want once, after the reap", releases.Load(), reapedAtRelease.Load())
	}
	_, _, _ = process.Wait()
	if releases.Load() != 1 {
		t.Fatalf("a repeated Wait released the leader again (%d)", releases.Load())
	}
}
