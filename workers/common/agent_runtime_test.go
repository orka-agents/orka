/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestLoadWorkspaceConfig_AllFields(t *testing.T) {
	t.Setenv(workerenv.GitRepo, "https://github.com/example/repo.git")
	t.Setenv(workerenv.GitBranch, "main")
	t.Setenv(workerenv.GitRef, "abc123")
	t.Setenv(workerenv.GitRefShallow, "true")
	t.Setenv(workerenv.PRBaseBranch, "release")
	t.Setenv(workerenv.PRBaseRepo, "https://github.com/example/base.git")
	t.Setenv(workerenv.PRBaseSHA, "def456")
	t.Setenv(workerenv.WorkspaceSubpath, "src/./pkg")

	cfg, err := LoadWorkspaceConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := WorkspaceConfig{
		GitRepo:       "https://github.com/example/repo.git",
		GitBranch:     "main",
		GitRef:        "abc123",
		ShallowGitRef: true,
		PRBaseBranch:  "release",
		PRBaseRepo:    "https://github.com/example/base.git",
		PRBaseSHA:     "def456",
		SubPath:       "src/pkg",
	}
	if *cfg != want {
		t.Errorf("LoadWorkspaceConfig() = %+v, want %+v", *cfg, want)
	}
}

func TestLoadWorkspaceConfig_RejectsSubPathTraversal(t *testing.T) {
	for _, subPath := range []string{"../outside", "src/../../outside", "/etc"} {
		t.Run(subPath, func(t *testing.T) {
			t.Setenv(workerenv.WorkspaceSubpath, subPath)
			if _, err := LoadWorkspaceConfig(); err == nil || !strings.Contains(err.Error(), "path traversal") {
				t.Fatalf("LoadWorkspaceConfig() error = %v, want path traversal rejection", err)
			}
		})
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
	cfg := &WorkspaceConfig{
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
	cfg := &WorkspaceConfig{
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
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
	cfg := &WorkspaceConfig{
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
	cfg := &WorkspaceConfig{
		GitRepo: "file://" + bareDir,
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
	shallow := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "--is-shallow-repository"))
	if shallow != "false" {
		t.Fatalf("is-shallow-repository = %q, want full history for an ordinary pinned workspace", shallow)
	}
	if commitCount := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-list", "--count", "HEAD")); commitCount != "2" {
		t.Fatalf("pinned HEAD history count = %s, want full feature history", commitCount)
	}
}

func TestCloneRepo_ShallowRefRecoversAncestorFromRemoteHeads(t *testing.T) {
	bareDir := t.TempDir()
	runGit(t, bareDir, "init", "--bare")

	workDir := t.TempDir()
	runGit(t, workDir, "init")
	runGit(t, workDir, "checkout", "-b", "main")
	runGit(t, workDir, "config", "user.email", "test@test.com")
	runGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "version.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "base")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "origin", "main")
	runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")

	runGit(t, workDir, "checkout", "-b", "feature/validation")
	if err := os.WriteFile(filepath.Join(workDir, "version.txt"), []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "target")
	targetSHA := strings.TrimSpace(runGitOutput(t, workDir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(workDir, "version.txt"), []byte("newer"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "newer")
	runGit(t, workDir, "push", "origin", "feature/validation")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "git")
	shim := `#!/bin/sh
seen_fetch=false
for arg in "$@"; do
  if [ "$arg" = fetch ]; then seen_fetch=true; continue; fi
  if [ "$seen_fetch" = true ] && [ "$arg" = "$REJECT_GIT_REF" ]; then exit 1; fi
done
if [ "$seen_fetch" = true ]; then printf '%s\n' "$*" >> "$GIT_SHIM_LOG"; fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("REJECT_GIT_REF", targetSHA)
	shimLog := filepath.Join(shimDir, "fetch.log")
	t.Setenv("GIT_SHIM_LOG", shimLog)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cloneDir := filepath.Join(t.TempDir(), "cloned")
	cfg := &WorkspaceConfig{
		GitRepo:       "file://" + bareDir,
		GitRef:        targetSHA,
		ShallowGitRef: true,
	}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("CloneRepo failed: %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, cloneDir, "rev-parse", "HEAD")); got != targetSHA {
		t.Fatalf("HEAD = %s, want ancestor %s", got, targetSHA)
	}
	fetchLog, err := os.ReadFile(shimLog)
	if err != nil {
		t.Fatal(err)
	}
	wantFetch := "--depth=256 origin +refs/heads/*:refs/remotes/origin/*"
	if !strings.Contains(string(fetchLog), wantFetch) ||
		strings.Contains(string(fetchLog), "--unshallow") {
		t.Fatalf("fetch log = %q, want bounded remote-head fetch", fetchLog)
	}
	data, err := os.ReadFile(filepath.Join(cloneDir, "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "target" {
		t.Fatalf("version.txt = %q, want target ancestor contents", data)
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}

	cfg = &WorkspaceConfig{GitRepo: bareDir, GitRef: featureSHA}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}

	featureCfg := &WorkspaceConfig{GitRepo: bareDir, GitRef: "feature/reused"}
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
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

	err := CloneRepo(context.Background(), &WorkspaceConfig{GitRepo: bareDir, GitRef: "missing/ref"}, cloneDir)
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
	cfg := &WorkspaceConfig{GitRepo: bareDir, GitBranch: "main"}
	if err := CloneRepo(context.Background(), cfg, cloneDir); err != nil {
		t.Fatalf("initial CloneRepo failed: %v", err)
	}

	err := CloneRepo(context.Background(), &WorkspaceConfig{GitRepo: otherBareDir, GitBranch: "main"}, cloneDir)
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

	cfg := &WorkspaceConfig{
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
