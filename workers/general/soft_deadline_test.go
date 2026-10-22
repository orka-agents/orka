/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
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
	os.Args = []string{"worker", "sh", "-c", `printf ready > "$ORKA_ARTIFACTS_DIR/ready.txt"; sleep 60`}
	defer func() { os.Args = originalArgs }()
	if err := run(); errors.Is(err, errSoftDeadline) || err == nil {
		t.Fatalf("run() error = %v, want the cancellation, not the soft deadline", err)
	}
}
