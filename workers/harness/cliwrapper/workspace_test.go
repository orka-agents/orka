package cliwrapper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

const testWindowsOS = "windows"

func TestCleanupTurnWorkspacePathRemovesUnreadableChildTree(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("unix permission cleanup regression")
	}
	root := t.TempDir()
	stubborn := filepath.Join(root, "workspace", "stubborn")
	if err := os.MkdirAll(stubborn, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubborn, "artifact.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stubborn, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(stubborn, 0o700) }()

	if err := cleanupTurnWorkspacePath(filepath.Join(root, "workspace")); err != nil {
		t.Fatalf("cleanupTurnWorkspacePath: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists or stat failed: %v", err)
	}
}

func TestRelaxWorkspaceTreePermissionsSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("unix symlink permissions regression")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}

	if err := relaxWorkspaceTreePermissions(workspace); err != nil {
		t.Fatalf("relaxWorkspaceTreePermissions: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("symlink target mode = %o, want unchanged 0600", got)
	}
}

func TestValidateWorkspaceRefForFetchRejectsUnsafeRefs(t *testing.T) {
	for _, ref := range []string{
		"", " -branch", "-branch", "feature..main", "main@{1}",
		"feature branch", "main~1", "main:other", "main\\othere",
	} {
		t.Run(ref, func(t *testing.T) {
			if err := validateWorkspaceRefForFetch(ref); err == nil {
				t.Fatalf("validateWorkspaceRefForFetch(%q) error = nil, want rejection", ref)
			}
		})
	}
	for _, ref := range []string{"main", "feature/test", "refs/heads/main", "origin/feature"} {
		t.Run("valid_"+ref, func(t *testing.T) {
			if err := validateWorkspaceRefForFetch(ref); err != nil {
				t.Fatalf("validateWorkspaceRefForFetch(%q) error = %v, want nil", ref, err)
			}
		})
	}
}

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
	const (
		testUser     = "test-user"
		testPassword = "test-passphrase"
	)
	rawRepo := "https://" + testUser + ":" + testPassword + "@example.invalid/org/repo.git"
	err := fetchWorkspaceRemoteBranch(context.Background(), cloneDir, "missing-remote", rawRepo)
	if err == nil {
		t.Fatal("expected fetch failure")
	}
	message := err.Error()
	if strings.Contains(message, rawRepo) ||
		strings.Contains(message, testUser) ||
		strings.Contains(message, testPassword) {
		t.Fatalf("fetch error leaked credential-bearing repository URL: %v", err)
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

func TestFetchAndCheckoutWorkspaceRefFullOIDCannotBeShadowed(t *testing.T) {
	cloneDir, targetSHA, decoySHA := newWorkspaceGitOIDShadowFixture(t)

	if err := fetchAndCheckoutWorkspaceRef(
		context.Background(), cloneDir, targetSHA, "https://github.com/example/repo.git",
	); err != nil {
		t.Fatalf("fetchAndCheckoutWorkspaceRef: %v", err)
	}
	if got := testGitOutput(t, cloneDir, "rev-parse", "HEAD"); got != targetSHA {
		t.Fatalf("HEAD = %s, want exact target %s instead of shadow branch commit %s", got, targetSHA, decoySHA)
	}
	if branch := testGitOutput(t, cloneDir, "branch", "--show-current"); branch != "" {
		t.Fatalf("branch = %q, want detached HEAD for full object ID", branch)
	}
}

func TestFetchAndCheckoutWorkspaceRefFullOIDDeepensPastBoundedRefFetch(t *testing.T) {
	cloneDir, targetSHA := newWorkspaceGitDeepHistoryFixture(t)
	rejectDirectWorkspaceGitOIDFetch(t, targetSHA)

	if err := fetchAndCheckoutWorkspaceRef(
		context.Background(), cloneDir, targetSHA, "https://github.com/example/repo.git",
	); err != nil {
		t.Fatalf("fetchAndCheckoutWorkspaceRef: %v", err)
	}
	if got := testGitOutput(t, cloneDir, "rev-parse", "HEAD"); got != targetSHA {
		t.Fatalf("HEAD = %s, want exact deep target %s", got, targetSHA)
	}
	if branch := testGitOutput(t, cloneDir, "branch", "--show-current"); branch != "" {
		t.Fatalf("branch = %q, want detached HEAD for full object ID", branch)
	}
	if shallow := testGitOutput(t, cloneDir, "rev-parse", "--is-shallow-repository"); shallow != "false" {
		t.Fatalf("is-shallow-repository = %q, want full fallback to unshallow the clone", shallow)
	}
}

func TestFetchAndCheckoutWorkspaceRefFullOIDDeepensPastSHAshapedTag(t *testing.T) {
	cloneDir, targetSHA, decoySHA := newWorkspaceGitDeepOIDShadowFixture(t)
	rejectDirectWorkspaceGitOIDFetch(t, targetSHA)

	if err := fetchAndCheckoutWorkspaceRef(
		context.Background(), cloneDir, targetSHA, "https://github.com/example/repo.git",
	); err != nil {
		t.Fatalf("fetchAndCheckoutWorkspaceRef: %v", err)
	}
	if got := testGitOutput(t, cloneDir, "rev-parse", "HEAD"); got != targetSHA {
		t.Fatalf("HEAD = %s, want exact deep target %s instead of shadow tag commit %s", got, targetSHA, decoySHA)
	}
	if branch := testGitOutput(t, cloneDir, "branch", "--show-current"); branch != "" {
		t.Fatalf("branch = %q, want detached HEAD for full object ID", branch)
	}
	if shallow := testGitOutput(t, cloneDir, "rev-parse", "--is-shallow-repository"); shallow != "false" {
		t.Fatalf("is-shallow-repository = %q, want shadow mismatch to trigger full fallback", shallow)
	}
}

func TestFetchAndCheckoutWorkspaceRefExplicitSHAshapedBranchRemainsSymbolic(t *testing.T) {
	cloneDir, targetSHA, decoySHA := newWorkspaceGitOIDShadowFixture(t)

	if err := fetchAndCheckoutWorkspaceRef(
		context.Background(), cloneDir, "refs/heads/"+targetSHA, "https://github.com/example/repo.git",
	); err != nil {
		t.Fatalf("fetchAndCheckoutWorkspaceRef: %v", err)
	}
	if got := testGitOutput(t, cloneDir, "rev-parse", "HEAD"); got != decoySHA {
		t.Fatalf("HEAD = %s, want explicit branch commit %s", got, decoySHA)
	}
}

func TestFetchAndCheckoutWorkspaceRefExplicitSHAshapedTagRemainsSymbolic(t *testing.T) {
	cloneDir, targetSHA, decoySHA := newWorkspaceGitOIDShadowFixture(t)

	if err := fetchAndCheckoutWorkspaceRef(
		context.Background(), cloneDir, "refs/tags/"+targetSHA, "https://github.com/example/repo.git",
	); err != nil {
		t.Fatalf("fetchAndCheckoutWorkspaceRef: %v", err)
	}
	if got := testGitOutput(t, cloneDir, "rev-parse", "HEAD"); got != decoySHA {
		t.Fatalf("HEAD = %s, want explicit tag commit %s", got, decoySHA)
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

func newWorkspaceGitOIDShadowFixture(t *testing.T) (cloneDir, targetSHA, decoySHA string) {
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
	if err := os.WriteFile(filepath.Join(sourceDir, "target.txt"), []byte("target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "add", ".")
	testGit(t, sourceDir, "commit", "-m", "target")
	targetSHA = testGitOutput(t, sourceDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(sourceDir, "decoy.txt"), []byte("decoy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, sourceDir, "add", ".")
	testGit(t, sourceDir, "commit", "-m", "decoy")
	decoySHA = testGitOutput(t, sourceDir, "rev-parse", "HEAD")
	testGit(t, sourceDir, "branch", targetSHA, decoySHA)
	testGit(t, sourceDir, "tag", targetSHA, decoySHA)

	originDir := filepath.Join(root, "origin.git")
	testGit(t, root, "clone", "--bare", sourceDir, originDir)
	cloneDir = filepath.Join(root, "clone")
	testGit(t, root, "clone", originDir, cloneDir)
	return cloneDir, targetSHA, decoySHA
}

func newWorkspaceGitDeepHistoryFixture(t *testing.T) (cloneDir, targetSHA string) {
	cloneDir, targetSHA, _ = newWorkspaceGitDeepHistoryFixtureWithShadow(t, false)
	return cloneDir, targetSHA
}

func newWorkspaceGitDeepOIDShadowFixture(t *testing.T) (cloneDir, targetSHA, decoySHA string) {
	return newWorkspaceGitDeepHistoryFixtureWithShadow(t, true)
}

func newWorkspaceGitDeepHistoryFixtureWithShadow(t *testing.T, shadow bool) (cloneDir, targetSHA, decoySHA string) {
	t.Helper()
	depth, err := strconv.Atoi(workspaceRefFetchDepth)
	if err != nil {
		t.Fatalf("parse workspaceRefFetchDepth: %v", err)
	}
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
	mainCommit := testGitOutput(t, sourceDir, "rev-parse", "HEAD")

	commitCount := depth + 2
	var stream strings.Builder
	for i := 1; i <= commitCount; i++ {
		mark := strconv.Itoa(i)
		message := "feature " + mark
		stream.WriteString("commit refs/heads/feature\n")
		stream.WriteString("mark :" + mark + "\n")
		stream.WriteString("committer Test User <test@example.invalid> " + strconv.Itoa(i+1) + " +0000\n")
		stream.WriteString("data " + strconv.Itoa(len(message)) + "\n" + message + "\n")
		if i == 1 {
			stream.WriteString("from " + mainCommit + "\n")
		} else {
			stream.WriteString("from :" + strconv.Itoa(i-1) + "\n")
		}
		stream.WriteByte('\n')
	}
	stream.WriteString("done\n")
	fastImport := exec.Command("git", "fast-import", "--quiet")
	fastImport.Dir = sourceDir
	fastImport.Stdin = strings.NewReader(stream.String())
	if out, err := fastImport.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import: %v: %s", err, strings.TrimSpace(string(out)))
	}
	targetSHA = testGitOutput(t, sourceDir, "rev-parse", "feature~"+strconv.Itoa(commitCount-1))
	decoySHA = testGitOutput(t, sourceDir, "rev-parse", "feature")
	if shadow {
		testGit(t, sourceDir, "tag", targetSHA, decoySHA)
	}

	originDir := filepath.Join(root, "origin.git")
	testGit(t, root, "clone", "--bare", sourceDir, originDir)
	cloneDir = filepath.Join(root, "clone")
	testGit(t, root, "clone", "--depth=1", "--branch", "main", "file://"+originDir, cloneDir)
	return cloneDir, targetSHA, decoySHA
}

func rejectDirectWorkspaceGitOIDFetch(t *testing.T, oid string) {
	t.Helper()
	realGit := wrapperGitBinary
	quotedRealGit := strings.ReplaceAll(realGit, "'", "'\"'\"'")
	shim := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"is_fetch=false\n" +
		"is_oid=false\n" +
		"for arg in \"$@\"; do\n" +
		"  [ \"$arg\" = fetch ] && is_fetch=true\n" +
		"  [ \"$arg\" = " + oid + " ] && is_oid=true\n" +
		"done\n" +
		"if [ \"$is_fetch\" = true ] && [ \"$is_oid\" = true ]; then\n" +
		"  echo 'raw object ID fetch disabled by test fixture' >&2\n" +
		"  exit 128\n" +
		"fi\n" +
		"exec '" + quotedRealGit + "' \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapperGitBinary = shim
	t.Cleanup(func() { wrapperGitBinary = realGit })
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

func TestContainedWorkspaceDirRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	if _, err := containedWorkspaceDir(root, link); err == nil {
		t.Fatal("containedWorkspaceDir() error = nil, want escaping symlink rejection")
	}
}
