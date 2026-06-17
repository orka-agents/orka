package cliwrapper

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
)

func TestCommandRunnerSuccess(t *testing.T) {
	runner := CommandRunner{StdoutLimitBytes: 5, StderrLimitBytes: 64, CancelGrace: 10 * time.Millisecond}
	result, err := runner.Run(context.Background(), &CommandSpec{Path: "/bin/sh", Args: []string{"-c", "printf abcdefgh"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestLimitedBufferTruncatesOutput(t *testing.T) {
	buf := newLimitedBuffer(5)
	if _, err := buf.Write([]byte("abcdefgh")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); got != "abcde" {
		t.Fatalf("buffer = %q, want truncated prefix", got)
	}
	if !buf.truncated {
		t.Fatal("truncated = false, want true")
	}
}

func TestCommandRunnerFailure(t *testing.T) {
	runner := CommandRunner{StdoutLimitBytes: 64, StderrLimitBytes: 64, CancelGrace: 10 * time.Millisecond}
	result, err := runner.Run(context.Background(), &CommandSpec{
		Path: "/bin/sh",
		Args: []string{"-c", "echo nope >&2; exit 7"},
	})
	if err == nil {
		t.Fatal("Run error = nil, want exit error")
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
}

func TestSetTemporaryEnvEntriesUsesSafePath(t *testing.T) {
	t.Setenv("PATH", "/original")
	restore := setTemporaryEnvEntries([]string{
		"PATH=/tmp/evil",
		"ORKA_TEST_ENV=value",
		"GIT_CONFIG_COUNT=1",
		workerenv.GitToken + "=git-token",
		"HTTPS_PROXY=http://proxy.invalid",
		"ORKA_ARTIFACTS_DIR=/tmp/evil-artifacts",
	})
	if got := os.Getenv("PATH"); got != wrapperSafeCommandPath {
		t.Fatalf("PATH during temporary env = %q, want safe wrapper path", got)
	}
	if got := os.Getenv("ORKA_TEST_ENV"); got != "value" {
		t.Fatalf("ORKA_TEST_ENV = %q, want value", got)
	}
	if got := os.Getenv("GIT_CONFIG_COUNT"); got != "" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want blocked", got)
	}
	if got := os.Getenv(workerenv.GitToken); got != "git-token" {
		t.Fatalf("%s = %q, want allowed git credential", workerenv.GitToken, got)
	}
	if got := os.Getenv("HTTPS_PROXY"); got != "" {
		t.Fatalf("HTTPS_PROXY = %q, want blocked", got)
	}
	if got := os.Getenv("ORKA_ARTIFACTS_DIR"); got != "" {
		t.Fatalf("ORKA_ARTIFACTS_DIR = %q, want blocked", got)
	}
	restore()
	if got := os.Getenv("PATH"); got != "/original" {
		t.Fatalf("PATH after restore = %q, want original", got)
	}
	if got := os.Getenv("ORKA_TEST_ENV"); got != "" {
		t.Fatalf("ORKA_TEST_ENV after restore = %q, want empty", got)
	}
}

func TestCommandRunnerSuccessReapsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups use Unix signals")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-done")
	runner := CommandRunner{StdoutLimitBytes: 64, StderrLimitBytes: 64, CancelGrace: 10 * time.Millisecond}
	result, err := runner.Run(context.Background(), &CommandSpec{
		Path: "/bin/sh",
		Args: []string{"-c", "(sleep 5; touch " + marker + ") & exit 0"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child marker exists or stat err=%v; successful process group was not reaped", err)
	}
}

func TestCommandRunnerTimeoutKillsProcess(t *testing.T) {
	runner := CommandRunner{StdoutLimitBytes: 64, StderrLimitBytes: 64, CancelGrace: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := runner.Run(ctx, &CommandSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 5"}})
	if err == nil {
		t.Fatal("Run error = nil, want timeout kill")
	}
	if !result.TimedOut {
		t.Fatalf("TimedOut = false, want true; result=%#v", result)
	}
}

func TestCommandRunnerCancelKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups use Unix signals")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-done")
	ctx, cancel := context.WithCancel(context.Background())
	runner := CommandRunner{StdoutLimitBytes: 64, StderrLimitBytes: 64, CancelGrace: 10 * time.Millisecond}
	done := make(chan CommandResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := runner.Run(ctx, &CommandSpec{
			Path: "/bin/sh",
			Args: []string{"-c", "(trap '' TERM; sleep 5; touch " + marker + ") & wait"},
		})
		done <- result
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	result := <-done
	<-errCh
	if !result.Cancelled {
		t.Fatalf("Cancelled = false, want true; result=%#v", result)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child marker exists or stat err=%v; process group was not killed", err)
	}
}
