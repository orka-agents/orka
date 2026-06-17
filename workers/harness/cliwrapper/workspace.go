package cliwrapper

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sozercan/orka/internal/workerenv"
)

func workspaceGitCommand(ctx context.Context, args ...string) *exec.Cmd {
	baseArgs := []string{
		"-c", "credential.helper=",
		"-c", "core.askPass=",
	}
	cmd := exec.CommandContext(ctx, wrapperGitBinary, append(baseArgs, args...)...)
	env := []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"PATH=" + wrapperSafeCommandPath,
		"SSH_ASKPASS=/bin/false",
		"HOME=/tmp/orka-empty-git-home",
		"XDG_CONFIG_HOME=/tmp/orka-empty-git-config",
	}
	if token := strings.TrimSpace(os.Getenv(workerenv.GitToken)); token != "" {
		env = setEnv(env, workerenv.GitToken, token)
		env = setEnv(env, workerenv.GitHubToken, firstNonEmpty(os.Getenv(workerenv.GitHubToken), token))
		env = setEnv(env, workerenv.GitAskpass, firstNonEmpty(os.Getenv(workerenv.GitAskpass), "/bin/echo-token"))
		env = setEnv(env, "GIT_ASKPASS", envEntryValue(env, workerenv.GitAskpass))
		if username := strings.TrimSpace(os.Getenv(workerenv.GitUsername)); username != "" {
			env = setEnv(env, workerenv.GitUsername, username)
		}
	}
	cmd.Env = env
	return cmd
}

func validateWorkspaceRepoURL(rawRepo string) error {
	parsed, err := url.Parse(rawRepo)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("workspace git repo must be an absolute https URL")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("workspace git repo scheme %q is not supported", parsed.Scheme)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("workspace git repo URL must not include credentials, query, or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" {
		return fmt.Errorf("workspace git repo host %q is not allowed", parsed.Hostname())
	}
	return nil
}

type preparedWorkspace struct {
	workDir string
	rootDir string
	baseDir string
	cleanup func()
}

func prepareTurnWorkspace(ctx context.Context, turn TurnContext) (preparedWorkspace, error) {
	repo := strings.TrimSpace(turn.Metadata["gitRepo"])
	if repo == "" {
		root, err := os.MkdirTemp("/tmp", "orka-harness-turn-*")
		if err != nil {
			return preparedWorkspace{}, fmt.Errorf("create isolated turn workspace: %w", err)
		}
		cleanup := func() { _ = removeAllForChild(root); _ = os.RemoveAll(root) }
		workDir := filepath.Join(root, "workspace")
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			cleanup()
			return preparedWorkspace{}, fmt.Errorf("create isolated turn workdir: %w", err)
		}
		return preparedWorkspace{workDir: workDir, rootDir: root, baseDir: root, cleanup: cleanup}, nil
	}
	if err := validateWorkspaceRepoURL(repo); err != nil {
		return preparedWorkspace{}, err
	}
	root, err := os.MkdirTemp("/tmp", "orka-harness-workspace-*")
	if err != nil {
		return preparedWorkspace{}, fmt.Errorf("create turn workspace: %w", err)
	}
	cloneDir := filepath.Join(root, "repo")
	cleanup := func() { _ = removeAllForChild(cloneDir); _ = os.RemoveAll(root) }
	args := []string{"clone", "--depth=1"}
	if branch := strings.TrimSpace(turn.Metadata["gitBranch"]); branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repo, cloneDir)
	if out, err := workspaceGitCommand(ctx, args...).CombinedOutput(); err != nil {
		cleanup()
		return preparedWorkspace{}, gitCommandError("clone turn workspace", err, out, repo)
	}
	if ref := strings.TrimSpace(turn.Metadata["gitRef"]); ref != "" {
		if err := fetchAndCheckoutWorkspaceRef(ctx, cloneDir, ref, repo); err != nil {
			cleanup()
			return preparedWorkspace{}, err
		}
	}
	pushBranch := strings.TrimSpace(turn.Metadata["pushBranch"])
	gitRef := strings.TrimSpace(turn.Metadata["gitRef"])
	if pushBranch != "" && gitRef == "" {
		checkoutArgs := []string{"-C", cloneDir, "checkout", "-B", pushBranch}
		if branch := strings.TrimSpace(turn.Metadata["gitBranch"]); branch != "" {
			checkoutArgs = append(checkoutArgs, "origin/"+branch)
		}
		if out, err := workspaceGitCommand(ctx, checkoutArgs...).CombinedOutput(); err != nil {
			cleanup()
			return preparedWorkspace{}, gitCommandError("checkout workspace push branch", err, out, repo)
		}
	}
	if err := scrubWorkspaceRemote(ctx, cloneDir, repo); err != nil {
		cleanup()
		return preparedWorkspace{}, err
	}
	workDir := cloneDir
	if subPath := strings.TrimSpace(turn.Metadata["workspaceSubPath"]); subPath != "" {
		cleanSubPath := filepath.Clean(subPath)
		if filepath.IsAbs(cleanSubPath) || strings.HasPrefix(cleanSubPath, "..") {
			cleanup()
			return preparedWorkspace{}, fmt.Errorf("workspace subpath %q is invalid", subPath)
		}
		workDir = filepath.Join(cloneDir, cleanSubPath)
	}
	contained, err := containedWorkspaceDir(cloneDir, workDir)
	if err != nil {
		cleanup()
		return preparedWorkspace{}, err
	}
	return preparedWorkspace{workDir: contained, rootDir: cloneDir, baseDir: root, cleanup: cleanup}, nil
}

func fetchAndCheckoutWorkspaceRef(ctx context.Context, cloneDir, ref, repo string) error {
	fetch := workspaceGitCommand(ctx, "-C", cloneDir, "fetch", "--depth=1", "origin", ref)
	if _, err := fetch.CombinedOutput(); err == nil {
		return checkoutWorkspaceCommit(ctx, cloneDir, "FETCH_HEAD", repo)
	}
	if looksLikeCommitRef(ref) {
		if err := fetchWorkspaceRemoteHeads(ctx, cloneDir); err != nil {
			return err
		}
		return checkoutWorkspaceCommit(ctx, cloneDir, ref, repo)
	}
	branch, err := normalizeWorkspaceBranchRef(ref)
	if err != nil {
		return err
	}
	if err := fetchWorkspaceRemoteBranch(ctx, cloneDir, branch, repo); err != nil {
		return err
	}
	commit, err := resolveWorkspaceRemoteBranch(ctx, cloneDir, branch, repo)
	if err != nil {
		return err
	}
	return checkoutWorkspaceCommit(ctx, cloneDir, commit, repo)
}

func looksLikeCommitRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, r := range ref {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func normalizeWorkspaceBranchRef(ref string) (string, error) {
	branch := strings.TrimSpace(ref)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	branch = strings.TrimPrefix(branch, "origin/")
	if branch == "" || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") ||
		strings.ContainsAny(branch, " ~^:?*[\\") {
		return "", fmt.Errorf("checkout turn workspace ref: invalid ref %q", ref)
	}
	return branch, nil
}

func fetchWorkspaceRemoteBranch(ctx context.Context, cloneDir, branch, repo string) error {
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	cmd := workspaceGitCommand(ctx, "-C", cloneDir, "fetch", "--depth=1", "origin", refspec)
	if out, err := cmd.CombinedOutput(); err != nil {
		return gitCommandError("fetch turn workspace remote branch", err, out, repo)
	}
	return nil
}

func fetchWorkspaceRemoteHeads(ctx context.Context, cloneDir string) error {
	cmd := workspaceGitCommand(ctx, "-C", cloneDir, "fetch", "--depth=1", "origin", "+refs/heads/*:refs/remotes/origin/*")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch turn workspace remote heads: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func resolveWorkspaceRemoteBranch(ctx context.Context, cloneDir, branch, repo string) (string, error) {
	remoteRef := "refs/remotes/origin/" + branch
	verify := workspaceGitCommand(ctx, "-C", cloneDir, "rev-parse", "--verify", "--end-of-options", remoteRef+"^{commit}")
	out, err := verify.CombinedOutput()
	if err != nil {
		return "", gitCommandError("resolve turn workspace ref", err, out, repo)
	}
	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return "", fmt.Errorf("resolve turn workspace ref: empty commit for %q", branch)
	}
	return commit, nil
}

func checkoutWorkspaceCommit(ctx context.Context, cloneDir, ref, repo string) error {
	verify := workspaceGitCommand(ctx, "-C", cloneDir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	out, err := verify.CombinedOutput()
	if err != nil {
		return gitCommandError("resolve turn workspace ref", err, out, repo)
	}
	commit := strings.TrimSpace(string(out))
	checkout := workspaceGitCommand(ctx, "-C", cloneDir, "checkout", "--detach", commit)
	if out, err := checkout.CombinedOutput(); err != nil {
		return gitCommandError("checkout turn workspace ref", err, out, repo)
	}
	return nil
}

func gitCommandError(operation string, err error, output []byte, rawRepo string) error {
	message := strings.TrimSpace(string(output))
	if rawRepo != "" {
		message = strings.ReplaceAll(message, rawRepo, scrubbedRepoURL(rawRepo))
	}
	return fmt.Errorf("%s: %w: %s", operation, err, message)
}

func scrubbedRepoURL(rawRepo string) string {
	parsed, err := url.Parse(rawRepo)
	if err != nil {
		return rawRepo
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func scrubWorkspaceRemote(ctx context.Context, cloneDir, rawRepo string) error {
	safeRepo := scrubbedRepoURL(rawRepo)
	scrub := workspaceGitCommand(ctx, "-C", cloneDir, "remote", "set-url", "origin", safeRepo)
	if out, err := scrub.CombinedOutput(); err != nil {
		return gitCommandError("scrub workspace remote url", err, out, rawRepo)
	}
	return nil
}

func containedWorkspaceDir(root, candidate string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve workspace directory: %w", err)
	}
	info, err := os.Stat(resolvedCandidate)
	if err != nil {
		return "", fmt.Errorf("stat workspace directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %q is not a directory", candidate)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedCandidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("workspace directory %q escapes clone root", candidate)
	}
	return resolvedCandidate, nil
}
