//go:build linux

package acp

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// waitForZombie blocks until pid has exited, leaving it unreaped.
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

// An operator leader runs under the session's UID as a direct child of the
// supervisor. Stopping the adapter (a prompt cancellation does it while
// operator commands are admitted) reaps every exited process of that UID. If
// it took the leader, the owner would lose the exit status and the leader's
// PID, which is its process group ID, could be reused while the owner still
// signals that group.
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

	// Still a zombie: the PID is allocated and names only this command.
	waitForZombie(t, pid)
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("owner's Wait = %v, want the command's own exit status 7", err)
	}
}

// Once the owner has reaped the leader and released it, the PID is ordinary
// again. A stale entry would hide whichever later process is given that PID
// from the reaper, and the session's cleanup could never be proven.
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

// A leader's PID is free for reuse once its owner has reaped it, which is
// before the owner releases it. If a later operator command is given that PID
// in between, the earlier release must leave the later command withheld.
func TestReleasingAReusedPIDLeavesTheLaterOperatorLeaderWithheld(t *testing.T) {
	const pid = 4242
	earlier := &operatorLeaderRegistration{pid: pid}
	later := &operatorLeaderRegistration{pid: pid}
	operatorLeaders.mu.Lock()
	operatorLeaders.pids[pid] = earlier
	// The later command starts and registers under the same PID.
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
