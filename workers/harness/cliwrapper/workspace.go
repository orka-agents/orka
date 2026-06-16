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
	cmd := exec.CommandContext(ctx, "git", append(baseArgs, args...)...)
	env := []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
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
	if path := strings.TrimSpace(os.Getenv("PATH")); path != "" {
		env = append(env, "PATH="+path)
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
	cleanup func()
}

func prepareTurnWorkspace(ctx context.Context, turn TurnContext) (preparedWorkspace, error) {
	repo := strings.TrimSpace(turn.Metadata["gitRepo"])
	if repo == "" {
		root, err := os.MkdirTemp("/tmp", "orka-harness-turn-*")
		if err != nil {
			return preparedWorkspace{}, fmt.Errorf("create isolated turn workspace: %w", err)
		}
		cleanup := func() { _ = os.RemoveAll(root) }
		workDir := filepath.Join(root, "workspace")
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			cleanup()
			return preparedWorkspace{}, fmt.Errorf("create isolated turn workdir: %w", err)
		}
		return preparedWorkspace{workDir: workDir, rootDir: root, cleanup: cleanup}, nil
	}
	if err := validateWorkspaceRepoURL(repo); err != nil {
		return preparedWorkspace{}, err
	}
	root, err := os.MkdirTemp("/tmp", "orka-harness-workspace-*")
	if err != nil {
		return preparedWorkspace{}, fmt.Errorf("create turn workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	cloneDir := filepath.Join(root, "repo")
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
		fetch := workspaceGitCommand(ctx, "-C", cloneDir, "fetch", "--depth=1", "origin", ref)
		if out, err := fetch.CombinedOutput(); err != nil {
			cleanup()
			return preparedWorkspace{}, gitCommandError("fetch turn workspace ref", err, out, repo)
		}
		checkout := workspaceGitCommand(ctx, "-C", cloneDir, "checkout", "FETCH_HEAD")
		if out, err := checkout.CombinedOutput(); err != nil {
			cleanup()
			return preparedWorkspace{}, gitCommandError("checkout turn workspace ref", err, out, repo)
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
	return preparedWorkspace{workDir: contained, rootDir: cloneDir, cleanup: cleanup}, nil
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
