//go:build linux

package acp

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestOperatorShimHelperProcess is re-executed as the shim under test.
func TestOperatorShimHelperProcess(t *testing.T) {
	if os.Getenv("ORKA_TEST_OPERATOR_SHIM") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			os.Exit(RunOperatorShim(args[i+1:]))
		}
	}
	os.Exit(2)
}

func processAlive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d escaped operator containment", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOperatorShimReapsDescendantsThatLeaveTheGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	// The command starts a daemon in a new session (outside the process
	// group), prints its PID, and exits immediately.
	cmd := exec.Command(os.Args[0], "-test.run=TestOperatorShimHelperProcess", "--",
		"/bin/sh", "-c", "setsid sleep 60 </dev/null >/dev/null 2>&1 & echo $!")
	cmd.Env = append(os.Environ(), "ORKA_TEST_OPERATOR_SHIM=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read daemon PID: %v", err)
	}
	daemon, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("shim exit: %v", err)
	}
	requireGone(t, daemon)
}

func TestKillOperatorTreeKillsEscapedDescendants(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	cmd := exec.Command("/bin/sh", "-c", "setsid sleep 60 </dev/null >/dev/null 2>&1 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	daemon, _ := strconv.Atoi(strings.TrimSpace(line))
	if daemon <= 0 {
		t.Fatalf("bad daemon PID %q", line)
	}
	if descendants := processDescendants("/proc", cmd.Process.Pid, os.Getuid()); len(descendants) == 0 {
		t.Fatal("descendant scan found nothing")
	}
	KillOperatorTree(cmd.Process.Pid, os.Getuid())
	_ = cmd.Wait()
	requireGone(t, daemon)
}
