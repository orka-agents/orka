//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func TestHTTPObserverChild(_ *testing.T) {
	if os.Getenv("ORKA_VALIDATION_TEST_HTTP_CHILD") != "1" {
		return
	}
	mode := os.Getenv("ORKA_VALIDATION_TEST_HTTP_MODE")
	control := os.NewFile(4, "guard-status")
	stop := make(chan os.Signal, 1)
	if mode == "ignores-shutdown" {
		signal.Ignore(syscall.SIGTERM)
	} else if mode != "uncaught-shutdown" {
		signal.Notify(stop, syscall.SIGTERM)
	}
	if _, err := control.WriteString(guardReady); err != nil || control.Close() != nil {
		os.Exit(125)
	}
	if mode == "constructor" {
		target := os.Getenv("ORKA_VALIDATION_TEST_HTTP_EXEC")
		if unix.Exec(target, []string{target}, []string{}) != nil {
			os.Exit(125)
		}
	}
	if mode == "stdout-only" {
		_, _ = fmt.Fprint(os.Stdout, `{"status":200,"body":"healthy"}`)
		os.Exit(0)
	}
	if mode == "forged-report" {
		_, _ = fmt.Fprintln(os.Stdout, `{"httpCompleted":true,"executed":true,"exitCode":0}`)
		os.Exit(0)
	}
	if mode == "forged-output" {
		_, _ = fmt.Fprintln(os.Stdout, "healthy")
		_, _ = fmt.Fprintln(os.Stdout, `{"status":200,"body":"healthy"}`)
		_, _ = fmt.Fprintln(os.Stdout, `{"httpCompleted":true,"executed":true,"exitCode":0}`)
	}
	if mode == "overflow" {
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", pv.MaxOutputBytes+1))
	}
	file := os.NewFile(3, "http-listener")
	listener, err := net.FileListener(file)
	if err != nil || file.Close() != nil {
		os.Exit(125)
	}
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			switch mode {
			case "forged-output":
				writer.WriteHeader(500)
				_, _ = writer.Write([]byte("broken"))
			case "late":
				writer.WriteHeader(200)
				writer.(http.Flusher).Flush()
				<-request.Context().Done()
				_, _ = writer.Write([]byte("healthy"))
			case "premature-death":
				os.Exit(0)
			default:
				_, _ = writer.Write([]byte("healthy"))
			}
		}),
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	<-stop
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if server.Shutdown(shutdown) != nil || !errors.Is(<-served, http.ErrServerClosed) {
		os.Exit(125)
	}
	os.Exit(0)
}

func workerHTTPCheck(t *testing.T) pv.Check {
	t.Helper()
	healthy, err := pv.HTTPExpectation(200, "healthy")
	if err != nil {
		t.Fatal(err)
	}
	failure, err := pv.HTTPExpectation(500, "broken")
	if err != nil {
		t.Fatal(err)
	}
	return pv.Check{
		ID: "case-one", Kind: pv.Reproduction, TimeoutSeconds: 2,
		HTTP: &pv.HTTPCheck{
			Version: pv.HTTPCheckVersion, ServerCommand: []string{"/checks/run"}, Path: "/quantity?value=-1",
		},
		Healthy: healthy, Failure: failure,
	}
}

func startTestHTTPServer(t *testing.T, manager *processManager, mode, constructor string) (*childProcess, int) {
	t.Helper()
	listener, err := preopenedTCPListener(unix.SockaddrInet6{Addr: [16]byte{15: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	address, err := unix.Getsockname(int(listener.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := []string{executable, "-test.run=^TestHTTPObserverChild$"}
	if mode == "accept-only-policy" || mode == "duplicated-listener-policy" {
		command = []string{constructor}
		if mode == "duplicated-listener-policy" {
			command = append(command, "duplicate-listener")
		}
	}
	server, err := manager.start(processSpec{
		command: command,
		environment: []string{
			"ORKA_VALIDATION_TEST_HTTP_CHILD=1", "ORKA_VALIDATION_TEST_HTTP_MODE=" + mode,
			"ORKA_VALIDATION_TEST_HTTP_EXEC=" + constructor, "GORACE=atexit_sleep_ms=0",
		},
		directory: t.TempDir(), uid: os.Getuid(), fixture: true, listener: listener,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, address.(*unix.SockaddrInet6).Port
}

func TestHTTPObserverSupervisedCompletion(t *testing.T) {
	constructor := compileGuardFixture(t, "testdata/http_constructor.c")
	acceptOnly := compileGuardFixture(t, "testdata/http_server.c")
	probe, stopProbe := context.WithTimeout(context.Background(), time.Second)
	raw, err := exec.CommandContext(probe, constructor).Output()
	stopProbe()
	if err != nil || string(raw) != "healthy\n" {
		t.Fatal("constructor fixture did not reproduce the raw exit-zero/healthy-output bypass")
	}
	for _, scenario := range []struct {
		mode      string
		completed bool
		broken    bool
	}{
		{mode: "healthy", completed: true},
		{mode: "accept-only-policy", completed: true},
		{mode: "duplicated-listener-policy"},
		{mode: "forged-output", completed: true, broken: true},
		{mode: "constructor"},
		{mode: "stdout-only"},
		{mode: "forged-report"},
		{mode: "premature-death"},
		{mode: "late"},
		{mode: "overflow"},
		{mode: "ignores-shutdown"},
		{mode: "uncaught-shutdown"},
	} {
		t.Run(scenario.mode, func(t *testing.T) {
			manager := testManager(t)
			binary := constructor
			if scenario.mode == "accept-only-policy" || scenario.mode == "duplicated-listener-policy" {
				binary = acceptOnly
			}
			server, port := startTestHTTPServer(t, manager, scenario.mode, binary)
			check := workerHTTPCheck(t)
			input := inputFixture(t)
			input.Manifest.Checks = []pv.Check{check}
			input.Manifest.Environment.Profile = pv.LocalServices
			var err error
			input.Binding, err = pv.NewRunBinding(input.Manifest, "attempt-http", "task-http", "")
			if err != nil {
				t.Fatal(err)
			}
			report := newReport(input, "pod-http")
			evidence := &report.Evidence
			evidence.Observation.ContainerID = "rootless-http-fixture"
			setup, cancelSetup := context.WithTimeout(context.Background(), 3*time.Second)
			err = manager.waitGuard(setup, server)
			cancelSetup()
			budget := 2 * time.Second
			if scenario.mode == "late" || scenario.mode == "ignores-shutdown" {
				budget = 100 * time.Millisecond
			}
			running, cancel := context.WithTimeout(context.Background(), budget)
			if err == nil {
				err = manager.runHTTPObservation(running, server, check, port, evidence)
			}
			cancel()
			err = errors.Join(err, manager.completeEvidence(context.Background(), evidence, nil, nil),
				captureHTTPDiagnostics(evidence, server))
			// completeEvidence already consumed these drain results.
			for _, process := range manager.processes {
				process.drains = nil
			}
			if err != nil {
				evidence.Observation.SetupError = err.Error()
				evidence.Observation.HTTPCompleted = false
			}
			if evidence.Observation.HTTPCompleted != scenario.completed {
				t.Fatalf("unexpected protected completion %v: %v", evidence.Observation.HTTPCompleted, err)
			}
			assessment := pv.EvaluateOriginal(input.Manifest, input.Binding, []pv.Observation{evidence.Observation})
			if !scenario.completed {
				if err == nil || assessment.Conclusion != pv.UnableToValidate {
					t.Fatalf("incomplete server yielded %s: %v", assessment.Conclusion, err)
				}
				return
			}
			expected, conclusion := check.Healthy, pv.NotReproduced
			if scenario.broken {
				expected, conclusion = check.Failure, pv.Reproduced
			}
			if err != nil || evidence.Observation.ExitCode == nil || *evidence.Observation.ExitCode != 0 ||
				evidence.Observation.StdoutDigest != pv.Digest([]byte(expected.Stdout)) ||
				string(evidence.Blobs[evidence.Observation.StdoutDigest]) != expected.Stdout ||
				assessment.Conclusion != conclusion {
				t.Fatalf("subject output replaced actual HTTP behavior: %s, %v", assessment.Conclusion, err)
			}
			if scenario.mode == "forged-output" &&
				!strings.Contains(string(evidence.Blobs[evidence.Observation.StderrDigest]), `"httpCompleted":true`) {
				t.Fatal("forged subject output was not kept strictly as diagnostics")
			}
		})
	}
}

func TestHTTPObserverCancellationBeforeAcceptance(t *testing.T) {
	manager := testManager(t)
	server, port := startTestHTTPServer(t, manager, "healthy", "")
	setup, stopSetup := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopSetup()
	if err := manager.waitGuard(setup, server); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	evidence := pv.ExecutionEvidence{Blobs: make(map[string][]byte)}
	if err := manager.runHTTPObservation(canceled, server, workerHTTPCheck(t), port, &evidence); err == nil ||
		evidence.Observation.HTTPCompleted || evidence.Observation.ExitCode != nil {
		t.Fatal("cancellation admitted protected completion")
	}
}
