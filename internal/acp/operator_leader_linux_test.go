//go:build linux

package acp

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatalf("wait for %d to exit: %v", pid, err)
		}
		return
	}
}

func exitingCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh binary")
	}
	return exec.Command(shell, "-c", "exit 7")
}

func TestSessionReaperLeavesOperatorLeadersToTheirOwner(t *testing.T) {
	cmd := exitingCommand(t)
	release, err := StartOperatorCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	pid := cmd.Process.Pid
	waitForZombie(t, pid)

	reapExitedProcessesForUID(os.Getuid())
	reapExitedProcessesForUIDExcept(os.Getuid(), 0)

	waitForZombie(t, pid)
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("owner's Wait = %v, want the command's own exit status 7", err)
	}
}

func TestReleasedOperatorLeaderIsReapedLikeAnyOtherProcess(t *testing.T) {
	cmd := exitingCommand(t)
	release, err := StartOperatorCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	waitForZombie(t, pid)
	release()
	release()

	reapExitedProcessesForUID(os.Getuid())

	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); !errors.Is(err, unix.ECHILD) {
		t.Fatalf("a released leader was not reaped by the session-wide reaper: %v", err)
	}
	_ = cmd.Wait()
}

func TestStartOperatorCommandRegistersNothingWhenTheStartFails(t *testing.T) {
	if _, err := StartOperatorCommand(exec.Command("/nonexistent/orka-operator-command")); err == nil {
		t.Fatal("starting a missing binary must fail")
	}
	operatorLeaders.mu.Lock()
	defer operatorLeaders.mu.Unlock()
	if len(operatorLeaders.pids) != 0 {
		t.Fatalf("a failed start left leaders registered: %v", operatorLeaders.pids)
	}
}

func TestReleasingAReusedPIDLeavesTheLaterOperatorLeaderWithheld(t *testing.T) {
	const pid = 4242
	earlier := &operatorLeaderRegistration{pid: pid}
	later := &operatorLeaderRegistration{pid: pid}
	operatorLeaders.mu.Lock()
	operatorLeaders.pids[pid] = earlier
	operatorLeaders.pids[pid] = later
	operatorLeaders.mu.Unlock()
	t.Cleanup(func() { releaseOperatorLeader(later) })

	releaseOperatorLeader(earlier)
	operatorLeaders.mu.Lock()
	held := operatorLeaders.pids[pid]
	operatorLeaders.mu.Unlock()
	if held != later {
		t.Fatal("releasing a reaped leader withdrew the later command that was given its PID")
	}

	releaseOperatorLeader(later)
	releaseOperatorLeader(later)
	operatorLeaders.mu.Lock()
	defer operatorLeaders.mu.Unlock()
	if _, held := operatorLeaders.pids[pid]; held {
		t.Fatal("a command's own release left it withheld")
	}
}
