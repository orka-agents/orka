//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func helperCommand(t *testing.T, mode string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{executable, "-test.run=^TestProcessChild$", "--", mode}
}

func TestProcessChild(_ *testing.T) {
	if os.Getenv("ORKA_VALIDATION_TEST_CHILD") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode == "detached" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	fixture := strings.HasPrefix(mode, "fixture")
	fd := uintptr(3)
	if fixture {
		fd = 4
	}
	control := os.NewFile(fd, "guard-status")
	handshake := guardReady
	if mode == "bad-handshake" {
		handshake = "child-claimed-setup\n"
	}
	if mode == "fixture-ignores" {
		signal.Ignore(syscall.SIGTERM)
	}
	stop := make(chan os.Signal, 1)
	if mode == "fixture-orderly" {
		signal.Notify(stop, syscall.SIGTERM)
	}
	_, _ = control.WriteString(handshake)
	_ = control.Close()
	switch mode {
	case "success":
		_, _ = fmt.Fprint(os.Stdout, "observed\n")
		_, _ = fmt.Fprint(os.Stderr, "diagnostic\n")
	case "bad-handshake":
		_, _ = fmt.Fprint(os.Stdout, "{\"executed\":true}\n")
	case "timeout":
		time.Sleep(30 * time.Second)
	case "overflow":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", pv.MaxOutputBytes+1))
		time.Sleep(30 * time.Second)
	case "fixture-orderly":
		_, _ = fmt.Fprint(os.Stdout, "ready\n")
		<-stop
		_, _ = fmt.Fprint(os.Stdout, "stopped\n")
	case "fixture-ignores":
		_, _ = fmt.Fprint(os.Stdout, "ready\n")
		time.Sleep(30 * time.Second)
	case "fixture-not-ready":
		_, _ = fmt.Fprint(os.Stdout, "not-ready\n")
		time.Sleep(30 * time.Second)
	case "fixture-exits":
		_, _ = fmt.Fprint(os.Stdout, "ready\n")
		time.Sleep(100 * time.Millisecond)
		os.Exit(1)
	case "descendant":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(125)
		}
		process, err := os.StartProcess(executable,
			[]string{executable, "-test.run=^TestProcessChild$", "--", "detached"}, &os.ProcAttr{
				Env:   []string{"ORKA_VALIDATION_TEST_CHILD=1", "GORACE=atexit_sleep_ms=0"},
				Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
				Sys:   &syscall.SysProcAttr{Setsid: true},
			})
		if err != nil {
			os.Exit(125)
		}
		_, _ = fmt.Fprintf(os.Stdout, "descendant:%d\n", process.Pid)
		_ = process.Release()
	default:
		os.Exit(125)
	}
	os.Exit(0)
}

func testManager(t *testing.T) *processManager {
	t.Helper()
	if unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != nil {
		t.Fatal("test host does not support child subreaping")
	}
	manager := newProcessManager()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.cleanup(ctx); err != nil {
			t.Error(err)
		}
		if err := manager.drain(ctx); err != nil {
			t.Error(err)
		}
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0); err != nil {
			t.Error(err)
		}
	})
	return manager
}

func startTestProcess(t *testing.T, manager *processManager, mode string) *childProcess {
	t.Helper()
	spec := processSpec{
		command: helperCommand(t, mode), environment: []string{"ORKA_VALIDATION_TEST_CHILD=1", "GORACE=atexit_sleep_ms=0"},
		directory: t.TempDir(), uid: os.Getuid(), fixture: strings.HasPrefix(mode, "fixture"),
	}
	if spec.fixture {
		var err error
		spec.listener, err = preopenedListener(0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = spec.listener.Close() }()
		spec.readyOutput = "ready\n"
	}
	process, err := manager.start(spec)
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func TestProcessExitFactsAndGuardProtocol(t *testing.T) {
	for _, mode := range []string{"success", "bad-handshake"} {
		t.Run(mode, func(t *testing.T) {
			manager := testManager(t)
			process := startTestProcess(t, manager, mode)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := manager.waitGuard(ctx, process)
			if mode == "bad-handshake" {
				if err == nil || process.executed {
					t.Fatal("untrusted control output was accepted")
				}
				return
			}
			if err != nil || manager.waitSubject(ctx, process) != nil || !process.executed ||
				!process.waited || !process.status.Exited() || process.status.ExitStatus() != 0 ||
				process.finishedAt.Before(process.startedAt) {
				t.Fatalf("supervisor did not observe exact process facts: %v", err)
			}
		})
	}
}

func TestProcessTimeoutAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelEarly), func(t *testing.T) {
			manager := testManager(t)
			process := startTestProcess(t, manager, "timeout")
			setup, stopSetup := context.WithTimeout(context.Background(), time.Second)
			defer stopSetup()
			if err := manager.waitGuard(setup, process); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if cancelEarly {
				cancel()
			}
			if err := manager.waitSubject(ctx, process); err == nil {
				t.Fatal("incomplete check was accepted")
			}
			expected := context.DeadlineExceeded
			if cancelEarly {
				expected = context.Canceled
			}
			if !errors.Is(ctx.Err(), expected) {
				t.Fatal("deadline and cancellation were conflated")
			}
		})
	}
}

func TestFixtureReadinessAndOrderlyStop(t *testing.T) {
	manager := testManager(t)
	fixture := startTestProcess(t, manager, "fixture-orderly")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if manager.waitGuard(ctx, fixture) != nil || manager.waitReady(ctx, fixture) != nil {
		t.Fatal("live declared fixture readiness was not observed")
	}
	if err := manager.stopFixture(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	// Wait only for the capture, keeping descriptor teardown in the shared cleanup.
	select {
	case err := <-fixture.drains[0]:
		if err != nil {
			t.Fatal(err)
		}
		fixture.drains = fixture.drains[1:]
	case <-ctx.Done():
		t.Fatal("fixture clean-stop output was not drained")
	}
	stdout, truncated := fixture.stdout.snapshot()
	if string(stdout) != "ready\nstopped\n" || truncated {
		t.Fatal("fixture lifecycle output was not preserved")
	}
}

func TestProcessOutputOverflow(t *testing.T) {
	manager := testManager(t)
	process := startTestProcess(t, manager, "overflow")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := manager.waitGuard(ctx, process)
	if err == nil {
		err = manager.waitSubject(ctx, process)
	}
	if err == nil {
		t.Fatal("overflowing process produced usable completion")
	}
	content, truncated := process.stdout.snapshot()
	if !truncated || len(content) != pv.MaxOutputBytes {
		t.Fatal("process output was not bounded")
	}
}

func TestFixtureRequiresDeclaredReadiness(t *testing.T) {
	manager := testManager(t)
	fixture := startTestProcess(t, manager, "fixture-not-ready")
	setup, cancelSetup := context.WithTimeout(context.Background(), time.Second)
	defer cancelSetup()
	if err := manager.waitGuard(setup, fixture); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if manager.waitReady(ctx, fixture) == nil {
		t.Fatal("fixture without its declared readiness was accepted")
	}
}

func TestFixturePrematureExitAndUncleanStop(t *testing.T) {
	for _, mode := range []string{"fixture-exits", "fixture-ignores"} {
		t.Run(mode, func(t *testing.T) {
			manager := testManager(t)
			fixture := startTestProcess(t, manager, mode)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if manager.waitGuard(ctx, fixture) != nil || manager.waitReady(ctx, fixture) != nil {
				t.Fatal("fixture did not become ready")
			}
			if mode == "fixture-exits" {
				subject := startTestProcess(t, manager, "timeout")
				if manager.waitGuard(ctx, subject) != nil {
					t.Fatal("subject guard did not start")
				}
				if err := manager.waitSubject(ctx, subject); err == nil {
					t.Fatal("premature fixture death produced usable evidence")
				}
			} else {
				stopping, stop := context.WithTimeout(ctx, 30*time.Millisecond)
				defer stop()
				if err := manager.stopFixture(stopping, fixture); err == nil {
					t.Fatal("non-orderly fixture shutdown was accepted")
				}
			}
		})
	}
}

func TestFixtureShutdownHonorsCancellation(t *testing.T) {
	manager := testManager(t)
	setup, cancelSetup := context.WithTimeout(context.Background(), time.Second)
	defer cancelSetup()
	for range 2 {
		fixture := startTestProcess(t, manager, "fixture-ignores")
		if manager.waitGuard(setup, fixture) != nil || manager.waitReady(setup, fixture) != nil {
			t.Fatal("fixture did not become ready")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(30*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	if err := manager.stopFixtures(ctx); err == nil || ctx.Err() != context.Canceled {
		t.Fatal("fixture shutdown ignored worker cancellation")
	}
	if time.Since(started) >= time.Second {
		t.Fatal("fixture shutdown consumed the reserved process-cleanup grace")
	}
}

func TestCleanupReapsDetachedDescendants(t *testing.T) {
	manager := testManager(t)
	process := startTestProcess(t, manager, "descendant")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if manager.waitGuard(ctx, process) != nil || manager.waitSubject(ctx, process) != nil {
		t.Fatal("descendant fixture did not start")
	}
	if remaining, err := manager.hasDescendants(process.uid); err != nil || !remaining {
		t.Fatalf("setsid descendant escaped ownership: %v", err)
	}
	if err := manager.cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if remaining, err := manager.hasDescendants(process.uid); err != nil || remaining || !manager.noChildren {
		t.Fatal("setsid descendants were not exactly killed and reaped")
	}
}

func TestValidateSubjectExit(t *testing.T) {
	check := pv.Check{Healthy: pv.Expectation{ExitCode: 0}, Failure: pv.Expectation{ExitCode: 1}}
	for _, code := range []int{0, 1, 2, 77, 125, 126, 127} {
		process := &childProcess{
			executed: true, waited: true, status: unix.WaitStatus(code << 8),
			startedAt: time.Now(), finishedAt: time.Now().Add(time.Millisecond),
		}
		skipped, err := validateSubjectExit(process, check)
		if (err == nil) != (code < 2) || skipped != (code == 77) {
			t.Fatalf("exit %d was incorrectly classified", code)
		}
	}
	process := &childProcess{
		executed: true, waited: true, status: unix.WaitStatus(unix.SIGKILL),
		startedAt: time.Now(), finishedAt: time.Now().Add(time.Millisecond),
	}
	if _, err := validateSubjectExit(process, check); err == nil {
		t.Fatal("signal death was treated as a normal exit")
	}
}

func TestCanaryFailsClosed(t *testing.T) {
	for _, host := range []string{
		"localhost", "127.0.0.1", "::1", "0.0.0.0", "::", "169.254.169.254", "224.0.0.1", "fe80::1%eth0",
	} {
		called := false
		err := probeCanary(context.Background(), host, 8080, time.Second,
			func(context.Context, string, string) (net.Conn, error) {
				called = true
				return nil, os.ErrDeadlineExceeded
			})
		if err == nil || called {
			t.Fatal("untrusted canary address was probed or accepted")
		}
	}
	for _, result := range []string{"connected", "refused", "unroutable", "timeout", "canceled"} {
		t.Run(result, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if result == "canceled" {
				cancel()
			}
			dial := func(_ context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" || address != "192.0.2.10:8080" {
					t.Fatal("canary identity changed")
				}
				switch result {
				case "connected":
					first, second := net.Pipe()
					_ = second.Close()
					return first, nil
				case "refused":
					return nil, syscall.ECONNREFUSED
				case "unroutable":
					return nil, syscall.ENETUNREACH
				default:
					return nil, os.ErrDeadlineExceeded
				}
			}
			err := probeCanary(ctx, "192.0.2.10", 8080, time.Second, dial)
			if (err == nil) != (result == "timeout") {
				t.Fatalf("canary %s was not handled fail-closed", result)
			}
		})
	}
}
