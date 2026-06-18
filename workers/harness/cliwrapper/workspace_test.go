package cliwrapper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/workerenv"
)

func TestFetchAndCheckoutWorkspaceRefRejectsPathspecFallback(t *testing.T) {
	cloneDir, _ := newWorkspaceGitFixture(t)
	before := testGitOutput(t, cloneDir, "rev-parse", "HEAD")

	err := fetchAndCheckoutWorkspaceRef(context.Background(), cloneDir, "README.md", "https://github.com/example/repo.git")
	if err == nil {
		t.Fatal("fetchAndCheckoutWorkspaceRef error = nil, want invalid pathspec ref rejected")
	}
	if !strings.Contains(err.Error(), "fetch turn workspace remote branch") {
		t.Fatalf("fetchAndCheckoutWorkspaceRef error = %v, want exact-branch fetch failure", err)
	}
	after := testGitOutput(t, cloneDir, "rev-parse", "HEAD")
	if after != before {
		t.Fatalf("HEAD = %s after invalid ref, want unchanged %s", after, before)
	}
}

func TestFetchWorkspaceRemoteBranchRedactsRawRepoURL(t *testing.T) {
	cloneDir, _ := newWorkspaceGitFixture(t)
	rawRepo := "https://user:token@example.invalid/private/repo.git?secret=value#frag"
	err := fetchWorkspaceRemoteBranch(context.Background(), cloneDir, "missing", rawRepo)
	if err == nil {
		t.Fatal("fetchWorkspaceRemoteBranch error = nil, want fetch failure")
	}
	message := err.Error()
	if strings.Contains(message, "user:token") ||
		strings.Contains(message, "secret=value") ||
		strings.Contains(message, "#frag") {
		t.Fatalf("fetchWorkspaceRemoteBranch error leaked raw repo URL: %v", err)
	}
}

func TestFetchAndCheckoutWorkspaceRefFallbackChecksOutRemoteBranchCommit(t *testing.T) {
	cloneDir, featureCommit := newWorkspaceGitFixture(t)

	err := fetchAndCheckoutWorkspaceRef(
		context.Background(),
		cloneDir,
		"origin/feature",
		"https://github.com/example/repo.git",
	)
	if err != nil {
		t.Fatalf("fetchAndCheckoutWorkspaceRef: %v", err)
	}
	actual := testGitOutput(t, cloneDir, "rev-parse", "HEAD")
	if actual != featureCommit {
		t.Fatalf("HEAD = %s, want feature commit %s", actual, featureCommit)
	}
}

func TestFetchAndCheckoutWorkspaceRefFetchesPinnedCommitHistory(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "init")
	testGit(t, sourceDir, "config", "user.email", "test@example.invalid")
	testGit(t, sourceDir, "config", "user.name", "Test User")
	testGit(t, sourceDir, "checkout", "-B", "main")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "add", "README.md")
	testGit(t, sourceDir, "commit", "-m", "main")
	testGit(t, sourceDir, "checkout", "-B", "feature")
	if err := os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "add", "feature.txt")
	testGit(t, sourceDir, "commit", "-m", "feature first")
	firstFeatureCommit := testGitOutput(t, sourceDir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "commit", "-am", "feature second")
	testGit(t, sourceDir, "checkout", "main")

	originDir := filepath.Join(root, "origin.git")
	testGit(t, root, "clone", "--bare", sourceDir, originDir)
	cloneDir := filepath.Join(root, "clone")
	testGit(t, root, "clone", "--depth=1", "--branch", "main", "file://"+originDir, cloneDir)

	if err := fetchAndCheckoutWorkspaceRef(
		context.Background(),
		cloneDir,
		firstFeatureCommit,
		"https://github.com/example/repo.git",
	); err != nil {
		t.Fatalf("fetchAndCheckoutWorkspaceRef: %v", err)
	}
	if got := testGitOutput(t, cloneDir, "rev-parse", "HEAD"); got != firstFeatureCommit {
		t.Fatalf("HEAD = %s, want pinned commit %s", got, firstFeatureCommit)
	}
}

func TestWorkspaceGitCommandForcesControllerAskpass(t *testing.T) {
	t.Setenv(workerenv.GitToken, "token")
	t.Setenv(workerenv.GitAskpass, "/tmp/attacker-askpass")

	cmd := workspaceGitCommand(context.Background(), "status")
	env := map[string]string{}
	for _, entry := range cmd.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GIT_ASKPASS"]; got != controllerGitAskpassPath {
		t.Fatalf("GIT_ASKPASS = %q, want controller helper %q", got, controllerGitAskpassPath)
	}
	if got := env[workerenv.GitAskpass]; got != controllerGitAskpassPath {
		t.Fatalf("%s = %q, want controller helper %q", workerenv.GitAskpass, got, controllerGitAskpassPath)
	}
}

func TestWorkspaceGitCommandUsesGitHubTokenOnlyForGitHubRepos(t *testing.T) {
	t.Setenv(workerenv.GitToken, "")
	t.Setenv(workerenv.GitHubToken, "github-token")
	t.Setenv(workerenv.GitRepo, "https://github.com/example/private.git")

	cmd := workspaceGitCommand(context.Background(), "status")
	env := map[string]string{}
	for _, entry := range cmd.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if env[workerenv.GitToken] != "github-token" {
		t.Fatalf("%s = %q, want github fallback", workerenv.GitToken, env[workerenv.GitToken])
	}

	t.Setenv(workerenv.GitRepo, "http://github.com/example/private.git")
	cmd = workspaceGitCommand(context.Background(), "status")
	env = map[string]string{}
	for _, entry := range cmd.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if env[workerenv.GitToken] != "" {
		t.Fatalf("%s = %q, want no github fallback for non-HTTPS repo", workerenv.GitToken, env[workerenv.GitToken])
	}

	t.Setenv(workerenv.GitRepo, "https://gitlab.com/example/private.git")
	cmd = workspaceGitCommand(context.Background(), "status")
	env = map[string]string{}
	for _, entry := range cmd.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if env[workerenv.GitToken] != "" {
		t.Fatalf("%s = %q, want no github fallback for non-GitHub repo", workerenv.GitToken, env[workerenv.GitToken])
	}
}

func newWorkspaceGitFixture(t *testing.T) (cloneDir, featureCommit string) {
	t.Helper()
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "init")
	testGit(t, sourceDir, "config", "user.email", "test@example.invalid")
	testGit(t, sourceDir, "config", "user.name", "Test User")
	testGit(t, sourceDir, "checkout", "-B", "main")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "add", "README.md")
	testGit(t, sourceDir, "commit", "-m", "main")
	testGit(t, sourceDir, "checkout", "-B", "feature")
	if err := os.WriteFile(filepath.Join(sourceDir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "add", "feature.txt")
	testGit(t, sourceDir, "commit", "-m", "feature")
	featureCommit = testGitOutput(t, sourceDir, "rev-parse", "HEAD")
	testGit(t, sourceDir, "checkout", "main")

	originDir := filepath.Join(root, "origin.git")
	testGit(t, root, "clone", "--bare", sourceDir, originDir)
	cloneDir = filepath.Join(root, "clone")
	testGit(t, root, "clone", originDir, cloneDir)
	return cloneDir, featureCommit
}

func testGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = testGitOutput(t, dir, args...)
}

func testGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}
