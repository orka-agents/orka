package cliwrapper

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
)

var lookupWorkspaceHostIPs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

const (
	envAllowedGitHosts       = "ORKA_HARNESS_WRAPPER_ALLOWED_GIT_HOSTS"
	controllerGitAskpassPath = "/bin/echo-token"
	workspaceRefFetchDepth   = "1000"
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
	gitAuth := strings.TrimSpace(os.Getenv(workerenv.GitToken))
	githubAuth := strings.TrimSpace(os.Getenv(workerenv.GitHubToken))
	cloneAuth := gitAuth
	if cloneAuth == "" && workspaceRepoUsesGitHubToken(os.Getenv(workerenv.GitRepo)) {
		cloneAuth = githubAuth
	}
	if cloneAuth != "" {
		env = setEnv(env, workerenv.GitToken, cloneAuth)
		env = setEnv(env, workerenv.GitHubToken, firstNonEmpty(githubAuth, cloneAuth))
		env = setEnv(env, workerenv.GitAskpass, controllerGitAskpassPath)
		env = setEnv(env, "GIT_ASKPASS", controllerGitAskpassPath)
		if username := strings.TrimSpace(os.Getenv(workerenv.GitUsername)); username != "" {
			env = setEnv(env, workerenv.GitUsername, username)
		}
	}
	cmd.Env = env
	return cmd
}

func workspaceRepoUsesGitHubToken(rawRepo string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawRepo))
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
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
	if err := validateWorkspaceRepoHost(host); err != nil {
		return err
	}
	return nil
}

func validateWorkspaceRepoHost(host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("workspace git repo host %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if workspaceIPBlocked(ip) {
			return fmt.Errorf("workspace git repo host %q resolves to a private or local address", host)
		}
		if !workspaceGitHostAllowed(host) {
			return fmt.Errorf("workspace git repo host %q is not allowed", host)
		}
		return nil
	}
	if !workspaceGitHostAllowed(host) {
		return fmt.Errorf("workspace git repo host %q is not allowed", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := lookupWorkspaceHostIPs(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve workspace git repo host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve workspace git repo host %q: no addresses", host)
	}
	for _, addr := range addrs {
		if workspaceIPBlocked(addr.IP) {
			return fmt.Errorf("workspace git repo host %q resolves to a private or local address", host)
		}
	}
	return nil
}

func cleanupTurnWorkspacePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	childErr := removeAllForChild(path)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	if err := relaxWorkspaceTreePermissions(path); err != nil && childErr != nil {
		return fmt.Errorf("remove workspace as child: %w; relax workspace permissions: %v", childErr, err)
	}
	if err := os.RemoveAll(path); err != nil {
		if childErr != nil {
			return fmt.Errorf("remove workspace as child: %w; remove workspace as wrapper: %v", childErr, err)
		}
		return err
	}
	return nil
}

func relaxWorkspaceTreePermissions(root string) error {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	if err := os.Chmod(root, 0o700); err != nil && !os.IsNotExist(err) {
		return err
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		if chmodErr := os.Chmod(path, mode); chmodErr != nil && !os.IsNotExist(chmodErr) {
			return chmodErr
		}
		return nil
	})
}

func workspaceGitHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if slices.Contains([]string{"github.com", "gitlab.com", "bitbucket.org"}, host) {
		return true
	}
	for item := range strings.SplitSeq(os.Getenv(envAllowedGitHosts), ",") {
		if host == strings.ToLower(strings.TrimSpace(item)) {
			return true
		}
	}
	return false
}

func workspaceIPBlocked(ip net.IP) bool {
	return ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

type preparedWorkspace struct {
	workDir      string
	rootDir      string
	baseDir      string
	ownedBaseDir bool
	cleanup      func()
}

func prepareTurnWorkspace(ctx context.Context, turn TurnContext) (preparedWorkspace, error) {
	repo := strings.TrimSpace(turn.Metadata["gitRepo"])
	if repo == "" {
		root, err := os.MkdirTemp("/tmp", "orka-harness-turn-*")
		if err != nil {
			return preparedWorkspace{}, fmt.Errorf("create isolated turn workspace: %w", err)
		}
		cleanup := func() { _ = cleanupTurnWorkspacePath(root) }
		workDir := filepath.Join(root, "workspace")
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			cleanup()
			return preparedWorkspace{}, fmt.Errorf("create isolated turn workdir: %w", err)
		}
		return preparedWorkspace{workDir: workDir, rootDir: root, baseDir: root, ownedBaseDir: true, cleanup: cleanup}, nil
	}
	if err := validateWorkspaceRepoURL(repo); err != nil {
		return preparedWorkspace{}, err
	}
	root, err := os.MkdirTemp("/tmp", "orka-harness-workspace-*")
	if err != nil {
		return preparedWorkspace{}, fmt.Errorf("create turn workspace: %w", err)
	}
	cloneDir := filepath.Join(root, "repo")
	cleanup := func() { _ = cleanupTurnWorkspacePath(root) }
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
	return preparedWorkspace{
		workDir:      contained,
		rootDir:      cloneDir,
		baseDir:      root,
		ownedBaseDir: true,
		cleanup:      cleanup,
	}, nil
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
	cmd := workspaceGitCommand(ctx, "-C", cloneDir, "fetch", "--depth="+workspaceRefFetchDepth, "origin", refspec)
	if out, err := cmd.CombinedOutput(); err != nil {
		return gitCommandError("fetch turn workspace remote branch", err, out, repo)
	}
	return nil
}

func fetchWorkspaceRemoteHeads(ctx context.Context, cloneDir string) error {
	cmd := workspaceGitCommand(
		ctx,
		"-C", cloneDir,
		"fetch", "--depth="+workspaceRefFetchDepth,
		"origin", "+refs/heads/*:refs/remotes/origin/*",
	)
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
