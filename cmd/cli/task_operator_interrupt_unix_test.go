//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

// fakePortForward makes newClientWithTunnel "start" a port-forward to api and
// reports how it was started and whether it was stopped.
type fakePortForward struct {
	isolated atomic.Bool
	stopped  atomic.Int32
}

func installFakePortForward(t *testing.T, api *httptest.Server) *fakePortForward {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig"))
	_, rawPort, err := net.SplitHostPort(api.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	forward := &fakePortForward{}
	previousDiscover, previousStart := discoverServiceFn, startPortForwardFn
	t.Cleanup(func() { discoverServiceFn, startPortForwardFn = previousDiscover, previousStart })
	discoverServiceFn = func(string, string) (string, string) { return "orka-system", "orka" }
	startPortForwardFn = func(_, _, _ string, isolateInterrupt bool) (int, int, func(), error) {
		forward.isolated.Store(isolateInterrupt)
		return port, 0, func() { forward.stopped.Add(1) }, nil
	}
	// An interrupt must never reach the Go default action, which would end
	// the test binary, whatever handlers the code under test has registered.
	guard := make(chan os.Signal, 8)
	signal.Notify(guard, os.Interrupt)
	t.Cleanup(func() { signal.Stop(guard) })
	return forward
}

func interruptSelf(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// operatorExecFixture serves one operator exec stream. It reports when the
// stream opened and each SIGINT the CLI forwards, and finishes the command
// with exit status 0 once finish is closed.
type operatorExecFixture struct {
	opened    chan struct{}
	forwarded atomic.Int32
	finish    chan struct{}
}

func newOperatorExecFixture(t *testing.T) (*operatorExecFixture, *httptest.Server) {
	t.Helper()
	fixture := &operatorExecFixture{opened: make(chan struct{}), finish: make(chan struct{})}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{operatoraccess.WebSocketSubprotocol}})
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		close(fixture.opened)
		go func() {
			<-fixture.finish
			_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.StatusFrame(operatoraccess.Status{Reason: operatoraccess.ExitReasonExited}))
		}()
		for {
			_, payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var control operatoraccess.Control
			if len(payload) > 1 && payload[0] == operatoraccess.ChannelControl &&
				json.Unmarshal(payload[1:], &control) == nil && control.Type == operatoraccess.ControlSignal && control.Signal == "INT" {
				fixture.forwarded.Add(1)
			}
		}
	}))
	t.Cleanup(api.Close)
	return fixture, api
}

func runTaskExec(t *testing.T) chan error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs([]string{"task", "exec", "--quiet", "my-task", "--", "sleep", "100"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	result := make(chan error, 1)
	go func() { result <- root.Execute() }()
	return result
}

// Without a terminal the first Ctrl-C is forwarded to the remote command as
// SIGINT. A port-forward the command started itself must survive it: the
// stream would otherwise drop under a command that handles SIGINT and carries
// on. It is started outside the terminal's interrupt and no handler stops it.
func TestTaskExecKeepsItsPortForwardThroughTheFirstInterrupt(t *testing.T) {
	fixture, api := newOperatorExecFixture(t)
	forward := installFakePortForward(t, api)
	result := runTaskExec(t)
	<-fixture.opened
	interruptSelf(t)
	waitFor(t, "the interrupt to be forwarded", func() bool { return fixture.forwarded.Load() == 1 })
	// Give a handler that should not exist the chance to run.
	time.Sleep(100 * time.Millisecond)
	if stopped := forward.stopped.Load(); stopped != 0 {
		t.Fatalf("the first interrupt stopped the port-forward %d time(s); the remote command is still running", stopped)
	}
	if !forward.isolated.Load() {
		t.Fatal("the port-forward shares the terminal's foreground process group, so Ctrl-C would still reach it")
	}
	close(fixture.finish)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("exec after a handled interrupt = %v, want the command's exit status 0", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("exec did not finish")
	}
	if stopped := forward.stopped.Load(); stopped != 0 {
		t.Fatalf("a completed exec stopped the port-forward %d time(s); it is kept for later commands", stopped)
	}
}

// A second Ctrl-C abandons the stream, and with it the port-forward the
// command owned.
func TestTaskExecStopsItsPortForwardWhenTheStreamIsAbandoned(t *testing.T) {
	fixture, api := newOperatorExecFixture(t)
	t.Cleanup(func() { close(fixture.finish) })
	forward := installFakePortForward(t, api)
	result := runTaskExec(t)
	<-fixture.opened
	interruptSelf(t)
	waitFor(t, "the interrupt to be forwarded", func() bool { return fixture.forwarded.Load() == 1 })
	interruptSelf(t)
	select {
	case err := <-result:
		var exitErr *operatorExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 {
			t.Fatalf("abandoned exec = %v, want exit 130", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("exec did not return after the second interrupt")
	}
	if stopped := forward.stopped.Load(); stopped != 1 {
		t.Fatalf("abandoning the stream stopped the port-forward %d time(s), want 1", stopped)
	}
}

// Every other command keeps the existing behavior: an interrupt stops a
// port-forward it started.
func TestNewClientFromCmdStopsItsPortForwardOnInterrupt(t *testing.T) {
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()
	forward := installFakePortForward(t, api)
	if c := newClientFromCmd(newRootCmd()); c == nil {
		t.Fatal("no client")
	}
	if forward.isolated.Load() {
		t.Fatal("a command that does not own its port-forward must start it as before")
	}
	interruptSelf(t)
	waitFor(t, "the port-forward to stop", func() bool { return forward.stopped.Load() == 1 })
}

func TestForwardOperatorInterruptsForwardsOnceThenAbandons(t *testing.T) {
	interrupts := make(chan os.Signal, 2)
	controls := make(chan operatoraccess.Control, 8)
	abandoned := make(chan struct{})
	go forwardOperatorInterrupts(context.Background(), interrupts, controls, func() { close(abandoned) })
	interrupts <- os.Interrupt
	select {
	case control := <-controls:
		if control.Type != operatoraccess.ControlSignal || control.Signal != "INT" {
			t.Fatalf("forwarded %+v, want SIGINT", control)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first interrupt was not forwarded")
	}
	select {
	case <-abandoned:
		t.Fatal("the first interrupt abandoned the stream")
	case <-time.After(50 * time.Millisecond):
	}
	interrupts <- os.Interrupt
	select {
	case <-abandoned:
	case <-time.After(5 * time.Second):
		t.Fatal("the second interrupt did not abandon the stream")
	}
}

// Ctrl-C signals the terminal's foreground process group. A port-forward the
// exec owns is started in a group of its own so only the CLI decides when it
// ends.
func TestIsolateFromTerminalInterruptLeavesTheForegroundProcessGroup(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(sleep, "30")
	cmd.Stderr = &stderr
	isolateFromTerminalInterrupt(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	group, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if group == syscall.Getpgrp() || group != cmd.Process.Pid {
		t.Fatalf("child process group = %d (ours %d, child %d), want a group of its own", group, syscall.Getpgrp(), cmd.Process.Pid)
	}
}
