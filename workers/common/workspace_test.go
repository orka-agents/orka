/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
)

const (
	workspaceTestFeatureBranch = "feature-branch"
)

func TestPrepareWorkspace_NoOp(t *testing.T) {
	// When ORKA_PRIOR_TASK is not set, PrepareWorkspace should be a no-op
	os.Unsetenv("ORKA_PRIOR_TASK") //nolint:errcheck
	err := PrepareWorkspace("/tmp/test")
	if err != nil {
		t.Errorf("expected no error when ORKA_PRIOR_TASK not set, got: %v", err)
	}
}

func TestPrepareWorkspace_MissingControllerURL(t *testing.T) {
	t.Setenv("ORKA_PRIOR_TASK", "task-1")
	t.Setenv("ORKA_CONTROLLER_URL", "")
	err := PrepareWorkspace("/tmp/test")
	if err == nil {
		t.Fatal("expected error when ORKA_CONTROLLER_URL is empty")
	}
}

func TestPrepareWorkspace_NoDiffInResult(t *testing.T) {
	// Mock server returns a structured result with no diff
	sr := StructuredResult{Version: 1, Summary: "completed"}
	resultJSON, _ := json.Marshal(sr)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"result": string(resultJSON)}) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("ORKA_PRIOR_TASK", "task-1")
	t.Setenv("ORKA_PRIOR_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)

	err := PrepareWorkspace("/tmp/test")
	if err != nil {
		t.Errorf("expected no error when diff is empty, got: %v", err)
	}
}

func TestFinalizeResult_EmptyWorkDir(t *testing.T) {
	data, err := FinalizeResult("", "hello output")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "hello output" {
		t.Errorf("expected plain text output, got %q", string(data))
	}
}

func TestFinalizeResult_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	data, err := FinalizeResult(dir, "agent did stuff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "agent did stuff" {
		t.Errorf("expected plain text fallback, got %q", string(data))
	}
}

func TestParseDiffStatFiles(t *testing.T) {
	stat := ` auth.go       | 10 +++++++---
 middleware.go | 5 +++++
 2 files changed, 12 insertions(+), 3 deletions(-)
`
	files := parseDiffStatFiles(stat)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0] != "auth.go" {
		t.Errorf("expected auth.go, got %q", files[0])
	}
	if files[1] != "middleware.go" {
		t.Errorf("expected middleware.go, got %q", files[1])
	}
}

func TestPreparePullRequestReviewContextWritesIgnoredDiffFiles(t *testing.T) {
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	sourceDir := t.TempDir()
	runGitWS(t, sourceDir, "init")
	runGitWS(t, sourceDir, "checkout", "-b", "main")
	runGitWS(t, sourceDir, "config", "user.email", "test@test.com")
	runGitWS(t, sourceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, ".orka"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, ".orka", "pr-review.diff"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "base")
	runGitWS(t, sourceDir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, sourceDir, "push", gitOriginRemote, "main")
	runGitWS(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGitWS(t, sourceDir, "checkout", "-b", workspaceTestFeatureBranch)
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "feature")
	runGitWS(t, sourceDir, "push", gitOriginRemote, workspaceTestFeatureBranch)

	cloneDir := filepath.Join(t.TempDir(), "clone")
	runGitWS(t, t.TempDir(),
		"clone", "--branch", workspaceTestFeatureBranch, "--single-branch", "file://"+bareDir, cloneDir,
	)
	if err := PreparePullRequestReviewContext(cloneDir, &AgentConfig{PRBaseBranch: "main"}); err != nil {
		t.Fatalf("PreparePullRequestReviewContext() error = %v", err)
	}

	diff, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewDiffPath))
	if err != nil {
		t.Fatalf("ReadFile(diff) error = %v", err)
	}
	if !strings.Contains(string(diff), "+feature") {
		t.Fatalf("diff = %q, want feature line", string(diff))
	}
	files, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewFilesPath))
	if err != nil {
		t.Fatalf("ReadFile(files) error = %v", err)
	}
	if strings.TrimSpace(string(files)) != "README.md" {
		t.Fatalf("files = %q, want README.md", string(files))
	}
	legacyTracked, err := os.ReadFile(filepath.Join(cloneDir, ".orka", "pr-review.diff"))
	if err != nil {
		t.Fatalf("ReadFile(legacy tracked diff) error = %v", err)
	}
	if string(legacyTracked) != "tracked\n" {
		t.Fatalf("legacy tracked diff = %q, want tracked file untouched", string(legacyTracked))
	}
	status := strings.TrimSpace(runGitOutputWS(t, cloneDir, "status", "--porcelain"))
	if status != "" {
		t.Fatalf("git status = %q, want generated review context ignored", status)
	}
}

func TestPreparePullRequestReviewContextFetchesBaseFromTrustedRepo(t *testing.T) {
	baseBareDir := t.TempDir()
	runGitWS(t, baseBareDir, "init", "--bare")
	forkBareDir := t.TempDir()
	runGitWS(t, forkBareDir, "init", "--bare")

	sourceDir := t.TempDir()
	runGitWS(t, sourceDir, "init")
	runGitWS(t, sourceDir, "checkout", "-b", "main")
	runGitWS(t, sourceDir, "config", "user.email", "test@test.com")
	runGitWS(t, sourceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "base")
	runGitWS(t, sourceDir, "remote", "add", gitOriginRemote, baseBareDir)
	runGitWS(t, sourceDir, "push", gitOriginRemote, "main")
	runGitWS(t, baseBareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGitWS(t, sourceDir, "checkout", "-b", workspaceTestFeatureBranch)
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "feature")
	runGitWS(t, sourceDir, "remote", "add", "fork", forkBareDir)
	runGitWS(t, sourceDir, "push", "fork", workspaceTestFeatureBranch)

	cloneDir := filepath.Join(t.TempDir(), "fork-clone")
	runGitWS(t, t.TempDir(),
		"clone", "--branch", workspaceTestFeatureBranch, "--single-branch", "file://"+forkBareDir, cloneDir,
	)
	if err := PreparePullRequestReviewContext(cloneDir, &AgentConfig{
		PRBaseBranch: "main",
		PRBaseRepo:   "file://" + baseBareDir,
	}); err != nil {
		t.Fatalf("PreparePullRequestReviewContext() error = %v", err)
	}

	diff, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewDiffPath))
	if err != nil {
		t.Fatalf("ReadFile(diff) error = %v", err)
	}
	if !strings.Contains(string(diff), "+feature") {
		t.Fatalf("diff = %q, want trusted base comparison", string(diff))
	}
	instructions, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewInstructionsPath))
	if err != nil {
		t.Fatalf("ReadFile(instructions) error = %v", err)
	}
	if !strings.Contains(string(instructions), "Base repo: file://"+baseBareDir) {
		t.Fatalf("instructions = %q, want trusted base repo", string(instructions))
	}
}

func TestPreparePullRequestReviewContextDeepensBaseSHAForStalePR(t *testing.T) {
	baseBareDir := t.TempDir()
	runGitWS(t, baseBareDir, "init", "--bare")
	forkBareDir := t.TempDir()
	runGitWS(t, forkBareDir, "init", "--bare")

	sourceDir := t.TempDir()
	runGitWS(t, sourceDir, "init")
	runGitWS(t, sourceDir, "checkout", "-b", "main")
	runGitWS(t, sourceDir, "config", "user.email", "test@test.com")
	runGitWS(t, sourceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "base")

	runGitWS(t, sourceDir, "checkout", "-b", workspaceTestFeatureBranch)
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "feature")
	runGitWS(t, sourceDir, "remote", "add", "fork", forkBareDir)
	runGitWS(t, sourceDir, "push", "fork", workspaceTestFeatureBranch)

	runGitWS(t, sourceDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\nbase branch moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "advance base")
	baseSHA := strings.TrimSpace(runGitOutputWS(t, sourceDir, "rev-parse", "HEAD"))
	runGitWS(t, sourceDir, "remote", "add", gitOriginRemote, baseBareDir)
	runGitWS(t, sourceDir, "push", gitOriginRemote, "main")
	runGitWS(t, baseBareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	cloneDir := filepath.Join(t.TempDir(), "fork-clone")
	runGitWS(t, t.TempDir(),
		"clone", "--branch", workspaceTestFeatureBranch, "--single-branch", "file://"+forkBareDir, cloneDir,
	)
	if err := PreparePullRequestReviewContext(cloneDir, &AgentConfig{
		PRBaseBranch: "main",
		PRBaseRepo:   "file://" + baseBareDir,
		PRBaseSHA:    baseSHA,
	}); err != nil {
		t.Fatalf("PreparePullRequestReviewContext() error = %v", err)
	}

	diff, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewDiffPath))
	if err != nil {
		t.Fatalf("ReadFile(diff) error = %v", err)
	}
	if !strings.Contains(string(diff), "+feature") {
		t.Fatalf("diff = %q, want feature line", string(diff))
	}
	if strings.Contains(string(diff), "base branch moved") {
		t.Fatalf("diff = %q, want merge-base comparison excluding base-only changes", string(diff))
	}
}

func TestPreparePullRequestReviewContextFallsBackToBaseBranchWhenBaseSHAFetchFails(t *testing.T) {
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	sourceDir := t.TempDir()
	runGitWS(t, sourceDir, "init")
	runGitWS(t, sourceDir, "checkout", "-b", "main")
	runGitWS(t, sourceDir, "config", "user.email", "test@test.com")
	runGitWS(t, sourceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "base")
	runGitWS(t, sourceDir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, sourceDir, "push", gitOriginRemote, "main")
	runGitWS(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGitWS(t, sourceDir, "checkout", "-b", workspaceTestFeatureBranch)
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "feature")
	runGitWS(t, sourceDir, "push", gitOriginRemote, workspaceTestFeatureBranch)

	cloneDir := filepath.Join(t.TempDir(), "clone")
	runGitWS(t, t.TempDir(),
		"clone", "--branch", workspaceTestFeatureBranch, "--single-branch", "file://"+bareDir, cloneDir,
	)
	if err := PreparePullRequestReviewContext(cloneDir, &AgentConfig{
		PRBaseBranch: "main",
		PRBaseSHA:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatalf("PreparePullRequestReviewContext() error = %v", err)
	}

	diff, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewDiffPath))
	if err != nil {
		t.Fatalf("ReadFile(diff) error = %v", err)
	}
	if !strings.Contains(string(diff), "+feature") {
		t.Fatalf("diff = %q, want feature line from base branch fallback", string(diff))
	}
}

func TestPreparePullRequestReviewContextTruncatesLargeDiff(t *testing.T) {
	oldDiffLimit := pullRequestReviewDiffLimitBytes
	oldListLimit := pullRequestReviewListLimitBytes
	pullRequestReviewDiffLimitBytes = 256
	pullRequestReviewListLimitBytes = 1024
	t.Cleanup(func() {
		pullRequestReviewDiffLimitBytes = oldDiffLimit
		pullRequestReviewListLimitBytes = oldListLimit
	})

	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	sourceDir := t.TempDir()
	runGitWS(t, sourceDir, "init")
	runGitWS(t, sourceDir, "checkout", "-b", "main")
	runGitWS(t, sourceDir, "config", "user.email", "test@test.com")
	runGitWS(t, sourceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "base")
	runGitWS(t, sourceDir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, sourceDir, "push", gitOriginRemote, "main")
	runGitWS(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGitWS(t, sourceDir, "checkout", "-b", workspaceTestFeatureBranch)
	largeReadme := "base\n" + strings.Repeat("feature line\n", 200)
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte(largeReadme), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, sourceDir, "add", ".")
	runGitWS(t, sourceDir, "commit", "-m", "large feature")
	runGitWS(t, sourceDir, "push", gitOriginRemote, workspaceTestFeatureBranch)

	cloneDir := filepath.Join(t.TempDir(), "clone")
	runGitWS(t, t.TempDir(),
		"clone", "--branch", workspaceTestFeatureBranch, "--single-branch", "file://"+bareDir, cloneDir,
	)
	if err := PreparePullRequestReviewContext(cloneDir, &AgentConfig{PRBaseBranch: "main"}); err != nil {
		t.Fatalf("PreparePullRequestReviewContext() error = %v", err)
	}

	diff, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewDiffPath))
	if err != nil {
		t.Fatalf("ReadFile(diff) error = %v", err)
	}
	if !strings.Contains(string(diff), "Orka truncated this diff") {
		t.Fatalf("diff = %q, want truncation notice", string(diff))
	}
	instructions, err := os.ReadFile(filepath.Join(cloneDir, pullRequestReviewInstructionsPath))
	if err != nil {
		t.Fatalf("ReadFile(instructions) error = %v", err)
	}
	if !strings.Contains(string(instructions), "diff truncated at 256 bytes") {
		t.Fatalf("instructions = %q, want truncation note", string(instructions))
	}
}

func TestPrepareWorkspace_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error")) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("ORKA_PRIOR_TASK", "task-1")
	t.Setenv("ORKA_PRIOR_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)

	err := PrepareWorkspace("/tmp/test")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error should mention HTTP 500, got: %v", err)
	}
}

func TestPrepareWorkspace_WithDiff(t *testing.T) {
	// Set up a git repo
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	// Generate a diff
	if err := os.WriteFile(dir+"/file.txt", []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "diff")
	cmd.Dir = dir
	diffBytes, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	// Reset back to original
	runGitWS(t, dir, "checkout", "--", ".")

	// Get current HEAD
	headCmd := exec.Command("git", "rev-parse", "HEAD")
	headCmd.Dir = dir
	headOut, err := headCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(headOut))

	sr := StructuredResult{
		Version: 1,
		Summary: "made changes",
		BaseSHA: baseSHA,
		Diff:    string(diffBytes),
	}
	resultJSON, _ := json.Marshal(sr)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"result": string(resultJSON)}) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("ORKA_PRIOR_TASK", "task-1")
	t.Setenv("ORKA_PRIOR_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)

	err = PrepareWorkspace(dir)
	if err != nil {
		t.Fatalf("PrepareWorkspace failed: %v", err)
	}

	// Verify the diff was applied
	content, err := os.ReadFile(dir + "/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "modified\n" {
		t.Errorf("expected modified content, got %q", string(content))
	}
}

func TestPrepareWorkspace_NamespaceFallback(t *testing.T) {
	sr := StructuredResult{Version: 1, Summary: "done"}
	resultJSON, _ := json.Marshal(sr)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the namespace comes from ORKA_TASK_NAMESPACE fallback
		if !strings.Contains(r.URL.String(), "namespace=fallback-ns") {
			t.Errorf("expected namespace=fallback-ns in URL, got %s", r.URL.String())
		}
		json.NewEncoder(w).Encode(map[string]string{"result": string(resultJSON)}) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("ORKA_PRIOR_TASK", "task-1")
	t.Setenv("ORKA_PRIOR_TASK_NAMESPACE", "")      // not set
	t.Setenv("ORKA_TASK_NAMESPACE", "fallback-ns") // fallback
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)

	err := PrepareWorkspace("/tmp/test")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinalizeResult_TruncatesLongSummary(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	longSummary := strings.Repeat("x", MaxStructuredSummaryChars+128)
	data, err := FinalizeResult(dir, longSummary)
	if err != nil {
		t.Fatalf("FinalizeResult failed: %v", err)
	}

	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("expected JSON result, got: %s", string(data))
	}
	if len(sr.Summary) >= len(longSummary) {
		t.Fatalf("summary was not truncated: got %d want less than %d", len(sr.Summary), len(longSummary))
	}
	if !strings.Contains(sr.Summary, "summary truncated") {
		t.Fatalf("summary missing truncation marker: %q", sr.Summary)
	}
}

func TestFinalizeResult_GitRepoWithChanges(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/initial.txt", []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	// Make a change
	if err := os.WriteFile(dir+"/new-file.txt", []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := FinalizeResult(dir, "agent output")
	if err != nil {
		t.Fatalf("FinalizeResult failed: %v", err)
	}

	// Should return structured JSON with diff
	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("expected JSON result, got: %s", string(data))
	}
	if sr.Summary != "agent output" {
		t.Errorf("Summary = %q, want 'agent output'", sr.Summary)
	}
	if sr.Diff == "" {
		t.Error("expected non-empty diff")
	}
	if sr.BaseSHA == "" {
		t.Error("expected non-empty BaseSHA")
	}
}

func TestFinalizeResult_GitRepoNoChanges(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	// No changes made
	data, err := FinalizeResult(dir, "no changes needed")
	if err != nil {
		t.Fatalf("FinalizeResult failed: %v", err)
	}

	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("expected JSON result, got: %s", string(data))
	}
	if sr.Diff != "" {
		t.Errorf("expected empty diff for no changes, got %q", sr.Diff)
	}
}

func TestFinalizeResult_IgnoresWorkspaceArtifactsSymlink(t *testing.T) {
	prepareArtifactsDir(t)

	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	if err := EnsureWorkspaceArtifactsLink(dir); err != nil {
		t.Fatalf("EnsureWorkspaceArtifactsLink() error = %v", err)
	}
	writeArtifactFile(t, "security-threat-model.md", []byte("# threat model\n"))

	data, err := FinalizeResult(dir, "scan complete")
	if err != nil {
		t.Fatalf("FinalizeResult() error = %v", err)
	}

	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("expected JSON result, got: %s", string(data))
	}
	if sr.Diff != "" {
		t.Fatalf("expected empty diff, got %q", sr.Diff)
	}
	if len(sr.Files) != 0 {
		t.Fatalf("expected no changed files, got %v", sr.Files)
	}
}

func TestFinalizeResult_IncludesUserSkillsDirectory(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	skillDir := filepath.Join(dir, ".skills", "example")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Generated skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := FinalizeResult(dir, "skills only")
	if err != nil {
		t.Fatalf("FinalizeResult() error = %v", err)
	}

	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("expected JSON result, got: %s", string(data))
	}
	if !strings.Contains(sr.Diff, ".skills/example/SKILL.md") {
		t.Fatalf("expected .skills user edit in diff, got %q", sr.Diff)
	}
}

func TestFinalizeResult_PushBranchNoRemote(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	// Make a change
	if err := os.WriteFile(dir+"/new.txt", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Set push branch but no remote — push will fail gracefully
	t.Setenv("ORKA_PUSH_BRANCH", workspaceTestFeatureBranch)

	data, err := FinalizeResult(dir, "agent output")
	if err != nil {
		t.Fatalf("FinalizeResult failed: %v", err)
	}

	// Should still produce a structured result despite push failure
	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("expected JSON result, got: %s", string(data))
	}
	if sr.Diff == "" {
		t.Error("expected diff in result")
	}
	if sr.PushBranch != "" {
		t.Errorf("PushBranch = %q, want empty when push fails", sr.PushBranch)
	}
	if !strings.Contains(sr.PushError, "git push failed") {
		t.Errorf("PushError = %q, want git push failure", sr.PushError)
	}
}

func TestFinalizeResult_PushBranchWithRemote(t *testing.T) {
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "checkout", "-b", "main")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")
	runGitWS(t, dir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, "main")

	if err := os.WriteFile(dir+"/new.txt", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORKA_PUSH_BRANCH", workspaceTestFeatureBranch)

	data, err := FinalizeResult(dir, "agent output")
	if err != nil {
		t.Fatalf("FinalizeResult failed: %v", err)
	}

	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("expected JSON result, got: %s", string(data))
	}
	if sr.PushBranch != workspaceTestFeatureBranch {
		t.Errorf("PushBranch = %q, want %s", sr.PushBranch, workspaceTestFeatureBranch)
	}
	if sr.PushError != "" {
		t.Errorf("PushError = %q, want empty", sr.PushError)
	}
}

func TestFinalizeResult_RequirePushBranchNoRemote(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	if err := os.WriteFile(dir+"/new.txt", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORKA_PUSH_BRANCH", workspaceTestFeatureBranch)
	t.Setenv(requirePushBranchEnvVar, "true")

	_, err := FinalizeResult(dir, "agent output")
	if err == nil {
		t.Fatal("expected push-required finalize to fail without a remote")
	}
	if !strings.Contains(err.Error(), "failed to push to "+workspaceTestFeatureBranch) {
		t.Fatalf("expected push failure error, got: %v", err)
	}
}

func TestFinalizeResult_RequirePushBranchWithoutWorkspaceDiff(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	t.Setenv("ORKA_PUSH_BRANCH", workspaceTestFeatureBranch)
	t.Setenv(requirePushBranchEnvVar, "true")

	_, err := FinalizeResult(dir, "agent output")
	if err == nil {
		t.Fatal("expected push-required finalize to fail without a workspace diff")
	}
	if !strings.Contains(err.Error(), "no workspace diff was produced") {
		t.Fatalf("expected no-diff error, got: %v", err)
	}
}

func TestFinalizeResult_RequirePushBranchAllowsEmptyWorkspaceDiff(t *testing.T) {
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "checkout", "-b", "main")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")
	runGitWS(t, dir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, "main")
	runGitWS(t, dir, "checkout", "-b", workspaceTestFeatureBranch)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, workspaceTestFeatureBranch)

	t.Setenv("ORKA_PUSH_BRANCH", workspaceTestFeatureBranch)
	t.Setenv(requirePushBranchEnvVar, "true")
	t.Setenv(workerenv.AllowEmptyPushBranch, "true")
	t.Setenv(workerenv.PRBaseBranch, "main")

	data, err := FinalizeResult(dir, "already up to date")
	if err != nil {
		t.Fatalf("FinalizeResult failed: %v", err)
	}
	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("unmarshal structured result: %v", err)
	}
	if sr.PushBranch != workspaceTestFeatureBranch {
		t.Errorf("PushBranch = %q, want %s", sr.PushBranch, workspaceTestFeatureBranch)
	}
	if sr.PushError != "" {
		t.Errorf("PushError = %q, want empty", sr.PushError)
	}
}

func TestFinalizeResult_RequirePushBranchAllowsEmptyWorkspaceDiffRejectsStaleHead(t *testing.T) {
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "checkout", "-b", "main")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")
	runGitWS(t, dir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, "main")
	runGitWS(t, dir, "checkout", "-b", workspaceTestFeatureBranch)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, workspaceTestFeatureBranch)

	runGitWS(t, dir, "checkout", "main")
	runGitWS(t, dir, "commit", "--allow-empty", "-m", "advance base")
	runGitWS(t, dir, "push", gitOriginRemote, "main")
	runGitWS(t, dir, "checkout", workspaceTestFeatureBranch)

	t.Setenv("ORKA_PUSH_BRANCH", workspaceTestFeatureBranch)
	t.Setenv(requirePushBranchEnvVar, "true")
	t.Setenv(workerenv.AllowEmptyPushBranch, "true")
	t.Setenv(workerenv.PRBaseBranch, "main")

	_, err := FinalizeResult(dir, "stale branch")
	if err == nil {
		t.Fatal("expected stale empty push to fail")
	}
	if !strings.Contains(err.Error(), "does not contain origin/main") {
		t.Fatalf("expected stale base containment error, got: %v", err)
	}
}

func TestFinalizeResult_RequirePushBranchAllowsEmptyWorkspaceDiffPushesAdvancedHead(t *testing.T) {
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "checkout", "-b", "main")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")
	runGitWS(t, dir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, "main")
	runGitWS(t, dir, "commit", "--allow-empty", "-m", "advance head")
	advancedHead := strings.TrimSpace(runGitOutputWS(t, dir, "rev-parse", "HEAD"))

	t.Setenv("ORKA_PUSH_BRANCH", workspaceTestFeatureBranch)
	t.Setenv(requirePushBranchEnvVar, "true")
	t.Setenv(workerenv.AllowEmptyPushBranch, "true")

	data, err := FinalizeResult(dir, "advanced head")
	if err != nil {
		t.Fatalf("FinalizeResult failed: %v", err)
	}
	var sr StructuredResult
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatalf("unmarshal structured result: %v", err)
	}
	if sr.PushBranch != workspaceTestFeatureBranch {
		t.Errorf("PushBranch = %q, want %s", sr.PushBranch, workspaceTestFeatureBranch)
	}
	remoteHead := strings.TrimSpace(runGitOutputWS(t, bareDir, "rev-parse", "refs/heads/"+workspaceTestFeatureBranch))
	if remoteHead != advancedHead {
		t.Fatalf("remote feature-branch HEAD = %q, want %q", remoteHead, advancedHead)
	}
}

func TestPushChanges_NothingToCommit(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	// No changes — pushChanges should return nil (nothing to commit)
	err := pushChanges(dir, workspaceTestFeatureBranch)
	if err != nil {
		t.Fatalf("pushChanges should succeed with nothing to commit, got: %v", err)
	}
}

func TestPushChanges_NoRemote(t *testing.T) {
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")

	// Make a change
	if err := os.WriteFile(dir+"/new.txt", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Push should fail because there's no remote
	err := pushChanges(dir, workspaceTestFeatureBranch)
	if err == nil {
		t.Fatal("expected error pushing without remote")
	}
	if !strings.Contains(err.Error(), "git push failed") {
		t.Errorf("error should mention git push failed, got: %v", err)
	}
}

func TestPushChanges_WithRemote(t *testing.T) {
	// Create a bare remote
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	// Create a working repo with a remote
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "checkout", "-b", "main")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")
	runGitWS(t, dir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, "main")

	// Make a change
	if err := os.WriteFile(dir+"/new.txt", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origWait := waitForRemoteBranchVisibility
	t.Cleanup(func() { waitForRemoteBranchVisibility = origWait })
	waitCalled := false
	waitForRemoteBranchVisibility = func(workDir, remote, branch string, timeout time.Duration) error {
		waitCalled = true
		if remote != gitOriginRemote {
			t.Fatalf("remote = %q, want origin", remote)
		}
		if branch != workspaceTestFeatureBranch {
			t.Fatalf("branch = %q, want %s", branch, workspaceTestFeatureBranch)
		}
		if timeout <= 0 {
			t.Fatalf("timeout = %v, want > 0", timeout)
		}
		return nil
	}

	err := pushChanges(dir, workspaceTestFeatureBranch)
	if err != nil {
		t.Fatalf("pushChanges failed: %v", err)
	}
	if !waitCalled {
		t.Fatal("expected waitForRemoteBranchVisibility to be called")
	}
}

func TestPushChanges_WithRemoteBranchVisibilityFailure(t *testing.T) {
	// Create a bare remote
	bareDir := t.TempDir()
	runGitWS(t, bareDir, "init", "--bare")

	// Create a working repo with a remote
	dir := t.TempDir()
	runGitWS(t, dir, "init")
	runGitWS(t, dir, "checkout", "-b", "main")
	runGitWS(t, dir, "config", "user.email", "test@test.com")
	runGitWS(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/file.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitWS(t, dir, "add", ".")
	runGitWS(t, dir, "commit", "-m", "initial")
	runGitWS(t, dir, "remote", "add", gitOriginRemote, bareDir)
	runGitWS(t, dir, "push", "-u", gitOriginRemote, "main")

	if err := os.WriteFile(dir+"/new.txt", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origWait := waitForRemoteBranchVisibility
	t.Cleanup(func() { waitForRemoteBranchVisibility = origWait })
	waitForRemoteBranchVisibility = func(workDir, remote, branch string, timeout time.Duration) error {
		return errors.New("branch not visible yet")
	}

	err := pushChanges(dir, workspaceTestFeatureBranch)
	if err == nil {
		t.Fatal("expected pushChanges to fail when remote branch never becomes visible")
	}
	if !strings.Contains(err.Error(), "remote branch "+workspaceTestFeatureBranch+" not visible after push") {
		t.Fatalf("expected visibility failure, got: %v", err)
	}
}

// runGitWS is a test helper to execute git commands in workspace tests.
func runGitWS(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func runGitOutputWS(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}
