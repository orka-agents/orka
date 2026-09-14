//go:build linux

package acp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRuntimeSessionRestoreTimeoutProvesDetachedDescendantsStopped(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the production supervisor UID boundary")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	cfg := testRestoreConfig(t, "load-timeout")
	cfg.RestoreTimeout = 50 * time.Millisecond
	cfg.Process.Args = []string{"-test.run=^TestACPRestoreDetachedDescendantHelper$"}
	marker := filepath.Join(cfg.Process.Paths.Home, "restore-descendant-pid")
	cfg.Process.Environment = append(cfg.Process.Environment, "ACP_DETACHED_DESCENDANT_MARKER="+marker)
	_, err := NewRuntimeSession(context.Background(), cfg)
	failure := assertRestoreFailureCleanup(t, err)
	if !errors.Is(failure, context.DeadlineExceeded) {
		t.Fatalf("restore timeout cause = %v", failure.Cause)
	}
	for _, path := range []string{marker, filepath.Join(cfg.Process.Paths.Home, "restore-pid")} {
		pid := readPIDMarker(t, path)
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("restore returned with process %d still present: %v", pid, err)
		}
	}
}

func TestACPRestoreDetachedDescendantHelper(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_RESTORE_HELPER") != "1" {
		return
	}
	// This descendant escapes the process group and ignores SIGTERM. The
	// constructor must escalate to UID-scoped SIGKILL and reap it before
	// reporting that reconstruction can safely start in another process.
	command := exec.Command(os.Args[0], "-test.run=^TestACPDetachedDescendantProcess$")
	command.Env = append(os.Environ(), "GO_WANT_ACP_DETACHED_DESCENDANT=child")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	marker := os.Getenv("ACP_DETACHED_DESCENDANT_MARKER")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restore descendant did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	TestACPRestoreHelperProcess(t)
}
