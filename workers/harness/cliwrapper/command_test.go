package cliwrapper

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
