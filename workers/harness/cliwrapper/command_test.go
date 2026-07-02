package cliwrapper

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/workerenv"
)

const envCommandRunnerStdoutHelper = "ORKA_CLIWRAPPER_STDOUT_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(envCommandRunnerStdoutHelper) == "1" {
		_, _ = os.Stdout.Write([]byte("abcdefgh"))
		return
	}
	os.Exit(m.Run())
}

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

func TestCommandRunnerPreservesFullStdoutWhenLogPreviewTruncates(t *testing.T) {
	runner := CommandRunner{StdoutLimitBytes: 5, StderrLimitBytes: 64, CancelGrace: 10 * time.Millisecond}
	result, err := runner.Run(context.Background(), &CommandSpec{
		Path: os.Args[0],
		Env:  []string{envCommandRunnerStdoutHelper + "=1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := result.Stdout; got != "abcde\n[stdout truncated at 5 bytes]" {
		t.Fatalf("Stdout preview = %q, want truncated preview marker", got)
	}
	if result.FullStdoutTruncated {
		t.Fatal("FullStdoutTruncated = true, want false for small output")
	}
	if got := result.ExactStdout(); got != "abcdefgh" {
		t.Fatalf("ExactStdout = %q, want full stdout", got)
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
		"HOME=/tmp/evil-home",
		"XDG_CONFIG_HOME=/tmp/evil-xdg",
		"GIT_CONFIG_COUNT=1",
		workerenv.GitToken + "=git-token",
		"HTTPS_PROXY=http://proxy.invalid",
		"ORKA_ARTIFACTS_DIR=/tmp/evil-artifacts",
		workerenv.GitAskpass + "=/tmp/evil-askpass",
		"LD_PRELOAD=libevil.so",
		"DYLD_INSERT_LIBRARIES=libevil.dylib",
	})
	if got := os.Getenv("PATH"); got != wrapperSafeCommandPath {
		t.Fatalf("PATH during temporary env = %q, want safe wrapper path", got)
	}
	if got := os.Getenv("ORKA_TEST_ENV"); got != "value" {
		t.Fatalf("ORKA_TEST_ENV = %q, want value", got)
	}
	if got := os.Getenv("HOME"); got != "/tmp/orka-empty-git-home" {
		t.Fatalf("HOME = %q, want safe root-prep HOME", got)
	}
	if got := os.Getenv("XDG_CONFIG_HOME"); got != "/tmp/orka-empty-git-config" {
		t.Fatalf("XDG_CONFIG_HOME = %q, want safe root-prep XDG config", got)
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
	if got := os.Getenv(workerenv.GitAskpass); got != controllerGitAskpassPath {
		t.Fatalf("%s = %q, want controller helper %q", workerenv.GitAskpass, got, controllerGitAskpassPath)
	}
	if got := os.Getenv("GIT_ASKPASS"); got != controllerGitAskpassPath {
		t.Fatalf("GIT_ASKPASS = %q, want controller helper %q", got, controllerGitAskpassPath)
	}
	if got := os.Getenv("LD_PRELOAD"); got != "" {
		t.Fatalf("LD_PRELOAD = %q, want blocked", got)
	}
	if got := os.Getenv("DYLD_INSERT_LIBRARIES"); got != "" {
		t.Fatalf("DYLD_INSERT_LIBRARIES = %q, want blocked", got)
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
