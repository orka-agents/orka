package cliwrapper

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/workerenv"
)

var lookupWorkspaceHostIPs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

const (
	envAllowedGitHosts       = harness.WrapperAllowedGitHostsEnv
	controllerGitAskpassPath = "/bin/echo-token"
	workspaceRefFetchDepth   = "1000"
	gitShallowTrue           = "true"
	gitShallowFalse          = "false"
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
		"GIT_NO_REPLACE_OBJECTS=1",
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
	return harness.WrapperGitHostAllowed(host, os.Getenv(envAllowedGitHosts))
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
	if err := validateWorkspaceRefForFetch(ref); err != nil {
		return err
	}
	if oid, ok := security.NormalizeFullGitObjectID(ref); ok {
		return fetchAndCheckoutWorkspaceCommitOID(ctx, cloneDir, oid, repo)
	}
	fetch := workspaceGitCommand(ctx, "-C", cloneDir, "fetch", "--depth=1", "origin", "--end-of-options", ref)
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

func fetchAndCheckoutWorkspaceCommitOID(ctx context.Context, cloneDir, oid, repo string) error {
	fetch := workspaceGitCommand(ctx, "-C", cloneDir, "fetch", "--depth=1", "origin", "--end-of-options", oid)
	if _, err := fetch.CombinedOutput(); err == nil {
		fetched, resolveErr := resolveWorkspaceCommit(ctx, cloneDir, "FETCH_HEAD", repo)
		if resolveErr == nil && strings.EqualFold(fetched, oid) {
			return checkoutWorkspaceExactCommit(ctx, cloneDir, oid, repo)
		}
	}
	if err := fetchWorkspaceRemoteCommitReachability(ctx, cloneDir, repo); err != nil {
		return err
	}
	resolved, resolveErr := resolveWorkspaceCommit(ctx, cloneDir, oid, repo)
	if resolveErr != nil || !strings.EqualFold(resolved, oid) {
		if err := fetchWorkspaceRemoteCommitFullReachability(ctx, cloneDir, repo); err != nil {
			return err
		}
	}
	return checkoutWorkspaceExactCommit(ctx, cloneDir, oid, repo)
}

func validateWorkspaceRefForFetch(ref string) error {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" || strings.HasPrefix(trimmed, "-") || strings.Contains(trimmed, "..") ||
		strings.Contains(trimmed, "@{") || strings.ContainsAny(trimmed, " ~^:?*[\\") {
		return fmt.Errorf("checkout turn workspace ref: invalid ref %q", ref)
	}
	return nil
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

func fetchWorkspaceRemoteCommitReachability(ctx context.Context, cloneDir, repo string) error {
	cmd := workspaceGitCommand(
		ctx,
		"-C", cloneDir,
		"fetch", "--depth="+workspaceRefFetchDepth,
		"origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return gitCommandError("fetch turn workspace remote refs", err, out, repo)
	}
	return nil
}

func fetchWorkspaceRemoteCommitFullReachability(ctx context.Context, cloneDir, repo string) error {
	shallow, err := workspaceRepositoryIsShallow(ctx, cloneDir, repo)
	if err != nil {
		return err
	}
	args := []string{"-C", cloneDir, "fetch"}
	if shallow {
		args = append(args, "--unshallow")
	}
	args = append(
		args,
		"origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*",
	)
	cmd := workspaceGitCommand(ctx, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return gitCommandError("fetch full turn workspace remote refs", err, out, repo)
	}
	return nil
}

func workspaceRepositoryIsShallow(ctx context.Context, cloneDir, repo string) (bool, error) {
	cmd := workspaceGitCommand(ctx, "-C", cloneDir, "rev-parse", "--is-shallow-repository")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, gitCommandError("inspect turn workspace repository depth", err, out, repo)
	}
	switch strings.TrimSpace(string(out)) {
	case gitShallowTrue:
		return true, nil
	case gitShallowFalse:
		return false, nil
	default:
		return false, fmt.Errorf(
			"inspect turn workspace repository depth: unexpected result %q",
			strings.TrimSpace(string(out)),
		)
	}
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

func resolveWorkspaceCommit(ctx context.Context, cloneDir, ref, repo string) (string, error) {
	verify := workspaceGitCommand(ctx, "-C", cloneDir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	var stderr bytes.Buffer
	verify.Stderr = &stderr
	out, err := verify.Output()
	if err != nil {
		return "", gitCommandError("resolve turn workspace ref", err, stderr.Bytes(), repo)
	}
	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return "", fmt.Errorf("resolve turn workspace ref: empty commit for %q", ref)
	}
	return commit, nil
}

func checkoutWorkspaceCommit(ctx context.Context, cloneDir, ref, repo string) error {
	commit, err := resolveWorkspaceCommit(ctx, cloneDir, ref, repo)
	if err != nil {
		return err
	}
	checkout := workspaceGitCommand(ctx, "-C", cloneDir, "checkout", "--detach", commit)
	if out, err := checkout.CombinedOutput(); err != nil {
		return gitCommandError("checkout turn workspace ref", err, out, repo)
	}
	return nil
}

func checkoutWorkspaceExactCommit(ctx context.Context, cloneDir, oid, repo string) error {
	commit, err := resolveWorkspaceCommit(ctx, cloneDir, oid, repo)
	if err != nil {
		return err
	}
	if !strings.EqualFold(commit, oid) {
		return fmt.Errorf("resolve turn workspace ref: commit %q does not match requested object ID %q", commit, oid)
	}
	if err := checkoutWorkspaceCommit(ctx, cloneDir, oid, repo); err != nil {
		return err
	}
	observed, err := resolveWorkspaceCommit(ctx, cloneDir, "HEAD", repo)
	if err != nil {
		return err
	}
	if !strings.EqualFold(observed, oid) {
		return fmt.Errorf("checkout turn workspace ref: commit %q does not match requested object ID %q", observed, oid)
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
	rel, err := filepath.Rel(resolvedRoot, resolvedCandidate)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("workspace directory %q escapes clone root", candidate)
	}
	rootHandle, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return "", fmt.Errorf("open workspace root: %w", err)
	}
	defer rootHandle.Close() //nolint:errcheck
	info, err := rootHandle.Stat(rel)
	if err != nil {
		return "", fmt.Errorf("stat workspace directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %q is not a directory", candidate)
	}
	return resolvedCandidate, nil
}
