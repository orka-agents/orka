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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestRun_SIGTERMStopsResultDeliveryWithoutLateEvents(t *testing.T) {
	testGeneralWorkerSIGTERM(t, "success")
}

func TestRun_SIGTERMStopsFailedCommandResultDeliveryWithoutLateEvents(t *testing.T) {
	testGeneralWorkerSIGTERM(t, "failure")
}

func testGeneralWorkerSIGTERM(t *testing.T, commandMode string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM subprocess behavior is Unix-specific")
	}
	var mu sync.Mutex
	var eventTypes []string
	resultStarted := make(chan struct{})
	releaseResult := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/internal/v1/results/default/sigterm-task"):
			startedOnce.Do(func() { close(resultStarted) })
			<-releaseResult
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/internal/v1/events/default/task/sigterm-task"):
			defer r.Body.Close() //nolint:errcheck
			var body struct {
				Type string `json:"type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			eventTypes = append(eventTypes, body.Type)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	defer close(releaseResult)

	cmd := exec.Command(os.Args[0], "-test.run=^TestGeneralWorkerSIGTERMHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		"ORKA_TEST_GENERAL_SIGTERM_HELPER=1",
		"ORKA_TEST_GENERAL_SIGTERM_COMMAND="+commandMode,
		workerenv.ControllerURL+"="+server.URL,
		workerenv.ResultEndpoint+"=",
		workerenv.TaskNamespace+"=default",
		workerenv.TaskName+"=sigterm-task",
		workerenv.GitRepo+"=",
		workerenv.PriorTask+"=",
		workerenv.ResultStdout+"=",
		"ORKA_ARTIFACTS_DIR="+filepath.Join(t.TempDir(), "artifacts"),
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper worker: %v", err)
	}

	select {
	case <-resultStarted:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("timed out waiting for result submission; helper output:\n%s", output.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("signal helper worker: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper worker exited with %v; output:\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("helper worker did not exit promptly after SIGTERM; output:\n%s", output.String())
	}

	mu.Lock()
	gotEvents := append([]string(nil), eventTypes...)
	mu.Unlock()
	lateEventTypes := []string{
		"ResultSubmitted", "ArtifactUploadCompleted", "ArtifactUploadFailed", "WorkerCompleted",
	}
	for _, lateType := range lateEventTypes {
		if slices.Contains(gotEvents, lateType) {
			t.Fatalf("events after SIGTERM = %#v, must not contain %s", gotEvents, lateType)
		}
	}
}

func TestGeneralWorkerSIGTERMHelper(t *testing.T) {
	if os.Getenv("ORKA_TEST_GENERAL_SIGTERM_HELPER") != "1" {
		return
	}
	originalArgs := os.Args
	switch os.Getenv("ORKA_TEST_GENERAL_SIGTERM_COMMAND") {
	case "failure":
		os.Args = []string{"worker", "sh", "-c", "printf failed-result; exit 7"}
	default:
		os.Args = []string{"worker", "printf", "result ready"}
	}
	defer func() { os.Args = originalArgs }()

	err := run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run() error = %v, want context canceled", err)
	}
}

func TestRun_Success(t *testing.T) {
	os.Args = []string{"worker", "echo", "hello"}
	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestRun_NoCommand(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Unsetenv("ORKA_COMMAND") //nolint:errcheck
	err := run()
	if err == nil {
		t.Error("run() should return error when no command specified")
	}
}

func TestRun_CommandFromEnv(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Setenv("ORKA_COMMAND", "echo hello") //nolint:errcheck
	defer os.Unsetenv("ORKA_COMMAND")       //nolint:errcheck

	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestRun_EmitsWorkerEventsOnSuccess(t *testing.T) {
	eventTypes := captureGeneralWorkerHTTPEvents(t, "general-success-task", func() {
		origArgs := os.Args
		t.Cleanup(func() { os.Args = origArgs })
		os.Args = []string{"worker", "printf", "hello"}

		if err := run(); err != nil {
			t.Fatalf("run() error = %v", err)
		}
	})

	want := []string{"WorkerStarted", "ResultSubmitted", "WorkerCompleted"}
	if !reflect.DeepEqual(eventTypes, want) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, want)
	}
}

func TestRun_EmitsWorkerFailedEventOnError(t *testing.T) {
	eventTypes := captureGeneralWorkerHTTPEvents(t, "general-failure-task", func() {
		origArgs := os.Args
		t.Cleanup(func() { os.Args = origArgs })
		os.Args = []string{"worker", "nonexistent_command_12345"}

		if err := run(); err == nil {
			t.Fatal("run() error = nil, want command error")
		}
	})

	want := []string{"WorkerStarted", "WorkerFailed"}
	if !reflect.DeepEqual(eventTypes, want) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, want)
	}
}

func captureGeneralWorkerHTTPEvents(t *testing.T, taskName string, runWorker func()) []string {
	t.Helper()

	var mu sync.Mutex
	var eventTypes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/internal/v1/events/default/task/"+taskName):
			defer r.Body.Close() //nolint:errcheck
			var body struct {
				Type     string `json:"type"`
				TaskName string `json:"taskName"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode event body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.TaskName != taskName {
				t.Errorf("event taskName = %q, want %q", body.TaskName, taskName)
			}
			mu.Lock()
			eventTypes = append(eventTypes, body.Type)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(r.URL.Path, "/internal/v1/results/default/"+taskName):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv("ORKA_ARTIFACTS_DIR", filepath.Join(t.TempDir(), "artifacts"))
	runWorker()

	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), eventTypes...)
}

func TestWorkspaceRootUsesSubPath(t *testing.T) {
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "src")
	if got := workspaceRoot(); got != filepath.Join(workspaceDir, "src") {
		t.Fatalf("workspaceRoot() = %q", got)
	}
}

func TestPrepareWorkspaceIfConfiguredSetsCredentialsForExistingCheckout(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("Mkdir(.git) error = %v", err)
	}
	oldWorkspaceDir := workspaceDir
	oldSetupGitCredentials := setupGitCredentialsForGeneral
	t.Cleanup(func() {
		workspaceDir = oldWorkspaceDir
		setupGitCredentialsForGeneral = oldSetupGitCredentials
	})
	workspaceDir = workspace
	calls := 0
	setupGitCredentialsForGeneral = func() {
		calls++
	}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "./src")

	got, err := prepareWorkspaceIfConfigured(context.Background())
	if err != nil {
		t.Fatalf("prepareWorkspaceIfConfigured() error = %v", err)
	}
	if got != filepath.Join(workspace, "src") {
		t.Fatalf("prepareWorkspaceIfConfigured() = %q, want sanitized subpath root", got)
	}
	if calls != 1 {
		t.Fatalf("setupGitCredentialsForGeneral calls = %d, want 1", calls)
	}
	if os.Getenv("ORKA_WORKSPACE_SUBPATH") != "src" {
		t.Fatalf("ORKA_WORKSPACE_SUBPATH = %q, want sanitized src", os.Getenv("ORKA_WORKSPACE_SUBPATH"))
	}
}

func TestPrepareWorkspaceIfConfiguredRejectsTraversalSubPath(t *testing.T) {
	oldSetupGitCredentials := setupGitCredentialsForGeneral
	t.Cleanup(func() {
		setupGitCredentialsForGeneral = oldSetupGitCredentials
	})
	setupGitCredentialsForGeneral = func() {
		t.Fatal("setupGitCredentialsForGeneral should not be called for invalid subpath")
	}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "../outside")

	if _, err := prepareWorkspaceIfConfigured(context.Background()); err == nil {
		t.Fatal("prepareWorkspaceIfConfigured() error = nil, want traversal rejection")
	} else if !strings.Contains(err.Error(), "contains path traversal") {
		t.Fatalf("prepareWorkspaceIfConfigured() error = %v, want traversal rejection", err)
	}
}

func TestRun_CommandNotFound(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	// run() calls os.Exit for exec failures, so we test the underlying exec
	os.Args = []string{"worker", "nonexistent_command_12345"}
	err := run()
	if err == nil {
		t.Error("run() should return error for nonexistent command")
	}
	if _, ok := err.(*exec.Error); !ok {
		t.Errorf("expected *exec.Error, got %T", err)
	}
}

func TestChangedFilesForSecurityScanFetchesMissingBaseInShallowClone(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "app.go")
	runGit(t, source, "commit", "-m", "base")
	baseCommit := runGit(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(
		filepath.Join(source, "app.go"),
		[]byte("package main\n\nfunc main() {}\n"),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile(head) error = %v", err)
	}
	runGit(t, source, "commit", "-am", "head")
	headCommit := runGit(t, source, "rev-parse", "HEAD")

	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", "--depth=1", "file://"+source, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone shallow failed: %s: %v", strings.TrimSpace(string(out)), err)
	}
	if gitCommitAvailable(context.Background(), clone, baseCommit) {
		t.Fatal("base commit is already available in shallow clone; fixture is invalid")
	}

	computed, files, lineRanges, diffSummary, message, resolvedHead := changedFilesForSecurityScan(
		context.Background(), clone, baseCommit, "",
	)
	if !computed {
		t.Fatalf("changedFilesForSecurityScan() computed=false message=%q", message)
	}
	if resolvedHead != headCommit {
		t.Fatalf("resolved head = %q, want %q", resolvedHead, headCommit)
	}
	if !reflect.DeepEqual(files, []string{"app.go"}) {
		t.Fatalf("changed files = %#v, want app.go", files)
	}
	if len(lineRanges) != 1 || lineRanges[0].Path != "app.go" ||
		lineRanges[0].StartLine != 2 || lineRanges[0].EndLine != 3 {
		t.Fatalf("changed line ranges = %#v, want app.go:2-3", lineRanges)
	}
	if !strings.Contains(diffSummary, "1 changed files") {
		t.Fatalf("diff summary = %q, want changed-file count", diffSummary)
	}
	if !gitCommitAvailable(context.Background(), clone, baseCommit) {
		t.Fatal("base commit was not fetched into shallow clone")
	}
}

func TestChangedFilesForSecurityScanFallsBackToFullReviewForDeletedFiles(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "auth.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "auth.go")
	runGit(t, source, "commit", "-m", "base")
	baseCommit := runGit(t, source, "rev-parse", "HEAD")
	runGit(t, source, "rm", "auth.go")
	runGit(t, source, "commit", "-m", "remove auth")
	headCommit := runGit(t, source, "rev-parse", "HEAD")

	computed, files, lineRanges, _, message, resolvedHead := changedFilesForSecurityScan(
		context.Background(),
		source,
		baseCommit,
		headCommit,
	)
	if computed {
		t.Fatal("changedFilesForSecurityScan() computed=true, want false when deleted files require full review")
	}
	if len(files) != 0 || len(lineRanges) != 0 {
		t.Fatalf("changed files/ranges = %#v/%#v, want none when falling back to full review", files, lineRanges)
	}
	if resolvedHead != headCommit {
		t.Fatalf("resolved head = %q, want %q", resolvedHead, headCommit)
	}
	if !strings.Contains(message, "deleted files require full review") || !strings.Contains(message, "auth.go") {
		t.Fatalf("message = %q, want deleted-file full-review fallback", message)
	}
}

func TestChangedFilesForSecurityScanRejectsNonSHARevisions(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "app.go")
	runGit(t, source, "commit", "-m", "base")

	computed, files, lineRanges, _, message, _ := changedFilesForSecurityScan(context.Background(), source, "HEAD", "")
	if computed {
		t.Fatal("changedFilesForSecurityScan() computed=true, want false for non-SHA base")
	}
	if len(files) != 0 || len(lineRanges) != 0 {
		t.Fatalf("changed files/ranges = %#v/%#v, want none for rejected base", files, lineRanges)
	}
	if !strings.Contains(message, "not a hex SHA") {
		t.Fatalf("message = %q, want non-SHA rejection", message)
	}
}

func TestChangedLineRangesForSecurityScanParsesUnifiedDiff(t *testing.T) {
	diff := []byte("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1,0 +2,2 @@\n+one\n+two\n")
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{{Path: "app.go", StartLine: 2, EndLine: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestChangedLineRangesHandlesMultipleHunksSameFile(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -1 +1 @@",
		"+one",
		"@@ -10 +12,2 @@",
		"+two",
		"+three",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{
		{Path: "app.go", StartLine: 1, EndLine: 1},
		{Path: "app.go", StartLine: 12, EndLine: 13},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestChangedLineRangesIgnoresAddedLinesThatLookLikeFileHeaders(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -1 +1,2 @@",
		"+++ not a file header",
		"+ordinary addition",
		"@@ -10 +11 @@",
		"+later addition",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{
		{Path: "app.go", StartLine: 1, EndLine: 2},
		{Path: "app.go", StartLine: 11, EndLine: 11},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestChangedLineRangesPreservesDeletionOnlyHunkAnchor(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -6 +6,0 @@",
		"-removedSecurityCheck()",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{{Path: "app.go", StartLine: 6, EndLine: 6}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want deletion anchor %#v", got, want)
	}
}

func TestChangedLineRangesPreservesFirstLineDeletionAnchor(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -1 +0,0 @@",
		"-removedSecurityCheck()",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{{Path: "app.go", StartLine: 1, EndLine: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want deletion anchor %#v", got, want)
	}
}

func TestChangedFilesForSecurityScanKeepsFilesWhenLineRangeParseFails(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "app.go")
	runGit(t, source, "commit", "-m", "base")
	baseCommit := runGit(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(head) error = %v", err)
	}
	runGit(t, source, "commit", "-am", "head")
	headCommit := runGit(t, source, "rev-parse", "HEAD")

	original := changedLineRangesForSecurityScan
	changedLineRangesForSecurityScan = func(context.Context, string, string, string) ([]security.ChangedLineRange, error) {
		return nil, fmt.Errorf("parse changed line ranges: unsupported hunk header")
	}
	t.Cleanup(func() { changedLineRangesForSecurityScan = original })

	computed, files, lineRanges, diffSummary, message, _ := changedFilesForSecurityScan(
		context.Background(), source, baseCommit, headCommit,
	)
	if !computed || message != "" {
		t.Fatalf("computed=%v message=%q, want file-level fallback without error message", computed, message)
	}
	if !reflect.DeepEqual(files, []string{"app.go"}) || len(lineRanges) != 0 {
		t.Fatalf("files/ranges = %#v/%#v, want app.go and no ranges", files, lineRanges)
	}
	if !strings.Contains(diffSummary, "changed line ranges omitted") {
		t.Fatalf("diffSummary = %q, want line-range omission", diffSummary)
	}
}

func TestChangedLineRangeParserStopsAtSafetyCap(t *testing.T) {
	diff := "diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n+" +
		strings.Repeat("x", maxChangedDiffBytesForLineRanges+1)
	_, err := parseChangedLineRangesFromUnifiedDiff([]byte(diff))
	if !errors.Is(err, errChangedDiffTooLarge) {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v, want diff safety cap", err)
	}
}

func TestChangedLineRangeParserStopsAtRangeCountCap(t *testing.T) {
	var diff strings.Builder
	diff.WriteString("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n")
	for i := 1; i <= maxChangedLineRangesForArtifact+1; i++ {
		fmt.Fprintf(&diff, "@@ -%d +%d @@\n+line\n", i, i)
	}
	_, err := parseChangedLineRangesFromUnifiedDiff([]byte(diff.String()))
	if !errors.Is(err, errChangedDiffTooLarge) {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v, want range count cap", err)
	}
}

func TestChangedLineRangesRejectsUnsafePaths(t *testing.T) {
	diff := []byte("diff --git a/../secret b/../secret\n--- a/../secret\n+++ b/../secret\n@@ -1 +1 @@\n+secret\n")
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ranges = %#v, want unsafe path ignored", got)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %s: %v", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out))
}
