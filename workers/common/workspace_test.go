/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
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
	t.Setenv("ORKA_PUSH_BRANCH", "feature-branch")

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
	err := pushChanges(dir, "feature-branch")
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
	err := pushChanges(dir, "feature-branch")
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
	runGitWS(t, dir, "remote", "add", "origin", bareDir)
	runGitWS(t, dir, "push", "-u", "origin", "main")

	// Make a change
	if err := os.WriteFile(dir+"/new.txt", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := pushChanges(dir, "feature-branch")
	if err != nil {
		t.Fatalf("pushChanges failed: %v", err)
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
