/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/workerenv"

	"github.com/orka-agents/orka/workers/common"
)

func TestSoftDeadlineMargin(t *testing.T) {
	for _, tt := range []struct {
		timeout, want time.Duration
	}{
		{2 * time.Second, time.Second},
		{30 * time.Second, 15 * time.Second},
		{time.Minute, 20 * time.Second},
		{5 * time.Minute, 30 * time.Second},
		{30 * time.Minute, 2 * time.Minute},
		{2 * time.Hour, 2 * time.Minute},
	} {
		if got := softDeadlineMargin(tt.timeout); got != tt.want {
			t.Errorf("softDeadlineMargin(%s) = %s, want %s", tt.timeout, got, tt.want)
		}
	}
}

func TestTaskTimeout(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"": 0, "soon": 0, "0": 0, "-5": 0, "1.5": 0, " 300 ": 5 * time.Minute,
	} {
		getenv := func(name string) string {
			if name == workerenv.TimeoutSeconds {
				return value
			}
			return ""
		}
		if got := taskTimeout(getenv); got != want {
			t.Errorf("taskTimeout(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestSoftDeadlineSummaryKeepsTheEndOfLongOutput(t *testing.T) {
	output := strings.Repeat("é", common.MaxStructuredSummaryChars) + "last line before the hang"
	summary := softDeadlineSummary(40*time.Second, time.Minute, output)
	if len(summary) > common.MaxStructuredSummaryChars {
		t.Fatalf("summary is %d bytes, want at most %d", len(summary), common.MaxStructuredSummaryChars)
	}
	for _, want := range []string{"40s of the 1m0s timeout", "earlier bytes omitted]", "last line before the hang"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary does not contain %q", want)
		}
	}
	if !utf8.ValidString(summary) || !strings.HasSuffix(summary, output[len(output)-1000:]) {
		t.Error("summary did not keep a whole-character tail of the output")
	}
}

// softDeadlineController records what a worker delivers.
type softDeadlineController struct {
	mu        sync.Mutex
	results   [][]byte
	artifacts map[string][]byte
}

func (c *softDeadlineController) serve(t *testing.T, taskName string) *httptest.Server {
	t.Helper()
	c.artifacts = map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.URL.Path == "/internal/v1/results/default/"+taskName:
			c.results = append(c.results, body)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/internal/v1/artifacts/default/"+taskName+"/"):
			c.artifacts[strings.TrimPrefix(r.URL.Path, "/internal/v1/artifacts/default/"+taskName+"/")] = body
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRun_SoftDeadlineDeliversPartialOutputAndArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	const taskName = "deadline-task"
	controller := &softDeadlineController{}
	server := controller.serve(t, taskName)
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.ResultEndpoint, "")
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TimeoutSeconds, "2")
	t.Setenv("ORKA_ARTIFACTS_DIR", artifacts)
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	// The background child keeps stdout open, as a browser would; stopping
	// the process group must release it well inside the margin.
	os.Args = []string{"worker", "sh", "-c",
		`printf partial-output; printf shot > "$ORKA_ARTIFACTS_DIR/shot.png"; (sleep 60) & sleep 60`}
	termination := captureTerminationMessage(t)

	started := time.Now()
	err := run()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("run() took %s, want the soft deadline to stop the command after about 1s", elapsed)
	}
	if !errors.Is(err, errSoftDeadline) || generalWorkerExitCode(err) != softDeadlineExitCode {
		t.Fatalf("run() error = %v (exit %d), want the soft deadline exit %d",
			err, generalWorkerExitCode(err), softDeadlineExitCode)
	}

	controller.mu.Lock()
	defer controller.mu.Unlock()
	if string(controller.artifacts["shot.png"]) != "shot" {
		t.Fatalf("artifacts = %v, want shot.png uploaded", controller.artifacts)
	}
	if len(controller.results) != 1 {
		t.Fatalf("posted %d results, want 1", len(controller.results))
	}
	var result common.StructuredResult
	if err := json.Unmarshal(controller.results[0], &result); err != nil {
		t.Fatalf("result is not a structured result: %v\n%s", err, controller.results[0])
	}
	if result.Version != 1 || result.Outcome != common.ResultOutcomeUnknown ||
		!strings.Contains(result.Summary, "partial-output") {
		t.Fatalf("result = %#v", result)
	}
	if result.Data["reason"] != "soft_deadline" || result.Data["timeoutSeconds"] != float64(2) {
		t.Fatalf("result data = %#v", result.Data)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Filename != "shot.png" ||
		result.Artifacts[0].ContentType != "image/png" {
		t.Fatalf("result artifacts = %#v", result.Artifacts)
	}
	if got, _ := os.ReadFile(termination); !strings.Contains(string(got), workerenv.SoftDeadlineTerminationMessage) {
		t.Fatalf("termination message = %q, want the soft-deadline marker so the controller does not retry", got)
	}
}

// captureTerminationMessage points the worker's termination message at a
// file the test can read.
func captureTerminationMessage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "termination-log")
	previous := terminationMessagePath
	terminationMessagePath = path
	t.Cleanup(func() { terminationMessagePath = previous })
	return path
}

func TestTaskStart(t *testing.T) {
	workerStart := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	timeout := 10 * time.Minute
	for _, tt := range []struct {
		name     string
		deadline string
		timeout  time.Duration
		want     time.Time
	}{
		{name: "no deadline", timeout: timeout, want: workerStart},
		{name: "malformed deadline", deadline: "soon", timeout: timeout, want: workerStart},
		{name: "no timeout", deadline: workerStart.Add(timeout).Format(time.RFC3339), want: workerStart},
		// The Pod spent 90s pulling its image before the worker ran; that counts.
		{
			name:     "startup counts",
			deadline: workerStart.Add(timeout - 90*time.Second).Format(time.RFC3339),
			timeout:  timeout,
			want:     workerStart.Add(-90 * time.Second),
		},
		// A deadline that puts the start in the future means the clocks disagree; never extend the budget.
		{
			name:     "future start",
			deadline: workerStart.Add(timeout + time.Minute).Format(time.RFC3339),
			timeout:  timeout,
			want:     workerStart,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(name string) string {
				if name == workerenv.Deadline {
					return tt.deadline
				}
				return ""
			}
			if got := taskStart(getenv, workerStart, tt.timeout); !got.Equal(tt.want) {
				t.Fatalf("taskStart() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRun_BlankOutputStillUploadsArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command is a shell script")
	}
	const taskName = "quiet-task"
	controller := &softDeadlineController{}
	server := controller.serve(t, taskName)
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.ResultEndpoint, "")
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TimeoutSeconds, "")
	t.Setenv("ORKA_ARTIFACTS_DIR", artifacts)
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	// A screenshot tool that prints nothing must still get its file uploaded.
	os.Args = []string{"worker", "sh", "-c", `printf shot > "$ORKA_ARTIFACTS_DIR/shot.png"`}

	if err := run(); err != nil {
		t.Fatalf("run() error = %v, want a quiet command to succeed", err)
	}

	controller.mu.Lock()
	defer controller.mu.Unlock()
	if string(controller.artifacts["shot.png"]) != "shot" {
		t.Fatalf("artifacts = %v, want shot.png uploaded", controller.artifacts)
	}
	if len(controller.results) != 0 {
		t.Fatalf("posted %d results, want none so the controller keeps the Pod log", len(controller.results))
	}
}

func TestRun_SIGTERMBeforeSoftDeadlineDeliversNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM subprocess behavior is Unix-specific")
	}
	const taskName = "cancelled-task"
	controller := &softDeadlineController{}
	server := controller.serve(t, taskName)
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	// Other tests in this package leave os.Args pointing at "worker".
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestGeneralWorkerSoftDeadlineHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		"ORKA_TEST_GENERAL_SOFT_DEADLINE_HELPER=1",
		workerenv.ControllerURL+"="+server.URL,
		workerenv.ResultEndpoint+"=",
		workerenv.TaskNamespace+"=default",
		workerenv.TaskName+"="+taskName,
		workerenv.TimeoutSeconds+"=60",
		workerenv.GitRepo+"=",
		workerenv.PriorTask+"=",
		workerenv.ResultStdout+"=",
		"ORKA_ARTIFACTS_DIR="+artifacts,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(artifacts, "ready.txt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("helper never wrote its artifact; output:\n%s", output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper exited with %v; output:\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("helper did not exit after SIGTERM; output:\n%s", output.String())
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.results) != 0 || len(controller.artifacts) != 0 {
		t.Fatalf("a cancelled run delivered results=%d artifacts=%v", len(controller.results), controller.artifacts)
	}
}

func TestGeneralWorkerSoftDeadlineHelper(t *testing.T) {
	if os.Getenv("ORKA_TEST_GENERAL_SOFT_DEADLINE_HELPER") != "1" {
		return
	}
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	if os.Getenv("ORKA_TEST_GENERAL_SOFT_DEADLINE_MODE") == "failed-leader" {
		// run() exits the process with the leader's code.
		os.Args = []string{"worker", "sh", "-c",
			`printf failed-leader; (sleep 8; touch "$ORKA_ARTIFACTS_DIR/survived") & exit 7`}
		_ = run()
		t.Fatal("run() returned instead of exiting with the command's code")
	}
	os.Args = []string{"worker", "sh", "-c", `printf ready > "$ORKA_ARTIFACTS_DIR/ready.txt"; sleep 60`}
	if err := run(); errors.Is(err, errSoftDeadline) || err == nil {
		t.Fatalf("run() error = %v, want the cancellation, not the soft deadline", err)
	}
}

func TestSoftDeadlineDeliveryIsBoundedByTheTimeout(t *testing.T) {
	start := time.Now()
	delivery, upload := softDeadlineDelivery(start, time.Minute, start.Add(50*time.Second))
	if want := start.Add(58 * time.Second); !delivery.Equal(want) {
		t.Fatalf("delivery deadline = %s after start, want 58s", delivery.Sub(start))
	}
	if upload.Before(start.Add(55*time.Second)) || !upload.Before(delivery) {
		t.Fatalf("upload deadline = %s after start, want two thirds of the 8s left", upload.Sub(start))
	}
}

// TestRun_FailedLeaderKeepsItsResultAndLeavesNothingRunning covers a command
// whose leader exits non-zero while a background child holds its output.
func TestRun_FailedLeaderKeepsItsResultAndLeavesNothingRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	for _, timeout := range []string{"", "2"} {
		t.Run("timeout="+timeout, func(t *testing.T) {
			const taskName = "failed-leader-task"
			controller := &softDeadlineController{}
			server := controller.serve(t, taskName)
			artifacts := filepath.Join(t.TempDir(), "artifacts")
			if err := os.MkdirAll(artifacts, 0o755); err != nil {
				t.Fatal(err)
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(self, "-test.run=^TestGeneralWorkerSoftDeadlineHelper$")
			cmd.Env = append(os.Environ(),
				"ORKA_TEST_GENERAL_SOFT_DEADLINE_HELPER=1",
				"ORKA_TEST_GENERAL_SOFT_DEADLINE_MODE=failed-leader",
				workerenv.ControllerURL+"="+server.URL,
				workerenv.ResultEndpoint+"=",
				workerenv.TaskNamespace+"=default",
				workerenv.TaskName+"="+taskName,
				workerenv.TimeoutSeconds+"="+timeout,
				workerenv.GitRepo+"=",
				workerenv.PriorTask+"=",
				workerenv.ResultStdout+"=",
				"ORKA_ARTIFACTS_DIR="+artifacts,
			)
			output, _ := cmd.CombinedOutput()
			if code := cmd.ProcessState.ExitCode(); code != 7 {
				t.Fatalf("worker exit = %d, want the leader's 7; output:\n%s", code, output)
			}
			controller.mu.Lock()
			results := append([][]byte(nil), controller.results...)
			controller.mu.Unlock()
			// The child's death may add a shell notice on stderr.
			if len(results) != 1 || !strings.HasPrefix(string(results[0]), "failed-leader") {
				t.Fatalf("results = %q, want the leader's own output, not a soft-deadline account", results)
			}
			time.Sleep(4 * time.Second)
			if _, err := os.Stat(filepath.Join(artifacts, "survived")); err == nil {
				t.Fatal("the leader's background child outlived the command")
			}
		})
	}
}

func TestStartSoftDeadlineEndsAtOnceWhenSetupUsedTheBudget(t *testing.T) {
	ctx, cancel := startSoftDeadline(context.Background(), time.Now().Add(-10*time.Second), 2*time.Second)
	defer cancel()
	if ctx.Err() == nil || !errors.Is(context.Cause(ctx), errSoftDeadline) {
		t.Fatalf("context err = %v, cause = %v; want it already ended by the soft deadline", ctx.Err(), context.Cause(ctx))
	}
}

// runGeneralWorker runs the worker against a recording controller.
func runGeneralWorker(
	t *testing.T, taskName, timeoutSeconds string, args ...string,
) (*softDeadlineController, time.Duration, error) {
	t.Helper()
	controller := &softDeadlineController{}
	server := controller.serve(t, taskName)
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.ResultEndpoint, "")
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TimeoutSeconds, timeoutSeconds)
	t.Setenv("ORKA_ARTIFACTS_DIR", artifacts)
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	os.Args = append([]string{"worker"}, args...)
	started := time.Now()
	err := run()
	return controller, time.Since(started), err
}

func TestRun_LeftoverChildrenDoNotTurnSuccessIntoFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	controller, elapsed, err := runGeneralWorker(t, "leftover-task", "",
		"sh", "-c", `printf leader-done; sleep 60 &`)
	if err != nil {
		t.Fatalf("run() error = %v, want the leader's success", err)
	}
	if elapsed > commandWaitDelay+3*time.Second {
		t.Fatalf("run() took %s, want the leftover child stopped after the wait delay", elapsed)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.results) != 1 || !strings.Contains(string(controller.results[0]), "leader-done") {
		t.Fatalf("results = %q, want the command's output", controller.results)
	}
}

func TestRun_SoftDeadlineStopsChildrenAfterTheLeaderExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	// The leader succeeds at once, so the run completed; the deadline only
	// stops the child that would otherwise hold the output for the wait delay.
	_, elapsed, err := runGeneralWorker(t, "early-exit-task", "2",
		"sh", "-c", `printf leader-done; sleep 60 &`)
	if err != nil {
		t.Fatalf("run() error = %v, want the leader's success", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("run() took %s, want the soft deadline at about 1s to stop the child", elapsed)
	}
}

func TestRun_SoftDeadlineKeepsTimeForTheResultWhenUploadsStall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	const taskName = "stalled-upload-task"
	controller := &softDeadlineController{artifacts: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasPrefix(r.URL.Path, "/internal/v1/artifacts/"):
			<-r.Context().Done()
		case r.URL.Path == "/internal/v1/results/default/"+taskName:
			controller.mu.Lock()
			controller.results = append(controller.results, body)
			controller.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(server.Close)
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.ResultEndpoint, "")
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TimeoutSeconds, "10")
	t.Setenv("ORKA_ARTIFACTS_DIR", artifacts)
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	os.Args = []string{"worker", "sh", "-c", `printf shot > "$ORKA_ARTIFACTS_DIR/shot.png"; sleep 60`}

	if err := run(); !errors.Is(err, errSoftDeadline) || strings.Contains(err.Error(), "not delivered") {
		t.Fatalf("run() error = %v, want the soft deadline with its result delivered", err)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.results) != 1 {
		t.Fatalf("posted %d results, want the result despite the stalled upload", len(controller.results))
	}
	var result common.StructuredResult
	err := json.Unmarshal(controller.results[0], &result)
	if err != nil || result.Data["artifactError"] == nil || len(result.Artifacts) != 0 {
		t.Fatalf("result = %#v (%v), want the upload failure recorded and no artifacts", result, err)
	}
}

func TestCommandWaitDelayFor(t *testing.T) {
	for _, tt := range []struct {
		timeout, want time.Duration
	}{
		{timeout: 0, want: commandWaitDelay},
		{timeout: 60 * time.Second, want: commandWaitDelay},
		{timeout: 10 * time.Second, want: 1250 * time.Millisecond},
		{timeout: 2 * time.Second, want: 250 * time.Millisecond},
	} {
		if got := commandWaitDelayFor(tt.timeout); got != tt.want {
			t.Errorf("commandWaitDelayFor(%s) = %s, want %s", tt.timeout, got, tt.want)
		}
	}
}

// A descendant that put itself in another process group survives the group
// kill and keeps stdout open; a short Task must still deliver before its
// Job deadline instead of waiting the full 5 s for that pipe.
func TestRun_ShortTimeoutDeliversDespiteADetachedDescendant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed to detach a descendant from the process group")
	}
	const taskName = "detached-task"
	// The marker is plain text, since pkill -f takes a regular expression.
	const detached = "import os, time; orka_test_detached_descendant = 1; os.setpgrp(); time.sleep(60)"
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", "orka_test_detached_descendant").Run() })
	controller := &softDeadlineController{}
	server := controller.serve(t, taskName)
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.ResultEndpoint, "")
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TimeoutSeconds, "2")
	t.Setenv("ORKA_ARTIFACTS_DIR", artifacts)
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	os.Args = []string{"worker", "sh", "-c", `printf partial; python3 -c '` + detached + `' & sleep 60`}

	started := time.Now()
	err := run()
	if elapsed := time.Since(started); elapsed > 1900*time.Millisecond {
		t.Fatalf("run() took %s, want delivery inside the 2s timeout despite the detached descendant", elapsed)
	}
	if !errors.Is(err, errSoftDeadline) {
		t.Fatalf("run() error = %v, want the soft deadline", err)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.results) != 1 || !strings.Contains(string(controller.results[0]), "unknown_outcome") {
		t.Fatalf("results = %q, want one soft-deadline account", controller.results)
	}
}

// A leader that moves itself out of the process group must still be stopped
// at the soft deadline, or Wait blocks until the Job deadline cancels delivery.
// The leader already heads its own group, so it joins this process's group
// instead; the group kill then reaches nothing and only the PID kill stops it.
func TestRun_SoftDeadlineStopsALeaderThatChangedProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed to move the leader to another process group")
	}
	const taskName = "regrouped-leader-task"
	const leader = "import os, sys, time; orka_test_regrouped_leader = 1; os.setpgid(0, os.getpgid(os.getppid())); " +
		"print('regrouped', flush=True); time.sleep(60)"
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", "orka_test_regrouped_leader").Run() })
	controller := &softDeadlineController{}
	server := controller.serve(t, taskName)
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.ResultEndpoint, "")
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TimeoutSeconds, "2")
	t.Setenv("ORKA_ARTIFACTS_DIR", artifacts)
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	os.Args = []string{"worker", "python3", "-c", leader}

	started := time.Now()
	err := run()
	if elapsed := time.Since(started); elapsed > 1900*time.Millisecond {
		t.Fatalf("run() took %s, want the regrouped leader killed at the 1s soft deadline", elapsed)
	}
	if !errors.Is(err, errSoftDeadline) {
		t.Fatalf("run() error = %v, want the soft deadline", err)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.results) != 1 || !strings.Contains(string(controller.results[0]), "regrouped") {
		t.Fatalf("results = %q, want one soft-deadline account with the leader's output", controller.results)
	}
}
