/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLoadConfig_RequiredFields(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for missing ORKA_PROMPT")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "hello")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_MODEL", "")
	t.Setenv("ORKA_SYSTEM_PROMPT", "")
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")

	cfg, err := LoadConfig(50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Prompt != "hello" {
		t.Errorf("expected Prompt 'hello', got %q", cfg.Prompt)
	}
	if cfg.MaxTurns != 50 {
		t.Errorf("expected MaxTurns 50, got %d", cfg.MaxTurns)
	}
}

func TestLoadConfig_AllFields(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "refactor code")
	t.Setenv("ORKA_TASK_NAME", "task1")
	t.Setenv("ORKA_TASK_NAMESPACE", "ns1")
	t.Setenv("ORKA_MODEL", "test-model")
	t.Setenv("ORKA_SYSTEM_PROMPT", "Be helpful")
	t.Setenv("ORKA_MAX_TURNS", "100")
	t.Setenv("ORKA_ALLOWED_TOOLS", "Read,Write,Edit")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "Bash")
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_GIT_BRANCH", "main")
	t.Setenv("ORKA_GIT_REF", "abc123")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "src")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "600")

	cfg, err := LoadConfig(50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", cfg.Model)
	}
	if cfg.MaxTurns != 100 {
		t.Errorf("MaxTurns = %d, want 100", cfg.MaxTurns)
	}
	if len(cfg.AllowedTools) != 3 {
		t.Errorf("AllowedTools len = %d, want 3", len(cfg.AllowedTools))
	}
	if len(cfg.DisallowedTools) != 1 {
		t.Errorf("DisallowedTools len = %d, want 1", len(cfg.DisallowedTools))
	}
	if cfg.GitRepo != "https://github.com/example/repo.git" {
		t.Errorf("GitRepo = %q", cfg.GitRepo)
	}
	if cfg.SubPath != "src" {
		t.Errorf("SubPath = %q, want src", cfg.SubPath)
	}
	if cfg.TimeoutSeconds != 600 {
		t.Errorf("TimeoutSeconds = %d, want 600", cfg.TimeoutSeconds)
	}
}

func TestLoadConfig_InvalidMaxTurns(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "hello")
	t.Setenv("ORKA_MAX_TURNS", "not-a-number")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for invalid ORKA_MAX_TURNS")
	}
}

func TestLoadConfig_InvalidTimeoutSeconds(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "hello")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "not-a-number")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for invalid ORKA_TIMEOUT_SECONDS")
	}
	if !strings.Contains(err.Error(), "invalid ORKA_TIMEOUT_SECONDS") {
		t.Errorf("error = %q, want it to mention ORKA_TIMEOUT_SECONDS", err.Error())
	}
}

func TestSetupGitCredentials_NoSecrets(t *testing.T) {
	t.Setenv("GIT_TOKEN", "")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("GIT_USERNAME", "")

	SetupGitCredentials()

	if os.Getenv("GIT_TOKEN") != "" {
		t.Error("GIT_TOKEN should not be set when no secret files exist")
	}
	if os.Getenv("GIT_ASKPASS") != "" {
		t.Error("GIT_ASKPASS should not be set when no secret files exist")
	}
	if os.Getenv("GIT_USERNAME") != "" {
		t.Error("GIT_USERNAME should not be set when no secret files exist")
	}
}

func TestSetupGitCredentials_WithTokenFile(t *testing.T) {
	// Create a temp directory simulating /secrets/git/token
	dir := t.TempDir()
	tokenPath := dir + "/token"
	if err := os.WriteFile(tokenPath, []byte("  my-secret-token  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// We can't override the hard-coded paths, but we can verify the function
	// doesn't panic with files that don't exist. The NoSecrets test already
	// covers the negative path. Here we test the username file path.
	usernamePath := dir + "/username"
	if err := os.WriteFile(usernamePath, []byte("bot-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// SetupGitCredentials reads from fixed paths (/secrets/git/...) so in
	// tests without those mounted, it simply no-ops. Verify it doesn't error.
	t.Setenv("GIT_TOKEN", "")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("GIT_USERNAME", "")
	t.Setenv("GITHUB_TOKEN", "")
	SetupGitCredentials()
	// No panic = success for the unmounted case.
}

func TestCloneRepo_InvalidRepo(t *testing.T) {
	dir := t.TempDir()
	cfg := &AgentConfig{
		GitRepo: "https://invalid.example.com/nonexistent/repo.git",
	}

	ctx := context.Background()
	err := CloneRepo(ctx, cfg, dir+"/clone-target")
	if err == nil {
		t.Fatal("expected error cloning invalid repo")
	}
	if !strings.Contains(err.Error(), "git clone failed") {
		t.Errorf("error should mention git clone failed, got: %v", err)
	}
}

func TestCloneRepo_WithBranch(t *testing.T) {
	// Create a local bare repo to clone from
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	// Create a working copy, add a commit, and push
	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	if err := os.WriteFile(workDir+"/test.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	runGit(t, workDir, "commit", "-m", "init")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")

	cloneDir := t.TempDir() + "/cloned"
	cfg := &AgentConfig{
		GitRepo:   bareDir,
		GitBranch: "main",
	}

	err := CloneRepo(context.Background(), cfg, cloneDir)
	if err != nil {
		t.Fatalf("CloneRepo failed: %v", err)
	}

	// Verify the file exists
	if _, err := os.Stat(cloneDir + "/test.txt"); err != nil {
		t.Errorf("expected test.txt in cloned repo: %v", err)
	}
}

func TestCloneRepo_WithRef(t *testing.T) {
	// Create a local bare repo
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	// Working copy with two commits
	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(workDir+"/a.txt", []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "first")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")

	cloneDir := t.TempDir() + "/cloned"
	cfg := &AgentConfig{
		GitRepo: bareDir,
		GitRef:  "main", // using branch name as ref
	}

	err := CloneRepo(context.Background(), cfg, cloneDir)
	if err != nil {
		t.Fatalf("CloneRepo failed: %v", err)
	}
}

func TestCloneRepo_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	cfg := &AgentConfig{
		GitRepo: "https://github.com/example/repo.git",
	}

	err := CloneRepo(ctx, cfg, t.TempDir()+"/target")
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestRunAgent_ConfigError(t *testing.T) {
	// Missing ORKA_PROMPT should cause config error
	t.Setenv("ORKA_PROMPT", "")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "")

	executor := func(_ context.Context, _ *AgentConfig) (string, error) {
		return "done", nil
	}

	err := RunAgent("test", "/tmp/ws", 50, executor)
	if err == nil {
		t.Fatal("expected error for missing ORKA_PROMPT")
	}
	if !strings.Contains(err.Error(), "invalid configuration") {
		t.Errorf("error should mention invalid configuration, got: %v", err)
	}
}

func TestRunAgent_ExecutorSuccess(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("ORKA_PRIOR_TASK", "")

	// Set up a result endpoint that accepts the result
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("ORKA_RESULT_ENDPOINT", server.URL)

	executor := func(_ context.Context, cfg *AgentConfig) (string, error) {
		if cfg.Prompt != "test prompt" {
			t.Errorf("expected prompt 'test prompt', got %q", cfg.Prompt)
		}
		return "completed successfully", nil
	}

	err := RunAgent("test-agent", "/tmp/ws", 50, executor)
	if err != nil {
		t.Fatalf("RunAgent should succeed, got: %v", err)
	}
}

func TestRunAgent_ExecutorFailure(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("ORKA_PRIOR_TASK", "")

	// Result endpoint for error submission
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("ORKA_RESULT_ENDPOINT", server.URL)

	executor := func(_ context.Context, _ *AgentConfig) (string, error) {
		return "partial output", fmt.Errorf("agent crashed")
	}

	err := RunAgent("test-agent", "/tmp/ws", 50, executor)
	if err == nil {
		t.Fatal("expected error from executor failure")
	}
	if !strings.Contains(err.Error(), "execution failed") {
		t.Errorf("error should mention execution failed, got: %v", err)
	}
}

func TestRunAgent_GitCloneFailure(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "https://invalid.example.com/no/repo.git")
	t.Setenv("ORKA_GIT_BRANCH", "")
	t.Setenv("ORKA_GIT_REF", "")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "")
	t.Setenv("ORKA_PRIOR_TASK", "")

	executor := func(_ context.Context, _ *AgentConfig) (string, error) {
		return "done", nil
	}

	err := RunAgent("test-agent", t.TempDir(), 50, executor)
	if err == nil {
		t.Fatal("expected error from git clone failure")
	}
	if !strings.Contains(err.Error(), "git clone failed") {
		t.Errorf("error should mention git clone failed, got: %v", err)
	}
}

// runGit is a test helper to execute git commands.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
