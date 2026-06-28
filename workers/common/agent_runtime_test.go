/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/tracing/testutil"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

const (
	testAgentSandboxTemplateNamespace = "template-ns"
	testTaskNamespace                 = "task-ns"
	executorResultDone                = "done"
)

func setBasicRunAgentEnv(t *testing.T, taskName string) {
	t.Helper()
	t.Setenv(workerenv.Prompt, "test prompt")
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.MaxTurns, "")
	t.Setenv(workerenv.AllowedTools, "")
	t.Setenv(workerenv.DisallowedTools, "")
	t.Setenv(workerenv.TimeoutSeconds, "")
	t.Setenv(workerenv.GitRepo, "")
	t.Setenv(workerenv.PriorTask, "")
	t.Setenv(workerenv.ControllerURL, "")
	t.Setenv(workerenv.ResultEndpoint, "")
}

func setRunAgentEventRecorderForTest(recorder EventRecorder) func() {
	previous := newEventRecorderFromEnv
	newEventRecorderFromEnv = func() EventRecorder { return recorder }
	return func() { newEventRecorderFromEnv = previous }
}

func assertRecordedEventTypes(t *testing.T, got []string, want []string) {
	t.Helper()
	seen := make(map[string]bool, len(got))
	for _, typ := range got {
		seen[typ] = true
	}
	for _, typ := range want {
		if !seen[typ] {
			t.Fatalf("event types %v missing %s", got, typ)
		}
	}
}

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
	t.Setenv(workerenv.TransactionID, "txn-123")
	t.Setenv(workerenv.TransactionProfile, "kontxt")
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
	if cfg.TransactionID != "txn-123" || cfg.TransactionProfile != "kontxt" {
		t.Errorf("transaction fields = %q/%q, want txn-123/kontxt", cfg.TransactionID, cfg.TransactionProfile)
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

// When ORKA_PUSH_BRANCH is set, CloneRepo must pre-checkout a local branch
// with that name so an agent-initiated `git push origin HEAD` lands on the
// intended remote branch rather than overwriting "main". This prevents the
// production bug we hit on sozercan/vekil where the Codex agent committed
// and pushed inside its own loop, landing on main and breaking the worker's
// post-run push to ORKA_PUSH_BRANCH.
func TestCloneRepo_PreChecksOutPushBranchFromEnv(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

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

	t.Setenv(workerenv.PushBranch, "orka/feature-branch")
	cloneDir := t.TempDir() + "/cloned"
	cfg := &AgentConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("CloneRepo failed: %v", err)
	}

	branch, err := exec.Command("git", "-C", cloneDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse failed: %v", err)
	}
	got := strings.TrimSpace(string(branch))
	if got != "orka/feature-branch" {
		t.Errorf("HEAD branch = %q, want %q (pre-checkout did not fire)", got, "orka/feature-branch")
	}
}

// When ORKA_PUSH_BRANCH is unset, CloneRepo must NOT alter the checked-out
// branch. Tasks that only read the workspace (validation, discovery) rely on
// HEAD remaining on the cloned branch.
func TestCloneRepo_NoPushBranchLeavesHEADAlone(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

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

	t.Setenv(workerenv.PushBranch, "")
	cloneDir := t.TempDir() + "/cloned"
	cfg := &AgentConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("CloneRepo failed: %v", err)
	}

	branch, err := exec.Command("git", "-C", cloneDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse failed: %v", err)
	}
	got := strings.TrimSpace(string(branch))
	if got != "main" {
		t.Errorf("HEAD branch = %q, want main (no pushBranch should leave HEAD alone)", got)
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
	gotBranch := strings.TrimSpace(runGitOutput(t, cloneDir, "branch", "--show-current"))
	if gotBranch != "main" {
		t.Fatalf("branch = %q, want main", gotBranch)
	}
}

func TestCloneRepo_WithCommitRefFromNonDefaultBranch(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(workDir+"/main.txt", []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "main")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGit(t, workDir, "checkout", "-b", "feature/validation")
	if err := os.WriteFile(workDir+"/feature.txt", []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "feature")
	featureSHA := strings.TrimSpace(runGitOutput(t, workDir, "rev-parse", "HEAD"))
	runGit(t, workDir, "push", "origin", "feature/validation")

	cloneDir := t.TempDir() + "/cloned"
	cfg := &AgentConfig{
		GitRepo: bareDir,
		GitRef:  featureSHA,
	}

	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("CloneRepo failed: %v", err)
	}

	gotSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))
	if gotSHA != featureSHA {
		t.Fatalf("HEAD = %s, want feature SHA %s", gotSHA, featureSHA)
	}
	if _, err := os.Stat(cloneDir + "/feature.txt"); err != nil {
		t.Errorf("expected feature.txt from non-default branch commit: %v", err)
	}
}

func TestCloneRepo_ReusedWorkspaceFastForwardsCheckedOutBranch(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "version.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "v1")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	cfg := &AgentConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workDir, "version.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "v2")
	wantSHA := strings.TrimSpace(runGitOutput(t, workDir, "rev-parse", "HEAD"))
	runGit(t, workDir, "push", "origin", "main")

	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("reused CloneRepo failed: %v", err)
	}

	gotSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))
	if gotSHA != wantSHA {
		t.Fatalf("HEAD = %s, want remote main %s", gotSHA, wantSHA)
	}
	data, err := os.ReadFile(filepath.Join(cloneDir, "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v2" {
		t.Fatalf("version.txt = %q, want v2", data)
	}
}

func TestCloneRepo_ReusedWorkspacePreservesSessionBranch(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "version.txt"), []byte("main-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "main v1")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	cfg := &AgentConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}
	runGit(t, cloneDir, "checkout", "-b", "demo/sandbox-metrics")
	if err := os.WriteFile(filepath.Join(cloneDir, "session.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, cloneDir, "add", ".")
	runGit(t, cloneDir, "config", "user.email", "test@test.com")
	runGit(t, cloneDir, "config", "user.name", "Test")
	runGit(t, cloneDir, "commit", "-m", "session work")
	sessionSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(workDir, "version.txt"), []byte("main-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "main v2")
	runGit(t, workDir, "push", "origin", "main")

	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("reused CloneRepo failed: %v", err)
	}

	gotBranch := strings.TrimSpace(runGitOutput(t, cloneDir, "branch", "--show-current"))
	if gotBranch != "demo/sandbox-metrics" {
		t.Fatalf("branch = %q, want session branch", gotBranch)
	}
	gotSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))
	if gotSHA != sessionSHA {
		t.Fatalf("HEAD = %s, want session SHA %s", gotSHA, sessionSHA)
	}
}

func TestCloneRepo_ReusedWorkspaceBranchFetchFailureIsFatal(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "init")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	cfg := &AgentConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}
	missingRepo := filepath.Join(t.TempDir(), "missing.git")
	runGit(t, cloneDir, "remote", "set-url", "origin", missingRepo)
	cfg.GitRepo = missingRepo

	err := CloneRepo(context.Background(), cfg, cloneDir)
	if err == nil {
		t.Fatal("expected reused CloneRepo to fail when branch fetch fails")
	}
	if !strings.Contains(err.Error(), "git fetch branch \"main\" on reused workspace failed") {
		t.Fatalf("error = %q, want branch fetch failure", err)
	}
}

func TestCloneRepo_ReusedWorkspaceChecksOutRef(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.txt"), []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "main")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGit(t, workDir, "checkout", "-b", "feature/reused")
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "feature")
	featureSHA := strings.TrimSpace(runGitOutput(t, workDir, "rev-parse", "HEAD"))
	runGit(t, workDir, "push", "origin", "feature/reused")

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	if err := CloneRepo(context.Background(), &AgentConfig{GitRepo: bareDir, GitBranch: "main"}, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}

	if err := CloneRepo(context.Background(), &AgentConfig{GitRepo: bareDir, GitRef: featureSHA}, cloneDir); err != nil {
		t.Fatalf("reused CloneRepo with ref failed: %v", err)
	}

	gotSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))
	if gotSHA != featureSHA {
		t.Fatalf("HEAD = %s, want feature SHA %s", gotSHA, featureSHA)
	}
	if _, err := os.Stat(filepath.Join(cloneDir, "feature.txt")); err != nil {
		t.Errorf("expected feature.txt from reused ref checkout: %v", err)
	}
}

func TestCloneRepo_ReusedWorkspaceChecksOutBranchRef(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.txt"), []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "main")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGit(t, workDir, "checkout", "-b", "feature/reused")
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "feature")
	wantSHA := strings.TrimSpace(runGitOutput(t, workDir, "rev-parse", "HEAD"))
	runGit(t, workDir, "push", "origin", "feature/reused")

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	if err := CloneRepo(context.Background(), &AgentConfig{GitRepo: bareDir, GitBranch: "main"}, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}

	featureCfg := &AgentConfig{GitRepo: bareDir, GitRef: "feature/reused"}
	if err := CloneRepo(context.Background(), featureCfg, cloneDir); err != nil {
		t.Fatalf("reused CloneRepo with branch ref failed: %v", err)
	}

	gotBranch := strings.TrimSpace(runGitOutput(t, cloneDir, "branch", "--show-current"))
	if gotBranch != "feature/reused" {
		t.Fatalf("branch = %q, want feature/reused", gotBranch)
	}
	gotSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))
	if gotSHA != wantSHA {
		t.Fatalf("HEAD = %s, want feature branch SHA %s", gotSHA, wantSHA)
	}
}

func TestCloneRepo_ReusedWorkspaceRejectsUnresolvedRef(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.txt"), []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "main")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	if err := CloneRepo(context.Background(), &AgentConfig{GitRepo: bareDir, GitBranch: "main"}, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}
	runGit(t, cloneDir, "checkout", "-b", "missing/ref")
	runGit(t, cloneDir, "config", "user.email", "test@test.com")
	runGit(t, cloneDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(cloneDir, "stale.txt"), []byte("stale local branch"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, cloneDir, "add", ".")
	runGit(t, cloneDir, "commit", "-m", "stale local branch")
	runGit(t, cloneDir, "checkout", "main")
	startSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))

	err := CloneRepo(context.Background(), &AgentConfig{GitRepo: bareDir, GitRef: "missing/ref"}, cloneDir)
	if err == nil {
		t.Fatal("expected reused CloneRepo with unresolved ref to fail")
	}
	if !strings.Contains(err.Error(), `git checkout ref "missing/ref" failed`) {
		t.Fatalf("error = %q, want unresolved ref checkout failure", err)
	}
	gotSHA := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD"))
	if gotSHA != startSHA {
		t.Fatalf("HEAD = %s, want unchanged SHA %s", gotSHA, startSHA)
	}
}

func TestCloneRepo_ReusedWorkspaceRejectsDifferentRemote(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")
	otherBareDir := t.TempDir()
	runGit(t, otherBareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "main.txt"), []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "main")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	if err := CloneRepo(context.Background(), &AgentConfig{GitRepo: bareDir, GitBranch: "main"}, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}

	err := CloneRepo(context.Background(), &AgentConfig{GitRepo: otherBareDir, GitBranch: "main"}, cloneDir)
	if err == nil {
		t.Fatal("expected reused CloneRepo with different remote to fail")
	}
	if !strings.Contains(err.Error(), "existing git remote origin does not match configured repo") {
		t.Fatalf("error = %q, want remote mismatch failure", err)
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

func TestGitSafeDirectoryArgs(t *testing.T) {
	dir := t.TempDir()

	args := gitSafeDirectoryArgs(dir, "status", "--short")
	if len(args) != 6 {
		t.Fatalf("len(args) = %d, want 6", len(args))
	}
	if args[0] != "-c" {
		t.Fatalf("args[0] = %q, want -c", args[0])
	}
	if !strings.HasPrefix(args[1], "safe.directory=") {
		t.Fatalf("args[1] = %q, want safe.directory=...", args[1])
	}
	if args[2] != "-c" || args[3] != "core.hooksPath=/dev/null" {
		t.Fatalf("hook args = %v, want [-c core.hooksPath=/dev/null]", args[2:4])
	}
	if args[4] != "status" || args[5] != "--short" {
		t.Fatalf("tail args = %v, want [status --short]", args[4:])
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
		return executorResultDone, nil
	}

	err := RunAgent("test", "/tmp/ws", 50, executor)
	if err == nil {
		t.Fatal("expected error for missing ORKA_PROMPT")
	}
	if !strings.Contains(err.Error(), "invalid configuration") {
		t.Errorf("error should mention invalid configuration, got: %v", err)
	}
}

func TestRunAgentConfigErrorEmitsWorkerFailedEvent(t *testing.T) {
	t.Setenv(workerenv.Prompt, "")
	t.Setenv(workerenv.TaskName, "config-error-task")
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.MaxTurns, "")
	t.Setenv(workerenv.AllowedTools, "")
	t.Setenv(workerenv.DisallowedTools, "")
	t.Setenv(workerenv.TimeoutSeconds, "")
	t.Setenv(workerenv.GitRepo, "")

	recorder := NewFakeEventRecorder()
	restoreRecorder := setRunAgentEventRecorderForTest(recorder)
	defer restoreRecorder()

	err := RunAgent("codex", t.TempDir(), 50, func(context.Context, *AgentConfig) (string, error) {
		return "should not run", nil
	})
	if err == nil {
		t.Fatal("RunAgent() error = nil, want config error")
	}
	if got := recorder.EventTypes(); !reflect.DeepEqual(got, []string{events.ExecutionEventTypeWorkerFailed}) {
		t.Fatalf("event types = %v, want WorkerFailed", got)
	}
	captured := recorder.Events()[0]
	if captured.TaskName != "config-error-task" || !strings.Contains(captured.Summary, "invalid configuration") {
		t.Fatalf("captured event = %#v, want task name and config error summary", captured)
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

func TestRunAgentEmitsTaskRunSpanFromTraceparent(t *testing.T) {
	if _, err := tracing.Init("test", false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	spans := testutil.NewSpanHarness(t)
	parentCtx, parentSpan := tracing.Tracer("test").Start(context.Background(), "parent")
	carrier := tracing.InjectContext(parentCtx)
	t.Setenv(workerenv.TraceParent, carrier.Get("traceparent"))
	t.Setenv(workerenv.AgentName, "agent-a")
	setBasicRunAgentEnv(t, "trace-task")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(workerenv.ResultEndpoint, server.URL)
	restoreRecorder := setRunAgentEventRecorderForTest(NewFakeEventRecorder())
	defer restoreRecorder()

	if err := RunAgent("codex", t.TempDir(), 50, func(context.Context, *AgentConfig) (string, error) {
		return "done", nil
	}); err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	parentSpan.End()
	taskRun := testutil.SpanNamed(spans.Recorder.Ended(), "task.run")
	if taskRun == nil {
		t.Fatal("missing task.run span")
	}
	if got, want := taskRun.Parent().SpanID(), parentSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("task.run parent = %s, want %s", got, want)
	}
	attrs := testutil.AttributeMap(taskRun)
	if got := attrs[tracing.AttrTaskID].AsString(); got != "trace-task" {
		t.Fatalf("%s = %q", tracing.AttrTaskID, got)
	}
	if got := attrs[tracing.AttrAgentName].AsString(); got != "agent-a" {
		t.Fatalf("%s = %q", tracing.AttrAgentName, got)
	}
}

func TestRunAgentEmitsLifecycleEvents(t *testing.T) {
	setBasicRunAgentEnv(t, "event-task")
	recorder := NewFakeEventRecorder()
	restoreRecorder := setRunAgentEventRecorderForTest(recorder)
	defer restoreRecorder()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(workerenv.ResultEndpoint, server.URL)
	t.Setenv(workerenv.ControllerURL, server.URL)

	err := RunAgent("codex", t.TempDir(), 50, func(_ context.Context, cfg *AgentConfig) (string, error) {
		if cfg.Prompt != "test prompt" {
			t.Fatalf("prompt = %q, want test prompt", cfg.Prompt)
		}
		return "completed successfully", nil
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	assertRecordedEventTypes(t, recorder.EventTypes(), []string{
		events.ExecutionEventTypeWorkerStarted,
		events.ExecutionEventTypeWorkspacePreparationStarted,
		events.ExecutionEventTypeWorkspacePreparationCompleted,
		events.ExecutionEventTypeAgentRuntimeStarted,
		events.ExecutionEventTypeAgentRuntimeCommandStarted,
		events.ExecutionEventTypeAgentRuntimeCompleted,
		events.ExecutionEventTypeResultSubmitted,
		events.ExecutionEventTypeWorkerCompleted,
	})
	var sawRuntimeCommand bool
	for _, event := range recorder.Events() {
		if event.Type == events.ExecutionEventTypeAgentRuntimeCommandStarted && event.AgentName == "codex" {
			sawRuntimeCommand = true
			if strings.Contains(string(event.Content), "test prompt") {
				t.Fatalf("runtime command event leaked prompt text: %s", event.Content)
			}
		}
	}
	if !sawRuntimeCommand {
		t.Fatalf("missing codex runtime command event: %#v", recorder.Events())
	}
}

func TestRunAgentFailureEmitsRuntimeAndWorkerFailedEvents(t *testing.T) {
	setBasicRunAgentEnv(t, "event-fail-task")
	recorder := NewFakeEventRecorder()
	restoreRecorder := setRunAgentEventRecorderForTest(recorder)
	defer restoreRecorder()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(workerenv.ResultEndpoint, server.URL)
	t.Setenv(workerenv.ControllerURL, server.URL)

	err := RunAgent("claude", t.TempDir(), 50, func(context.Context, *AgentConfig) (string, error) {
		return "partial output", fmt.Errorf("agent crashed")
	})
	if err == nil {
		t.Fatal("RunAgent() error = nil, want executor failure")
	}
	assertRecordedEventTypes(t, recorder.EventTypes(), []string{
		events.ExecutionEventTypeAgentRuntimeFailed,
		events.ExecutionEventTypeResultSubmitted,
		events.ExecutionEventTypeWorkerFailed,
	})
}

func TestRunAgentWorkspacePreparationFailedEvent(t *testing.T) {
	setBasicRunAgentEnv(t, "event-workspace-fail-task")
	recorder := NewFakeEventRecorder()
	restoreRecorder := setRunAgentEventRecorderForTest(recorder)
	defer restoreRecorder()
	workspaceDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(workspaceDir, workspaceArtifactsDirName),
		[]byte("not a symlink"),
		0o644,
	); err != nil {
		t.Fatalf("write conflicting artifact path: %v", err)
	}

	err := RunAgent("copilot", workspaceDir, 50, func(context.Context, *AgentConfig) (string, error) {
		return "should not run", nil
	})
	if err == nil {
		t.Fatal("RunAgent() error = nil, want workspace setup failure")
	}
	assertRecordedEventTypes(t, recorder.EventTypes(), []string{
		events.ExecutionEventTypeWorkspacePreparationStarted,
		events.ExecutionEventTypeWorkspacePreparationFailed,
		events.ExecutionEventTypeWorkerFailed,
	})
}

func TestRunAgent_ResultStdoutSubmitsRawExecutorOutputWithGitWorkspace(t *testing.T) {
	workspaceDir := t.TempDir()
	runGit(t, workspaceDir, "init")
	runGit(t, workspaceDir, "config", "user.email", "test@test.com")
	runGit(t, workspaceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workspaceDir, "initial.txt"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspaceDir, "add", ".")
	runGit(t, workspaceDir, "commit", "-m", "initial")

	rawResultBytes, err := json.Marshal(map[string]any{
		"kind":    "typed-review",
		"payload": strings.Repeat("x", MaxStructuredSummaryChars+128),
	})
	if err != nil {
		t.Fatalf("marshal raw result: %v", err)
	}
	rawResult := string(rawResultBytes)

	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_GIT_BRANCH", "")
	t.Setenv("ORKA_GIT_REF", "")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "")
	t.Setenv("ORKA_PRIOR_TASK", "")
	t.Setenv(workerenv.ResultStdout, "true")
	t.Setenv(workerenv.WorkspacePrepared, "true")

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = RunAgent("test-agent", workspaceDir, 50, func(_ context.Context, _ *AgentConfig) (string, error) {
			return rawResult, nil
		})
	})
	if runErr != nil {
		t.Fatalf("RunAgent() error = %v", runErr)
	}

	payload := decodeStdoutResultPayload(t, stdout)
	if string(payload) != rawResult {
		t.Fatalf("stdout result payload was changed, got %d bytes want %d", len(payload), len(rawResult))
	}

	var sr StructuredResult
	if err := json.Unmarshal(payload, &sr); err == nil && sr.Version != 0 {
		t.Fatalf("stdout result payload was wrapped as StructuredResult: %#v", sr)
	}
}

func TestRunAgent_PreparedWorkspaceSkipsPriorTaskPreparation(t *testing.T) {
	workspaceDir := t.TempDir()
	runGit(t, workspaceDir, "init")
	runGit(t, workspaceDir, "config", "user.email", "test@test.com")
	runGit(t, workspaceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workspaceDir, "file.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspaceDir, "add", ".")
	runGit(t, workspaceDir, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(workspaceDir, "file.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var priorResultRequests atomic.Int64
	controllerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/tasks/") {
			priorResultRequests.Add(1)
			http.Error(w, "prior result should not be fetched", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer controllerServer.Close()

	resultServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer resultServer.Close()

	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_GIT_BRANCH", "")
	t.Setenv("ORKA_GIT_REF", "")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "")
	t.Setenv("ORKA_PRIOR_TASK", "prior-task")
	t.Setenv("ORKA_PRIOR_TASK_NAMESPACE", "default")
	t.Setenv(workerenv.ControllerURL, controllerServer.URL)
	t.Setenv(workerenv.ResultEndpoint, resultServer.URL)
	t.Setenv(workerenv.WorkspacePrepared, "true")

	err := RunAgent("test-agent", workspaceDir, 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		content, readErr := os.ReadFile(filepath.Join(workspaceDir, "file.txt"))
		if readErr != nil {
			return "", readErr
		}
		if string(content) != "modified\n" {
			return "", fmt.Errorf("workspace file = %q, want modified", string(content))
		}
		return "prepared workspace used", nil
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if got := priorResultRequests.Load(); got != 0 {
		t.Fatalf("prior result requests = %d, want 0", got)
	}
}

func TestRunAgent_ExistingGitWorkspaceRequiresPreparedMarker(t *testing.T) {
	workspaceDir := t.TempDir()
	runGit(t, workspaceDir, "init")
	runGit(t, workspaceDir, "config", "user.email", "test@test.com")
	runGit(t, workspaceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workspaceDir, "initial.txt"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspaceDir, "add", ".")
	runGit(t, workspaceDir, "commit", "-m", "initial")

	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_GIT_BRANCH", "")
	t.Setenv("ORKA_GIT_REF", "")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "")
	t.Setenv("ORKA_PRIOR_TASK", "")
	t.Setenv(workerenv.WorkspacePrepared, "")

	var executorCalled atomic.Bool
	err := RunAgent("test-agent", workspaceDir, 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		executorCalled.Store(true)
		return executorResultDone, nil
	})
	if err == nil {
		t.Fatal("expected existing git workspace without prepared marker to fail")
	}
	if !strings.Contains(err.Error(), workerenv.WorkspacePrepared) {
		t.Fatalf("RunAgent() error = %q, want %s context", err.Error(), workerenv.WorkspacePrepared)
	}
	if executorCalled.Load() {
		t.Fatal("executor ran against unmarked existing git workspace")
	}
}

func TestRunAgent_ManagedExecutionWorkspaceReusesExistingGitCheckout(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "repo.git")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, bareDir, "init", "--bare")
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "clone", bareDir, ".")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "initial.txt"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "initial")
	runGit(t, workDir, "push", "origin", "HEAD:main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	if err := CloneRepo(context.Background(), &AgentConfig{
		GitRepo:   bareDir,
		GitBranch: "main",
	}, workspaceDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}
	runGit(t, workspaceDir, "checkout", "-B", "stale-retained")
	runGit(t, workspaceDir, "config", "user.email", "test@test.com")
	runGit(t, workspaceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workspaceDir, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspaceDir, "add", ".")
	runGit(t, workspaceDir, "commit", "-m", "stale retained branch")
	hookPath := filepath.Join(workspaceDir, ".git", "hooks", "post-checkout")
	hookMarker := filepath.Join(workspaceDir, "hook-ran")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\ntouch hook-ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspaceDir, "config", "--local", "credential.helper", "!touch credential-ran")
	runGit(t, workspaceDir, "config", "--local", "core.sshCommand", "touch ssh-ran")
	runGit(t, workspaceDir, "config", "--local", "core.worktree", "/tmp")
	runGit(t, workspaceDir, "config", "--local", "includeIf.gitdir:./.path", "../included-config")
	runGit(t, workspaceDir, "config", "--local", "diff.external", "touch diff-ran")
	runGit(t, workspaceDir, "config", "--local", "diff.demo.textconv", "touch textconv-ran")
	runGit(t, workspaceDir, "config", "--local", "remote.origin.pushURL", "https://evil.example/repo.git")
	runGit(t, workspaceDir, "config", "--local", "url.https://evil.example/.pushInsteadOf", "https://github.com/")
	runGit(t, workspaceDir, "config", "--local", "commit.gpgSign", "true")
	runGit(t, workspaceDir, "config", "--local", "gpg.program", "touch gpg-ran")
	runGit(t, workspaceDir, "config", "--local", "extensions.worktreeConfig", "true")
	if err := os.WriteFile(
		filepath.Join(workspaceDir, ".git", "config.worktree"),
		[]byte("[credential]\n\thelper = !touch worktree-config-ran\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	resultServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer resultServer.Close()

	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", bareDir)
	t.Setenv("ORKA_GIT_BRANCH", "main")
	t.Setenv("ORKA_GIT_REF", "")
	t.Setenv(workerenv.PushBranch, "feature/reuse")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "")
	t.Setenv("ORKA_PRIOR_TASK", "")
	t.Setenv(workerenv.ResultEndpoint, resultServer.URL)
	t.Setenv(workerenv.WorkspacePrepared, "")
	t.Setenv(workerenv.ExecutionWorkspaceDepth, "1")
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	if err := os.WriteFile(
		filepath.Join(homeDir, ".gitconfig"),
		[]byte("[credential]\n\thelper = !touch global-credential-ran\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	var executorCalled atomic.Bool
	err := RunAgent("test-agent", workspaceDir, 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		executorCalled.Store(true)
		if _, statErr := os.Stat(filepath.Join(workspaceDir, "stale.txt")); statErr == nil {
			return "", fmt.Errorf("stale retained branch file leaked into push branch")
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		return executorResultDone, nil
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if !executorCalled.Load() {
		t.Fatal("executor did not run against managed reused workspace")
	}
	if _, err := os.Stat(hookMarker); err == nil {
		t.Fatal("managed workspace refresh executed persisted git hook")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat hook marker: %v", err)
	}
	if helper, err := execGitOutputContext(
		context.Background(), workspaceDir, "config", "--local", "--get", "credential.helper",
	); err == nil {
		t.Fatalf("credential.helper remained after managed refresh: %q", helper)
	}
	if sshCommand, err := execGitOutputContext(
		context.Background(), workspaceDir, "config", "--local", "--get", "core.sshCommand",
	); err == nil {
		t.Fatalf("core.sshCommand remained after managed refresh: %q", sshCommand)
	}
	if worktree, err := execGitOutputContext(
		context.Background(), workspaceDir, "config", "--local", "--get", "core.worktree",
	); err == nil {
		t.Fatalf("core.worktree remained after managed refresh: %q", worktree)
	}
	for _, name := range []string{
		"includeIf.gitdir:./.path",
		"diff.external",
		"diff.demo.textconv",
		"remote.origin.pushURL",
		"url.https://evil.example/.pushInsteadOf",
		"commit.gpgSign",
		"gpg.program",
		"extensions.worktreeConfig",
	} {
		if got, err := execGitOutputContext(
			context.Background(), workspaceDir, "config", "--local", "--get", name,
		); err == nil {
			t.Fatalf("%s remained after managed refresh: %q", name, got)
		}
	}
	if got := os.Getenv("GIT_CONFIG_GLOBAL"); got != "/dev/null" {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, want /dev/null", got)
	}
	if got := os.Getenv("GIT_CONFIG_NOSYSTEM"); got != "1" {
		t.Fatalf("GIT_CONFIG_NOSYSTEM = %q, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git", "config.worktree")); err == nil {
		t.Fatal("config.worktree remained after managed refresh")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat config.worktree: %v", err)
	}
}

func TestRunAgent_ExecutorEmptyResultSubmitsPlaceholder(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "test prompt")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("ORKA_PRIOR_TASK", "")

	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("ORKA_RESULT_ENDPOINT", server.URL)

	err := RunAgent("test-agent", "/tmp/ws", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent should succeed, got: %v", err)
	}
	if got := string(body); !strings.Contains(got, "test-agent completed without a final message") {
		t.Fatalf("submitted body = %q, want non-empty placeholder", got)
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
		return executorResultDone, nil
	}

	err := RunAgent("test-agent", t.TempDir(), 50, executor)
	if err == nil {
		t.Fatal("expected error from git clone failure")
	}
	if !strings.Contains(err.Error(), "git clone failed") {
		t.Errorf("error should mention git clone failed, got: %v", err)
	}
}

func TestRunAgent_AgentSandboxExecutesInnerWorkerAndDeletesWorkspace(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	t.Setenv("SANDBOX_TEST_ENV", "outer-value")
	t.Setenv(workerenv.ServiceAccountToken, "outer-token")

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)
	eventRecorder := NewFakeEventRecorder()
	restoreEventRecorder := setRunAgentEventRecorderForTest(eventRecorder)
	t.Cleanup(restoreEventRecorder)

	workspaceDir := "/sandbox/workspace"
	err := RunAgent("test-agent", workspaceDir, 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete")
	if got := eventRecorder.EventTypes(); len(got) != 0 {
		t.Fatalf("outer sandbox wrapper events = %v, want none on successful handoff", got)
	}
	assertAgentSandboxClaimRequest(t, recorder)
	assertAgentSandboxWaitReadyRequest(t, recorder)
	assertAgentSandboxUploadRequest(t, recorder)
	assertAgentSandboxExecRequest(t, recorder, workspaceDir)
	assertAgentSandboxDeleteRequest(t, recorder)
}

func TestRunAgent_AgentSandboxForwardsStdoutResultMarker(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	t.Setenv(workerenv.ResultStdout, "true")

	rawResult := `{"kind":"typed-review","payload":"sandbox-result"}`
	recorder := newRecordingWorkspaceExecutor()
	recorder.fake.EnqueueExecResult(workspace.ExecResult{
		ExitCode: 0,
		Stdout: strings.Join([]string{
			"inner worker log",
			workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte(rawResult)),
			"ignored trailing log",
		}, "\n"),
	}, nil)
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
			t.Fatal("outer agent executor should not run when agent sandbox is enabled")
			return "", nil
		})
	})
	if runErr != nil {
		t.Fatalf("RunAgent returned error: %v", runErr)
	}

	if payload := decodeStdoutResultPayload(t, stdout); string(payload) != rawResult {
		t.Fatalf("forwarded stdout payload = %q, want %q", string(payload), rawResult)
	}
	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want 1", len(execReqs))
	}
	if execReqs[0].MaxOutputBytes != agentSandboxStdoutResultMaxOutputBytes {
		t.Fatalf(
			"exec max output bytes = %d, want %d",
			execReqs[0].MaxOutputBytes,
			agentSandboxStdoutResultMaxOutputBytes,
		)
	}
	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete")
}

func TestRunAgent_AgentSandboxForwardsStdoutResultMarkerOnCommandFailure(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	t.Setenv(workerenv.ResultStdout, "true")

	rawResult := `{"kind":"typed-review","payload":"sandbox-error-result"}`
	recorder := newRecordingWorkspaceExecutor()
	recorder.fake.EnqueueExecResult(workspace.ExecResult{
		ExitCode: 42,
		Stdout: strings.Join([]string{
			"inner worker log",
			workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte(rawResult)),
			"ignored trailing log",
		}, "\n"),
		Stderr: "inner stderr",
	}, nil)
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
			t.Fatal("outer agent executor should not run when agent sandbox is enabled")
			return "", nil
		})
	})
	if runErr == nil {
		t.Fatal("expected sandbox command failure")
	}
	if !strings.Contains(runErr.Error(), "test-agent sandbox execution failed") {
		t.Fatalf("RunAgent() error = %q, want sandbox execution context", runErr.Error())
	}
	if payload := decodeStdoutResultPayload(t, stdout); string(payload) != rawResult {
		t.Fatalf("forwarded stdout payload = %q, want %q", string(payload), rawResult)
	}
	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want 1", len(execReqs))
	}
	if execReqs[0].MaxOutputBytes != agentSandboxStdoutResultMaxOutputBytes {
		t.Fatalf(
			"exec max output bytes = %d, want %d",
			execReqs[0].MaxOutputBytes,
			agentSandboxStdoutResultMaxOutputBytes,
		)
	}
	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete")
}

func TestRunAgent_AgentSandboxRetainsWorkspace(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "retain")

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "exec", "release")
	assertAgentSandboxTokenScrubRequest(t, recorder)

	releaseReqs := recorder.releaseRequests()
	if len(releaseReqs) != 1 {
		t.Fatalf("recorded %d release requests, want 1", len(releaseReqs))
	}
	if !releaseReqs[0].Retain {
		t.Error("release request Retain = false, want true")
	}
	if releaseReqs[0].Timeout != 3*time.Second {
		t.Errorf("release timeout = %v, want 3s", releaseReqs[0].Timeout)
	}
}

func TestRunAgent_AgentSandboxUnknownCleanupPolicyRetainsWorkspace(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "archive")

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "exec", "release")
	assertAgentSandboxTokenScrubRequest(t, recorder)

	releaseReqs := recorder.releaseRequests()
	if len(releaseReqs) != 1 {
		t.Fatalf("recorded %d release requests, want 1", len(releaseReqs))
	}
	if !releaseReqs[0].Retain {
		t.Error("release request Retain = false, want true")
	}
	if releaseReqs[0].Reason != "unsupported agent sandbox cleanup policy" {
		t.Errorf("release reason = %q, want unsupported policy reason", releaseReqs[0].Reason)
	}
}

func TestRunAgent_AgentSandboxCleanupUsesFreshContextAfterCancellation(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	ctx, cancel := context.WithCancel(context.Background())
	recorder.afterExec = cancel

	err := runAgentInSandbox(
		ctx,
		"test-agent",
		"/sandbox/workspace",
		workerenv.ParseAgentSandboxEnv(os.Getenv),
	)
	if err != nil {
		t.Fatalf("runAgentInSandbox returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete")

	deleteCtxErrs := recorder.deleteContextErrors()
	if len(deleteCtxErrs) != 1 {
		t.Fatalf("recorded %d delete context errors, want 1", len(deleteCtxErrs))
	}
	if deleteCtxErrs[0] != nil {
		t.Fatalf("delete context error = %v, want nil", deleteCtxErrs[0])
	}
}

func TestRunAgent_ExecutionWorkspaceCleanupFailureRetriedByDeferredCleanup(t *testing.T) {
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
	t.Setenv(workerenv.ServiceAccountToken, "")
	t.Setenv(workerenv.ServiceAccountTokenPath, filepath.Join(t.TempDir(), "missing-token"))

	recorder := newRecordingWorkspaceExecutor()
	recorder.deleteErr = fmt.Errorf("delete boom")
	recorder.afterDelete = func() {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.deleteErr = nil
		recorder.afterDelete = nil
	}
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := runAgentInWorkspace(
		context.Background(),
		"test-agent",
		"/sandbox/workspace",
		workerenv.ExecutionWorkspaceEnv{
			Provider:          string(corev1alpha1.WorkspaceProviderAgentSandbox),
			TemplateName:      "agent-template",
			TemplateNamespace: testAgentSandboxTemplateNamespace,
			ClaimNamespace:    testAgentSandboxTemplateNamespace,
			ClaimName:         "claim-name",
			ClaimTimeout:      3 * time.Second,
			CommandTimeout:    9 * time.Second,
			CleanupPolicy:     "delete",
		},
	)
	if err == nil {
		t.Fatal("expected terminal cleanup failure")
	}
	if !strings.Contains(err.Error(), "execution workspace cleanup failed") ||
		!strings.Contains(err.Error(), "delete boom") {
		t.Fatalf("runAgentInWorkspace() error = %q, want cleanup failure context", err.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete", "delete")
	deleteReqs := recorder.deleteRequests()
	if len(deleteReqs) != 2 {
		t.Fatalf("recorded %d delete requests, want terminal cleanup plus deferred retry", len(deleteReqs))
	}
}

func TestRunAgent_SubstratePreHandoffRetainFailureDeletesNewWorkspace(t *testing.T) {
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
	t.Setenv(workspaceHandoffTokenEnv, "handoff-token")

	recorder := newRecordingWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("not ready")
	restoreExecutor := setSubstrateWorkspaceExecutorForTest(recorder, nil)
	t.Cleanup(restoreExecutor)

	err := runAgentInWorkspace(
		context.Background(),
		"test-agent",
		"/workspace",
		workerenv.ExecutionWorkspaceEnv{
			Provider:          string(corev1alpha1.WorkspaceProviderSubstrate),
			TemplateName:      "orka-codex",
			TemplateNamespace: "ate-demo",
			ClaimNamespace:    "ate-demo",
			ClaimName:         "actor-1",
			ClaimTimeout:      3 * time.Second,
			CommandTimeout:    9 * time.Second,
			CleanupPolicy:     "retain",
		},
	)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(err.Error(), "wait for execution workspace") ||
		!strings.Contains(err.Error(), "not ready") {
		t.Fatalf("runAgentInWorkspace() error = %q, want readiness context", err.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "delete")
	if releaseReqs := recorder.releaseRequests(); len(releaseReqs) != 0 {
		t.Fatalf("recorded %d release requests, want delete before handoff bootstrap", len(releaseReqs))
	}
	deleteReqs := recorder.deleteRequests()
	if len(deleteReqs) != 1 {
		t.Fatalf("recorded %d delete requests, want 1", len(deleteReqs))
	}
	if deleteReqs[0].Reason != executionWorkspaceCleanupReasonDelete {
		t.Fatalf("delete reason = %q, want delete cleanup policy", deleteReqs[0].Reason)
	}
	if !deleteReqs[0].SkipScrub {
		t.Fatal("delete SkipScrub = false, want true before handoff bootstrap")
	}
}

func TestRunAgent_ExecutionWorkspaceForwardsStdoutResultMarker(t *testing.T) {
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
	t.Setenv(workerenv.ResultStdout, "true")
	t.Setenv(workerenv.ServiceAccountToken, "")
	t.Setenv(workerenv.ServiceAccountTokenPath, filepath.Join(t.TempDir(), "missing-token"))

	rawResult := `{"kind":"typed-review","payload":"workspace-result"}`
	recorder := newRecordingWorkspaceExecutor()
	recorder.fake.EnqueueExecResult(workspace.ExecResult{
		ExitCode: 0,
		Stdout: strings.Join([]string{
			"inner worker log",
			workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte(rawResult)),
			"ignored trailing log",
		}, "\n"),
	}, nil)
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/sandbox/workspace",
			workerenv.ExecutionWorkspaceEnv{
				Provider:          string(corev1alpha1.WorkspaceProviderAgentSandbox),
				TemplateName:      "agent-template",
				TemplateNamespace: testAgentSandboxTemplateNamespace,
				ClaimNamespace:    testAgentSandboxTemplateNamespace,
				ClaimName:         "claim-name",
				ClaimTimeout:      3 * time.Second,
				CommandTimeout:    9 * time.Second,
				CleanupPolicy:     "delete",
			},
		)
	})
	if runErr != nil {
		t.Fatalf("runAgentInWorkspace returned error: %v", runErr)
	}

	if payload := decodeStdoutResultPayload(t, stdout); string(payload) != rawResult {
		t.Fatalf("forwarded stdout payload = %q, want %q", string(payload), rawResult)
	}
	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want 1", len(execReqs))
	}
	if execReqs[0].MaxOutputBytes != agentSandboxStdoutResultMaxOutputBytes {
		t.Fatalf(
			"exec max output bytes = %d, want %d",
			execReqs[0].MaxOutputBytes,
			agentSandboxStdoutResultMaxOutputBytes,
		)
	}
	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete")
}

func TestRunAgent_ExecutionWorkspaceForwardsStdoutResultMarkerOnCommandFailure(t *testing.T) {
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
	t.Setenv(workerenv.ResultStdout, "true")
	t.Setenv(workerenv.ServiceAccountToken, "")
	t.Setenv(workerenv.ServiceAccountTokenPath, filepath.Join(t.TempDir(), "missing-token"))

	rawResult := `{"kind":"typed-review","payload":"workspace-error-result"}`
	recorder := newRecordingWorkspaceExecutor()
	recorder.fake.EnqueueExecResult(workspace.ExecResult{
		ExitCode: 42,
		Stdout: strings.Join([]string{
			"inner worker log",
			workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte(rawResult)),
			"ignored trailing log",
		}, "\n"),
		Stderr: "inner stderr",
	}, nil)
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runAgentInWorkspace(
			context.Background(),
			"test-agent",
			"/sandbox/workspace",
			workerenv.ExecutionWorkspaceEnv{
				Provider:          string(corev1alpha1.WorkspaceProviderAgentSandbox),
				TemplateName:      "agent-template",
				TemplateNamespace: testAgentSandboxTemplateNamespace,
				ClaimNamespace:    testAgentSandboxTemplateNamespace,
				ClaimName:         "claim-name",
				ClaimTimeout:      3 * time.Second,
				CommandTimeout:    9 * time.Second,
				CleanupPolicy:     "delete",
			},
		)
	})
	if runErr == nil {
		t.Fatal("expected workspace command failure")
	}
	if !strings.Contains(runErr.Error(), "test-agent workspace execution failed") {
		t.Fatalf("runAgentInWorkspace() error = %q, want workspace execution context", runErr.Error())
	}
	if payload := decodeStdoutResultPayload(t, stdout); string(payload) != rawResult {
		t.Fatalf("forwarded stdout payload = %q, want %q", string(payload), rawResult)
	}
	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want 1", len(execReqs))
	}
	if execReqs[0].MaxOutputBytes != agentSandboxStdoutResultMaxOutputBytes {
		t.Fatalf(
			"exec max output bytes = %d, want %d",
			execReqs[0].MaxOutputBytes,
			agentSandboxStdoutResultMaxOutputBytes,
		)
	}
	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete")
}

func TestRunAgent_SubstratePreHandoffRetainFailureDeletesPooledWorkspace(t *testing.T) {
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
	t.Setenv(workspaceHandoffTokenEnv, "handoff-token")

	recorder := newRecordingWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("not ready")
	restoreExecutor := setSubstrateWorkspaceExecutorForTest(recorder, nil)
	t.Cleanup(restoreExecutor)

	err := runAgentInWorkspace(
		context.Background(),
		"test-agent",
		"/workspace",
		workerenv.ExecutionWorkspaceEnv{
			Provider:          string(corev1alpha1.WorkspaceProviderSubstrate),
			TemplateName:      "orka-codex",
			TemplateNamespace: "ate-demo",
			ClaimNamespace:    "ate-demo",
			ClaimName:         "orka-p-pool-00001",
			ClaimTimeout:      3 * time.Second,
			CommandTimeout:    9 * time.Second,
			CleanupPolicy:     "retain",
			PoolName:          "codex-pool",
			PoolNamespace:     "default",
		},
	)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(err.Error(), "wait for execution workspace") ||
		!strings.Contains(err.Error(), "not ready") {
		t.Fatalf("runAgentInWorkspace() error = %q, want readiness context", err.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "delete")
	deleteReqs := recorder.deleteRequests()
	if len(deleteReqs) != 1 {
		t.Fatalf("recorded %d delete requests, want 1", len(deleteReqs))
	}
	if deleteReqs[0].Reason != executionWorkspaceCleanupReasonDelete {
		t.Fatalf("delete reason = %q, want delete cleanup policy", deleteReqs[0].Reason)
	}
	if !deleteReqs[0].SkipScrub {
		t.Fatal("delete SkipScrub = false, want true before handoff bootstrap")
	}
	if releaseReqs := recorder.releaseRequests(); len(releaseReqs) != 0 {
		t.Fatalf("recorded %d release requests, want pooled workspace deleted", len(releaseReqs))
	}
}

func TestRunAgent_SubstratePreHandoffRetainFailurePreservesReusedWorkspace(t *testing.T) {
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
	t.Setenv(workspaceHandoffTokenEnv, "handoff-token")

	recorder := newRecordingWorkspaceExecutor()
	seed, err := recorder.fake.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ate-demo",
		ClaimName:       "actor-1",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Namespace: "ate-demo", Name: "orka-codex"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("seed retained workspace: %v", err)
	}
	if _, err := recorder.fake.Release(context.Background(), workspace.ReleaseRequest{
		Ref:     seed.Ref,
		Retain:  true,
		Reason:  "seed retained workspace",
		Timeout: time.Second,
	}); err != nil {
		t.Fatalf("retain seeded workspace: %v", err)
	}
	recorder.waitReadyErr = fmt.Errorf("not ready")
	restoreExecutor := setSubstrateWorkspaceExecutorForTest(recorder, nil)
	t.Cleanup(restoreExecutor)

	err = runAgentInWorkspace(
		context.Background(),
		"test-agent",
		"/workspace",
		workerenv.ExecutionWorkspaceEnv{
			Provider:          string(corev1alpha1.WorkspaceProviderSubstrate),
			TemplateName:      "orka-codex",
			TemplateNamespace: "ate-demo",
			ClaimNamespace:    "ate-demo",
			ClaimName:         "actor-1",
			ClaimTimeout:      3 * time.Second,
			CommandTimeout:    9 * time.Second,
			CleanupPolicy:     "retain",
		},
	)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(err.Error(), "wait for execution workspace") ||
		!strings.Contains(err.Error(), "not ready") {
		t.Fatalf("runAgentInWorkspace() error = %q, want readiness context", err.Error())
	}

	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "release")
	if deleteReqs := recorder.deleteRequests(); len(deleteReqs) != 0 {
		t.Fatalf("recorded %d delete requests, want reused workspace retained", len(deleteReqs))
	}
	releaseReqs := recorder.releaseRequests()
	if len(releaseReqs) != 1 {
		t.Fatalf("recorded %d release requests, want 1", len(releaseReqs))
	}
	if !releaseReqs[0].Retain {
		t.Fatal("release Retain = false, want true")
	}
	if !releaseReqs[0].SkipScrub {
		t.Fatal("release SkipScrub = false, want true before handoff bootstrap")
	}
}

func TestExecutionWorkspaceCompletionMessageOmitsSubstrateActorID(t *testing.T) {
	got := executionWorkspaceCompletionMessage(
		testTaskNamespace,
		"task-name",
		workerenv.ExecutionWorkspaceEnv{Provider: string(corev1alpha1.WorkspaceProviderSubstrate)},
		workspace.WorkspaceRef{ClaimName: "actor-1", ID: "actor-1"},
	)

	if strings.Contains(got, "actor-1") {
		t.Fatalf("message = %q, want no Substrate actor ID", got)
	}
	if got != "Task task-ns/task-name completed in substrate workspace" {
		t.Fatalf("message = %q, want sanitized substrate completion", got)
	}
}

func TestExecutionWorkspaceCompletionMessageKeepsAgentSandboxClaimName(t *testing.T) {
	got := executionWorkspaceCompletionMessage(
		testTaskNamespace,
		"task-name",
		workerenv.ExecutionWorkspaceEnv{Provider: string(corev1alpha1.WorkspaceProviderAgentSandbox)},
		workspace.WorkspaceRef{ClaimName: "claim-1"},
	)

	if got != "Task task-ns/task-name completed in agent-sandbox workspace claim-1" {
		t.Fatalf("message = %q, want legacy claim name", got)
	}
}

func TestRunAgent_AgentSandboxRecursionFailsFast(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	t.Setenv(workerenv.AgentSandboxDepth, "1")

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox recursion is detected")
		return "", nil
	})
	if err == nil {
		t.Fatal("expected recursion error")
	}
	if !strings.Contains(err.Error(), "agent sandbox recursion detected") {
		t.Fatalf("error = %q, want recursion context", err.Error())
	}
}

func TestRunAgent_AgentSandboxDefaultsTemplateNamespaceToTaskNamespace(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	t.Setenv(workerenv.AgentSandboxTemplateNamespace, "")
	t.Setenv(workerenv.AgentSandboxClaimNamespace, "")

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	claimReqs := recorder.claimRequests()
	if len(claimReqs) != 1 {
		t.Fatalf("recorded %d claim requests, want 1", len(claimReqs))
	}
	if claimReqs[0].Namespace != testTaskNamespace {
		t.Errorf("claim namespace = %q, want task-ns", claimReqs[0].Namespace)
	}
	if claimReqs[0].Template.Namespace != testTaskNamespace {
		t.Errorf("claim template namespace = %q, want task-ns", claimReqs[0].Template.Namespace)
	}
}

func TestRunAgent_AgentSandboxControllerStrategyUsesTemplateNamespaceForClaims(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	t.Setenv(workerenv.AgentSandboxClaimNamespace, "")
	t.Setenv(workerenv.AgentSandboxNamespaceStrategy, "controller")

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	claimReqs := recorder.claimRequests()
	if len(claimReqs) != 1 {
		t.Fatalf("recorded %d claim requests, want 1", len(claimReqs))
	}
	if claimReqs[0].Namespace != testAgentSandboxTemplateNamespace {
		t.Errorf("claim namespace = %q, want template-ns", claimReqs[0].Namespace)
	}
}

func TestRunAgent_AgentSandboxPassesWarmPoolPolicy(t *testing.T) {
	tests := []struct {
		name      string
		envPolicy string
		want      string
	}{
		{name: "default disabled", want: "none"},
		{name: "explicit disabled", envPolicy: "disabled", want: "none"},
		{name: "template", envPolicy: "template", want: "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredAgentSandboxEnv(t, "delete")
			if tt.envPolicy != "" {
				t.Setenv(workerenv.AgentSandboxWarmPoolPolicy, tt.envPolicy)
			}

			recorder := newRecordingWorkspaceExecutor()
			restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
			t.Cleanup(restoreExecutor)

			err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
				t.Fatal("outer agent executor should not run when agent sandbox is enabled")
				return "", nil
			})
			if err != nil {
				t.Fatalf("RunAgent returned error: %v", err)
			}

			claimReqs := recorder.claimRequests()
			if len(claimReqs) != 1 {
				t.Fatalf("recorded %d claim requests, want 1", len(claimReqs))
			}
			if claimReqs[0].WarmPoolPolicy != tt.want {
				t.Fatalf("claim warm pool policy = %q, want %q", claimReqs[0].WarmPoolPolicy, tt.want)
			}
		})
	}
}

func TestRunAgent_AgentSandboxSessionUsesDeterministicClaimName(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "retain")
	t.Setenv(workerenv.AgentSandboxReusePolicy, "session")

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	claimReqs := recorder.claimRequests()
	if len(claimReqs) != 1 {
		t.Fatalf("recorded %d claim requests, want 1", len(claimReqs))
	}
	want := agentSandboxSessionClaimName(
		workerenv.ParseAgentSandboxEnv(os.Getenv),
		testAgentSandboxTemplateNamespace,
		testTaskNamespace,
		testAgentSandboxTemplateNamespace,
	)
	if claimReqs[0].ClaimName != want || claimReqs[0].ClaimName == "" {
		t.Fatalf("claim name = %q, want deterministic %q", claimReqs[0].ClaimName, want)
	}
	if !claimReqs[0].CreateIfMissing {
		t.Fatal("CreateIfMissing = false, want true for deterministic session claim")
	}
}

func TestRunAgent_AgentSandboxPreservesGitCredentialsBeforeHandoff(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	t.Setenv(workerenv.ServiceAccountToken, "outer-token")

	previousSetup := setupGitCredentialsForRunAgent
	previousGitToken, hadGitToken := os.LookupEnv(workerenv.GitToken)
	previousGitHubToken, hadGitHubToken := os.LookupEnv(workerenv.GitHubToken)
	previousGitAskpass, hadGitAskpass := os.LookupEnv(workerenv.GitAskpass)
	setupGitCredentialsForRunAgent = func() {
		_ = os.Setenv(workerenv.GitToken, "git-token")
		_ = os.Setenv(workerenv.GitHubToken, "github-token")
		_ = os.Setenv(workerenv.GitAskpass, "/bin/echo-token")
	}
	t.Cleanup(func() {
		setupGitCredentialsForRunAgent = previousSetup
		restoreEnv(workerenv.GitToken, previousGitToken, hadGitToken)
		restoreEnv(workerenv.GitHubToken, previousGitHubToken, hadGitHubToken)
		restoreEnv(workerenv.GitAskpass, previousGitAskpass, hadGitAskpass)
	})

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want 1", len(execReqs))
	}
	if execReqs[0].Env[workerenv.GitToken] != "git-token" {
		t.Fatalf("inner %s = %q, want populated git token", workerenv.GitToken, execReqs[0].Env[workerenv.GitToken])
	}
	if execReqs[0].Env[workerenv.GitHubToken] != "github-token" ||
		execReqs[0].Env[workerenv.GitAskpass] != agentSandboxGitAskpassExecPath {
		t.Fatalf("inner git credential env not preserved: %#v", execReqs[0].Env)
	}
	wantCommand := []string{"sh", "-c", agentSandboxWorkerCommand(true, true), agentSandboxWorkerUploadPath}
	wantCommand = append(wantCommand, os.Args[1:]...)
	if !reflect.DeepEqual(execReqs[0].Command, wantCommand) {
		t.Fatalf("inner worker command = %#v, want %#v", execReqs[0].Command, wantCommand)
	}

	artifacts := artifactsByPath(t, recorder)
	assertAgentSandboxGitAskpassArtifact(t, artifacts[agentSandboxGitAskpassUploadPath])
}

func TestRunAgent_AgentSandboxStagesSharedTransactionTokenFile(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(" transaction-token \n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Setenv(workerenv.TransactionTokenFile, tokenPath)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, tokenPath)

	recorder := newRecordingWorkspaceExecutor()
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err != nil {
		t.Fatalf("RunAgent returned error: %v", err)
	}

	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want 1", len(execReqs))
	}
	if execReqs[0].Env[workerenv.TransactionTokenFile] != agentSandboxTransactionTokenExecPath {
		t.Fatalf(
			"inner %s = %q, want %q",
			workerenv.TransactionTokenFile,
			execReqs[0].Env[workerenv.TransactionTokenFile],
			agentSandboxTransactionTokenExecPath,
		)
	}
	if execReqs[0].Env[workerenv.ContextTokenSubjectTokenFile] != agentSandboxTransactionTokenExecPath {
		t.Fatalf(
			"inner %s = %q, want shared %q",
			workerenv.ContextTokenSubjectTokenFile,
			execReqs[0].Env[workerenv.ContextTokenSubjectTokenFile],
			agentSandboxTransactionTokenExecPath,
		)
	}
	wantCommand := []string{
		"sh",
		"-c",
		agentSandboxWorkerCommand(false, false, agentSandboxTransactionTokenExecPath),
		agentSandboxWorkerUploadPath,
	}
	wantCommand = append(wantCommand, os.Args[1:]...)
	if !reflect.DeepEqual(execReqs[0].Command, wantCommand) {
		t.Fatalf("inner worker command = %#v, want %#v", execReqs[0].Command, wantCommand)
	}

	artifacts := artifactsByPath(t, recorder)
	assertAgentSandboxWorkerArtifact(t, artifacts[agentSandboxWorkerUploadPath])
	assertAgentSandboxTransactionTokenArtifact(t, artifacts[agentSandboxTransactionTokenUploadPath], "transaction-token")
	if _, ok := artifacts[agentSandboxContextSubjectTokenUploadPath]; ok {
		t.Fatalf("uploaded duplicate context subject token artifact for shared source path")
	}
}

func TestRunAgent_AgentSandboxClaimFailure(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")

	recorder := newRecordingWorkspaceExecutor()
	recorder.claimErr = fmt.Errorf("claim boom")
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err == nil {
		t.Fatal("expected claim error")
	}
	if !strings.Contains(err.Error(), "claim agent sandbox workspace") {
		t.Errorf("error = %q, want claim context", err.Error())
	}
	if !strings.Contains(err.Error(), "claim boom") {
		t.Errorf("error = %q, want original claim error", err.Error())
	}
	assertOperationOrder(t, recorder.operations(), "claim")
}

func TestRunAgent_AgentSandboxWaitReadyFailureDeletesWorkspace(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")

	recorder := newRecordingWorkspaceExecutor()
	recorder.waitReadyErr = fmt.Errorf("not ready")
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err == nil {
		t.Fatal("expected wait-ready error")
	}
	if !strings.Contains(err.Error(), "wait for agent sandbox workspace") {
		t.Errorf("error = %q, want wait-ready context", err.Error())
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error = %q, want original wait-ready error", err.Error())
	}
	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "delete")
}

func TestRunAgent_AgentSandboxUploadFailureDeletesWorkspace(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")

	recorder := newRecordingWorkspaceExecutor()
	recorder.uploadErr = fmt.Errorf("upload boom")
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err == nil {
		t.Fatal("expected upload error")
	}
	if !strings.Contains(err.Error(), "stage agent executable in sandbox") {
		t.Errorf("error = %q, want staging context", err.Error())
	}
	if !strings.Contains(err.Error(), "upload boom") {
		t.Errorf("error = %q, want original upload error", err.Error())
	}
	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "delete")
}

func TestRunAgent_AgentSandboxCommandFailureCleansUpWithoutSubmittingResult(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")

	var resultRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resultRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(workerenv.ResultEndpoint, server.URL)

	recorder := newRecordingWorkspaceExecutor()
	recorder.fake.EnqueueExecResult(workspace.ExecResult{
		ExitCode: 42,
		Stdout:   "inner stdout",
		Stderr:   "inner stderr",
	}, nil)
	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(recorder)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err == nil {
		t.Fatal("expected sandbox command error")
	}
	if !strings.Contains(err.Error(), "test-agent sandbox execution failed") {
		t.Errorf("error = %q, want sandbox execution context", err.Error())
	}
	if !strings.Contains(err.Error(), "command exited with code 42") {
		t.Errorf("error = %q, want command exit code", err.Error())
	}
	if !strings.Contains(err.Error(), "stdout=inner stdout") || !strings.Contains(err.Error(), "stderr=inner stderr") {
		t.Errorf("error = %q, want inner stdout/stderr", err.Error())
	}
	if !strings.Contains(err.Error(), "stdout_truncated=false") ||
		!strings.Contains(err.Error(), "stderr_truncated=false") {
		t.Errorf("error = %q, want stdout/stderr truncation flags", err.Error())
	}
	assertOperationOrder(t, recorder.operations(), "claim", "waitReady", "upload", "exec", "delete")
	if got := resultRequests.Load(); got != 0 {
		t.Fatalf("result endpoint received %d requests, want 0", got)
	}
}

func TestSubmitExecutionWorkspaceStatusIncludesReadyPlacement(t *testing.T) {
	var status executionWorkspaceStatusUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	submitExecutionWorkspaceStatus(
		workerenv.ExecutionWorkspaceEnv{
			Provider:          string(corev1alpha1.WorkspaceProviderSubstrate),
			TemplateName:      "orka-codex",
			TemplateNamespace: "ate-demo",
			ReusePolicy:       "session",
			CleanupPolicy:     "retain",
			StatusEndpoint:    server.URL,
		},
		corev1alpha1.ExecutionWorkspacePhaseReady,
		corev1alpha1.ExecutionWorkspaceReasonReady,
		true,
		"workspace ready",
		withExecutionWorkspaceReadyResult(&workspace.ReadyResult{
			Placement: workspace.Placement{
				WorkerNamespace: "ate-demo",
				WorkerPool:      "codex-pool",
				WorkerPodName:   "ateom-worker-1",
				PodIP:           "10.244.0.42",
			},
			Density: workspace.Density{
				WorkerCount:         1,
				ActorCount:          3,
				RunningActorCount:   1,
				SuspendedActorCount: 2,
				ActorsPerWorker:     "3.00",
			},
			ResumeLatency: 750 * time.Millisecond,
		}),
	)

	if status.Placement == nil {
		t.Fatal("placement = nil, want placement status")
	}
	if status.Placement.WorkerPool != "codex-pool" ||
		status.Placement.WorkerPodName != "ateom-worker-1" {
		t.Fatalf("placement = %#v, want worker placement", status.Placement)
	}
	if status.ResumeLatency == nil || status.ResumeLatency.Duration != 750*time.Millisecond {
		t.Fatalf("resume latency = %#v, want 750ms", status.ResumeLatency)
	}
	if status.Density == nil {
		t.Fatal("density = nil, want density status")
	}
	if status.Density.ActorCount != 3 || status.Density.WorkerCount != 1 ||
		status.Density.ActorsPerWorker != "3.00" {
		t.Fatalf("density = %#v, want actor/worker density", status.Density)
	}
}

func TestRunAgent_AgentSandboxMissingWorkspaceExecutor(t *testing.T) {
	setRequiredAgentSandboxEnv(t, "delete")

	restoreExecutor := setAgentSandboxWorkspaceExecutorForTest(nil)
	t.Cleanup(restoreExecutor)

	err := RunAgent("test-agent", "/sandbox/workspace", 50, func(_ context.Context, _ *AgentConfig) (string, error) {
		t.Fatal("outer agent executor should not run when agent sandbox is enabled")
		return "", nil
	})
	if err == nil {
		t.Fatal("expected missing executor error")
	}
	if !strings.Contains(err.Error(), "agent sandbox workspace executor is not configured") {
		t.Errorf("error = %q, want missing executor context", err.Error())
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}

	done := make(chan []byte)
	go func() {
		out, _ := io.ReadAll(reader)
		done <- out
	}()

	os.Stdout = writer
	defer func() {
		os.Stdout = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	out := <-done
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return string(out)
}

func decodeStdoutResultPayload(t *testing.T, logs string) []byte {
	t.Helper()
	var encoded string
	for line := range strings.SplitSeq(logs, "\n") {
		if payload, ok := strings.CutPrefix(strings.TrimSpace(line), workerenv.ResultStdoutPrefix); ok {
			encoded = strings.TrimSpace(payload)
		}
	}
	if encoded == "" {
		t.Fatalf("stdout missing %s marker:\n%s", workerenv.ResultStdoutPrefix, logs)
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode stdout result payload: %v", err)
	}
	return payload
}

func assertAgentSandboxClaimRequest(t *testing.T, recorder *recordingWorkspaceExecutor) {
	t.Helper()
	claimReqs := recorder.claimRequests()
	if len(claimReqs) != 1 {
		t.Fatalf("recorded %d claim requests, want 1", len(claimReqs))
	}
	claimReq := claimReqs[0]
	if claimReq.Namespace != testAgentSandboxTemplateNamespace {
		t.Errorf("claim namespace = %q, want template-ns", claimReq.Namespace)
	}
	if claimReq.TaskName != "task-name" {
		t.Errorf("claim task name = %q, want task-name", claimReq.TaskName)
	}
	if claimReq.Template.Namespace != testAgentSandboxTemplateNamespace {
		t.Errorf("claim template namespace = %q, want template-ns", claimReq.Template.Namespace)
	}
	if claimReq.Template.Name != "agent-template" {
		t.Errorf("claim template name = %q, want agent-template", claimReq.Template.Name)
	}
	if claimReq.ReuseKey != "reuse-key" {
		t.Errorf("claim reuse key = %q, want reuse-key", claimReq.ReuseKey)
	}
	if claimReq.Timeout != 3*time.Second {
		t.Errorf("claim timeout = %v, want 3s", claimReq.Timeout)
	}
}

func assertAgentSandboxWaitReadyRequest(t *testing.T, recorder *recordingWorkspaceExecutor) {
	t.Helper()
	waitReadyReqs := recorder.waitReadyRequests()
	if len(waitReadyReqs) != 1 {
		t.Fatalf("recorded %d wait-ready requests, want 1", len(waitReadyReqs))
	}
	if waitReadyReqs[0].Timeout != 3*time.Second {
		t.Errorf("wait-ready timeout = %v, want 3s", waitReadyReqs[0].Timeout)
	}
}

func assertAgentSandboxUploadRequest(t *testing.T, recorder *recordingWorkspaceExecutor) {
	t.Helper()
	uploadReqs := recorder.uploadRequests()
	if len(uploadReqs) != 1 {
		t.Fatalf("recorded %d upload requests, want 1", len(uploadReqs))
	}
	uploadReq := uploadReqs[0]
	if uploadReq.Timeout != 9*time.Second {
		t.Errorf("upload timeout = %v, want 9s", uploadReq.Timeout)
	}
	if len(uploadReq.Artifacts) != 2 {
		t.Fatalf("uploaded artifacts = %d, want 2", len(uploadReq.Artifacts))
	}
	artifacts := artifactsByPath(t, recorder)
	assertAgentSandboxWorkerArtifact(t, artifacts[agentSandboxWorkerUploadPath])
	assertAgentSandboxTokenArtifact(t, artifacts[agentSandboxSATokenUploadPath])
}

func artifactsByPath(t *testing.T, recorder *recordingWorkspaceExecutor) map[string]workspace.UploadArtifact {
	t.Helper()
	uploadReqs := recorder.uploadRequests()
	if len(uploadReqs) != 1 {
		t.Fatalf("recorded %d upload requests, want 1", len(uploadReqs))
	}
	artifacts := make(map[string]workspace.UploadArtifact, len(uploadReqs[0].Artifacts))
	for _, artifact := range uploadReqs[0].Artifacts {
		artifacts[artifact.Path] = artifact
	}
	return artifacts
}

func assertAgentSandboxWorkerArtifact(t *testing.T, artifact workspace.UploadArtifact) {
	t.Helper()
	if artifact.Path != agentSandboxWorkerUploadPath {
		t.Errorf("uploaded worker artifact path = %q, want %q", artifact.Path, agentSandboxWorkerUploadPath)
	}
	if artifact.Mode != 0o700 {
		t.Errorf("uploaded worker artifact mode = %#o, want 0700", artifact.Mode)
	}
	if len(artifact.Data) == 0 {
		t.Fatal("uploaded worker executable is empty")
	}
}

func assertAgentSandboxTokenArtifact(t *testing.T, artifact workspace.UploadArtifact) {
	t.Helper()
	if artifact.Path != agentSandboxSATokenUploadPath {
		t.Errorf("uploaded token artifact path = %q, want %q", artifact.Path, agentSandboxSATokenUploadPath)
	}
	if artifact.Mode != 0o600 {
		t.Errorf("uploaded token artifact mode = %#o, want 0600", artifact.Mode)
	}
	if string(artifact.Data) != "outer-token" {
		t.Fatal("uploaded token artifact data was not the configured token")
	}
}

func assertAgentSandboxTransactionTokenArtifact(t *testing.T, artifact workspace.UploadArtifact, wantData string) {
	t.Helper()
	if artifact.Path != agentSandboxTransactionTokenUploadPath {
		t.Errorf(
			"uploaded transaction token artifact path = %q, want %q",
			artifact.Path,
			agentSandboxTransactionTokenUploadPath,
		)
	}
	if artifact.Mode != 0o600 {
		t.Errorf("uploaded transaction token artifact mode = %#o, want 0600", artifact.Mode)
	}
	if string(artifact.Data) != wantData {
		t.Fatal("uploaded transaction token artifact data was not the trimmed token")
	}
}

func assertAgentSandboxGitAskpassArtifact(t *testing.T, artifact workspace.UploadArtifact) {
	t.Helper()
	if artifact.Path != agentSandboxGitAskpassUploadPath {
		t.Errorf("uploaded git askpass artifact path = %q, want %q", artifact.Path, agentSandboxGitAskpassUploadPath)
	}
	if artifact.Mode != 0o700 {
		t.Errorf("uploaded git askpass artifact mode = %#o, want 0700", artifact.Mode)
	}
	if string(artifact.Data) != "#!/bin/sh\nprintf '%s\\n' \"$GIT_TOKEN\"\n" {
		t.Fatalf("uploaded git askpass script = %q", string(artifact.Data))
	}
}

func assertAgentSandboxExecRequest(t *testing.T, recorder *recordingWorkspaceExecutor, workspaceDir string) {
	t.Helper()
	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want 1", len(execReqs))
	}
	execReq := execReqs[0]
	if execReq.WorkDir != workspaceDir {
		t.Errorf("exec workdir = %q, want %q", execReq.WorkDir, workspaceDir)
	}
	if execReq.Timeout != 9*time.Second {
		t.Errorf("exec timeout = %v, want 9s", execReq.Timeout)
	}
	if execReq.MaxOutputBytes != agentSandboxExecMaxOutputBytes {
		t.Errorf("exec max output bytes = %d, want %d", execReq.MaxOutputBytes, agentSandboxExecMaxOutputBytes)
	}
	assertAgentSandboxInnerEnv(t, execReq.Env)
	assertAgentSandboxCommand(t, execReq.Command)
}

func assertAgentSandboxInnerEnv(t *testing.T, env map[string]string) {
	t.Helper()
	if env[workerenv.AgentSandboxEnabled] != "false" {
		t.Errorf("%s in inner env = %q, want false", workerenv.AgentSandboxEnabled, env[workerenv.AgentSandboxEnabled])
	}
	if env[workerenv.AgentSandboxDepth] != "1" {
		t.Errorf("%s in inner env = %q, want 1", workerenv.AgentSandboxDepth, env[workerenv.AgentSandboxDepth])
	}
	if env["SANDBOX_TEST_ENV"] != "outer-value" {
		t.Errorf("SANDBOX_TEST_ENV in inner env = %q, want outer-value", env["SANDBOX_TEST_ENV"])
	}
	if env[workerenv.ServiceAccountToken] != "" {
		t.Fatalf("inner env unexpectedly contains raw service account token")
	}
	if env[workerenv.ServiceAccountTokenPath] != agentSandboxSATokenExecPath {
		t.Errorf(
			"%s in inner env = %q, want %q",
			workerenv.ServiceAccountTokenPath,
			env[workerenv.ServiceAccountTokenPath],
			agentSandboxSATokenExecPath,
		)
	}
}

func assertAgentSandboxCommand(t *testing.T, got []string) {
	t.Helper()
	stagedCommand := []string{
		"sh",
		"-c",
		agentSandboxWorkerCommand(true, false),
		agentSandboxWorkerUploadPath,
	}
	wantCommand := append(stagedCommand, os.Args[1:]...)
	if !reflect.DeepEqual(got, wantCommand) {
		t.Errorf("exec command = %#v, want %#v", got, wantCommand)
	}
}

func assertAgentSandboxTokenScrubRequest(t *testing.T, recorder *recordingWorkspaceExecutor) {
	t.Helper()
	execReqs := recorder.execRequests()
	if len(execReqs) != 2 {
		t.Fatalf("recorded %d exec requests, want inner worker plus token scrub", len(execReqs))
	}
	scrub := execReqs[1]
	want := []string{
		"rm",
		"-f",
		agentSandboxSATokenExecPath,
		agentSandboxTransactionTokenExecPath,
		agentSandboxContextSubjectTokenExecPath,
	}
	if !reflect.DeepEqual(scrub.Command, want) {
		t.Fatalf("scrub command = %#v, want %#v", scrub.Command, want)
	}
	if scrub.Timeout != 3*time.Second {
		t.Errorf("scrub timeout = %v, want 3s", scrub.Timeout)
	}
}

func assertAgentSandboxDeleteRequest(t *testing.T, recorder *recordingWorkspaceExecutor) {
	t.Helper()
	deleteReqs := recorder.deleteRequests()
	if len(deleteReqs) != 1 {
		t.Fatalf("recorded %d delete requests, want 1", len(deleteReqs))
	}
	if deleteReqs[0].Timeout != 3*time.Second {
		t.Errorf("delete timeout = %v, want 3s", deleteReqs[0].Timeout)
	}
}

func restoreEnv(name, value string, hadValue bool) {
	if hadValue {
		_ = os.Setenv(name, value)
		return
	}
	_ = os.Unsetenv(name)
}

func setRequiredAgentSandboxEnv(t *testing.T, cleanupPolicy string) {
	t.Helper()
	t.Setenv(workerenv.AgentSandboxEnabled, "true")
	t.Setenv(workerenv.AgentSandboxTemplateName, "agent-template")
	t.Setenv(workerenv.AgentSandboxTemplateNamespace, testAgentSandboxTemplateNamespace)
	t.Setenv(workerenv.AgentSandboxClaimNamespace, testAgentSandboxTemplateNamespace)
	t.Setenv(workerenv.AgentSandboxReuseKey, "reuse-key")
	t.Setenv(workerenv.AgentSandboxClaimTimeoutSeconds, "3")
	t.Setenv(workerenv.AgentSandboxCommandTimeoutSeconds, "9")
	t.Setenv(workerenv.AgentSandboxCleanupPolicy, cleanupPolicy)
	t.Setenv(workerenv.TaskName, "task-name")
	t.Setenv(workerenv.TaskNamespace, testTaskNamespace)
}

func assertOperationOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %#v, want %#v", got, want)
	}
}

type recordingWorkspaceExecutor struct {
	fake *workspace.FakeExecutor

	mu            sync.Mutex
	ops           []string
	claimReqs     []workspace.ClaimRequest
	waitReadyReqs []workspace.WaitReadyRequest
	execReqs      []workspace.ExecRequest
	releaseReqs   []workspace.ReleaseRequest
	deleteReqs    []workspace.DeleteRequest
	uploadReqs    []workspace.UploadRequest
	downloadReqs  []workspace.DownloadRequest
	describeReqs  []workspace.DescribeRequest
	deleteCtxErrs []error
	afterExec     func()
	afterDelete   func()
	claimErr      error
	waitReadyErr  error
	execErr       error
	releaseErr    error
	deleteErr     error
	uploadErr     error
	downloadErr   error
	describeErr   error
}

func newRecordingWorkspaceExecutor() *recordingWorkspaceExecutor {
	return &recordingWorkspaceExecutor{
		fake: workspace.NewFakeExecutor(),
	}
}

func (r *recordingWorkspaceExecutor) Claim(
	ctx context.Context,
	req workspace.ClaimRequest,
) (*workspace.ClaimResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "claim")
	r.claimReqs = append(r.claimReqs, req)
	err := r.claimErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fake.Claim(ctx, req)
}

func (r *recordingWorkspaceExecutor) WaitReady(
	ctx context.Context,
	req workspace.WaitReadyRequest,
) (*workspace.ReadyResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "waitReady")
	r.waitReadyReqs = append(r.waitReadyReqs, req)
	err := r.waitReadyErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fake.WaitReady(ctx, req)
}

func (r *recordingWorkspaceExecutor) Exec(
	ctx context.Context,
	req workspace.ExecRequest,
) (*workspace.ExecResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "exec")
	r.execReqs = append(r.execReqs, copyExecRequest(req))
	err := r.execErr
	afterExec := r.afterExec
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	result, fakeErr := r.fake.Exec(ctx, req)
	if afterExec != nil {
		afterExec()
	}
	return result, fakeErr
}

func (r *recordingWorkspaceExecutor) Upload(
	ctx context.Context,
	req workspace.UploadRequest,
) (*workspace.UploadResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "upload")
	r.uploadReqs = append(r.uploadReqs, req)
	err := r.uploadErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fake.Upload(ctx, req)
}

func (r *recordingWorkspaceExecutor) Download(
	ctx context.Context,
	req workspace.DownloadRequest,
) (*workspace.DownloadResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "download")
	r.downloadReqs = append(r.downloadReqs, req)
	err := r.downloadErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fake.Download(ctx, req)
}

func (r *recordingWorkspaceExecutor) Release(
	ctx context.Context,
	req workspace.ReleaseRequest,
) (*workspace.ReleaseResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "release")
	r.releaseReqs = append(r.releaseReqs, req)
	err := r.releaseErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fake.Release(ctx, req)
}

func (r *recordingWorkspaceExecutor) Delete(
	ctx context.Context,
	req workspace.DeleteRequest,
) (*workspace.DeleteResult, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "delete")
	r.deleteReqs = append(r.deleteReqs, req)
	r.deleteCtxErrs = append(r.deleteCtxErrs, ctx.Err())
	err := r.deleteErr
	afterDelete := r.afterDelete
	r.mu.Unlock()
	if afterDelete != nil {
		afterDelete()
	}
	if err != nil {
		return nil, err
	}
	return r.fake.Delete(ctx, req)
}

func (r *recordingWorkspaceExecutor) Describe(
	ctx context.Context,
	req workspace.DescribeRequest,
) (*workspace.Description, error) {
	r.mu.Lock()
	r.ops = append(r.ops, "describe")
	r.describeReqs = append(r.describeReqs, req)
	err := r.describeErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.fake.Describe(ctx, req)
}

func (r *recordingWorkspaceExecutor) operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

func (r *recordingWorkspaceExecutor) claimRequests() []workspace.ClaimRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]workspace.ClaimRequest(nil), r.claimReqs...)
}

func (r *recordingWorkspaceExecutor) waitReadyRequests() []workspace.WaitReadyRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]workspace.WaitReadyRequest(nil), r.waitReadyReqs...)
}

func (r *recordingWorkspaceExecutor) execRequests() []workspace.ExecRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]workspace.ExecRequest, 0, len(r.execReqs))
	for _, req := range r.execReqs {
		out = append(out, copyExecRequest(req))
	}
	return out
}

func (r *recordingWorkspaceExecutor) uploadRequests() []workspace.UploadRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]workspace.UploadRequest, 0, len(r.uploadReqs))
	for _, req := range r.uploadReqs {
		req.Artifacts = append([]workspace.UploadArtifact(nil), req.Artifacts...)
		for i := range req.Artifacts {
			req.Artifacts[i].Data = append([]byte(nil), req.Artifacts[i].Data...)
		}
		out = append(out, req)
	}
	return out
}

func (r *recordingWorkspaceExecutor) releaseRequests() []workspace.ReleaseRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]workspace.ReleaseRequest(nil), r.releaseReqs...)
}

func (r *recordingWorkspaceExecutor) deleteRequests() []workspace.DeleteRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]workspace.DeleteRequest(nil), r.deleteReqs...)
}

func (r *recordingWorkspaceExecutor) deleteContextErrors() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.deleteCtxErrs...)
}

func copyExecRequest(req workspace.ExecRequest) workspace.ExecRequest {
	req.Command = append([]string(nil), req.Command...)
	req.Stdin = append([]byte(nil), req.Stdin...)
	if req.Env != nil {
		env := make(map[string]string, len(req.Env))
		maps.Copy(env, req.Env)
		req.Env = env
	}
	return req
}

var _ workspace.WorkspaceExecutor = (*recordingWorkspaceExecutor)(nil)
