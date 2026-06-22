/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/redact"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

// AgentConfig holds worker configuration from environment variables.
type AgentConfig struct {
	TaskName           string
	TaskNamespace      string
	TransactionID      string
	TransactionProfile string
	Prompt             string
	Model              string
	SystemPrompt       string
	MaxTurns           int
	AllowedTools       []string
	DisallowedTools    []string
	GitRepo            string
	GitBranch          string
	GitRef             string
	PRBaseBranch       string
	PRBaseRepo         string
	PRBaseSHA          string
	SubPath            string
	TimeoutSeconds     int

	securityReviewContextArtifact string
	securityReviewContextManifest []byte
}

// LoadConfig reads and validates agent configuration from environment variables.
func LoadConfig(defaultMaxTurns int) (*AgentConfig, error) {
	return loadConfig(defaultMaxTurns, true)
}

// LoadWorkspaceConfig reads and validates workspace configuration without
// requiring an agent prompt. Container workers use this for deterministic tasks.
func LoadWorkspaceConfig() (*AgentConfig, error) {
	return loadConfig(0, false)
}

func loadConfig(defaultMaxTurns int, requirePrompt bool) (*AgentConfig, error) {
	cfg := &AgentConfig{
		TaskName:           os.Getenv(workerenv.TaskName),
		TaskNamespace:      os.Getenv(workerenv.TaskNamespace),
		TransactionID:      os.Getenv(workerenv.TransactionID),
		TransactionProfile: os.Getenv(workerenv.TransactionProfile),
		Prompt:             os.Getenv(workerenv.Prompt),
		Model:              os.Getenv(workerenv.Model),
		SystemPrompt:       os.Getenv(workerenv.SystemPrompt),
		GitRepo:            os.Getenv(workerenv.GitRepo),
		GitBranch:          os.Getenv(workerenv.GitBranch),
		GitRef:             os.Getenv(workerenv.GitRef),
		PRBaseBranch:       os.Getenv(workerenv.PRBaseBranch),
		PRBaseRepo:         os.Getenv(workerenv.PRBaseRepo),
		PRBaseSHA:          os.Getenv(workerenv.PRBaseSHA),
		SubPath:            os.Getenv(workerenv.WorkspaceSubpath),
		MaxTurns:           defaultMaxTurns,
	}

	if requirePrompt && cfg.Prompt == "" {
		return nil, fmt.Errorf("%s is required", workerenv.Prompt)
	}

	if v := os.Getenv(workerenv.MaxTurns); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", workerenv.MaxTurns, err)
		}
		cfg.MaxTurns = n
	}

	if v := os.Getenv(workerenv.AllowedTools); v != "" {
		cfg.AllowedTools = strings.Split(v, ",")
	}
	if v := os.Getenv(workerenv.DisallowedTools); v != "" {
		cfg.DisallowedTools = strings.Split(v, ",")
	}

	if v := os.Getenv(workerenv.TimeoutSeconds); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", workerenv.TimeoutSeconds, err)
		}
		cfg.TimeoutSeconds = n
	}

	// Sanitize SubPath to prevent directory traversal
	if cfg.SubPath != "" {
		cleaned := filepath.Clean(cfg.SubPath)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
			return nil, fmt.Errorf("%s %q contains path traversal", workerenv.WorkspaceSubpath, cfg.SubPath)
		}
		cfg.SubPath = cleaned
	}

	return cfg, nil
}

// SetupGitCredentials sets git credential env vars globally so both clone and
// agent-initiated git operations (push, fetch) can authenticate.
func SetupGitCredentials() {
	tokenPaths := []string{
		"/secrets/git/token",
		"/secrets/git/password",
		"/secrets/git/GITHUB_TOKEN",
	}
	for _, path := range tokenPaths {
		if data, err := os.ReadFile(path); err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				os.Setenv(workerenv.GitToken, token)               //nolint:errcheck
				os.Setenv(workerenv.GitHubToken, token)            //nolint:errcheck
				os.Setenv(workerenv.GitAskpass, "/bin/echo-token") //nolint:errcheck
				break
			}
		}
	}
	if data, err := os.ReadFile("/secrets/git/username"); err == nil {
		username := strings.TrimSpace(string(data))
		if username != "" {
			os.Setenv(workerenv.GitUsername, username) //nolint:errcheck
		}
	}
}

// CloneRepo clones the configured git repository into the workspace directory.
//
// When the workspace already contains a git repository (e.g. a sandbox
// workspace reused across turns of the same session), CloneRepo skips the
// clone and refreshes in place. Branch workspaces are fast-forwarded only
// when the configured branch is still checked out; a session-created branch
// is preserved as part of the reused workspace state.
//
// When ORKA_PUSH_BRANCH is set and no ORKA_GIT_REF pinned a specific commit,
// CloneRepo also creates and checks out a local branch with that name. This
// way any agent-initiated `git push origin HEAD` lands on the intended remote
// branch instead of the upstream default (often "main"). The post-run worker
// finalize step still owns the canonical commit + push.
func CloneRepo(ctx context.Context, cfg *AgentConfig, workspaceDir string) error {
	// Detect a reused workspace: if <workspaceDir>/.git exists we already
	// have a clone (sandbox session reuse). Re-running `git clone` would
	// fail with "destination path already exists". Refresh in place instead.
	if info, err := os.Stat(filepath.Join(workspaceDir, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
		fmt.Printf("Reusing existing git repo at %s (sandbox workspace reuse)\n", workspaceDir)
		if err := validateReusedGitRemote(ctx, workspaceDir, cfg.GitRepo); err != nil {
			return err
		}
		if cfg.GitBranch != "" && cfg.GitRef == "" {
			if err := refreshReusedGitBranch(ctx, workspaceDir, cfg.GitBranch); err != nil {
				return err
			}
		}
		if cfg.GitRef != "" {
			fetchMode, err := fetchGitRef(ctx, workspaceDir, cfg.GitRef)
			if err != nil {
				return err
			}
			if err := checkoutGitRef(ctx, workspaceDir, cfg.GitRef, fetchMode); err != nil {
				return err
			}
		}
		if cfg.GitRef == "" {
			if err := checkoutPushBranchForAgentRun(ctx, workspaceDir, cfg.GitBranch); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf("Cloning %s into %s\n", cfg.GitRepo, workspaceDir)

	args := []string{"clone"}

	if cfg.GitBranch != "" {
		args = append(args, "--branch", cfg.GitBranch)
	}

	args = append(args, "--single-branch")
	if cfg.GitRef == "" {
		// Shallow clone only when no specific commit ref is needed
		args = append(args, "--depth=1")
	}
	args = append(args, cfg.GitRepo, workspaceDir)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	// Checkout specific ref if provided (overrides branch). Validation tasks often
	// pin workspace.ref to a pushed branch head SHA without also providing the
	// branch name, so fall back to fetching all remote heads when the server does
	// not allow fetching the object by SHA directly.
	if cfg.GitRef != "" {
		fetchMode, err := fetchGitRef(ctx, workspaceDir, cfg.GitRef)
		if err != nil {
			return err
		}
		if err := checkoutGitRef(ctx, workspaceDir, cfg.GitRef, fetchMode); err != nil {
			return err
		}
	}

	// If ORKA_PUSH_BRANCH is set and we're not pinned to a specific ref, create
	// and check out a local branch with that name. This way any agent-initiated
	// `git push origin HEAD` lands on the intended remote branch rather than
	// overwriting "main" (or whatever the upstream default branch was). Skipped
	// for ref-pinned validation tasks because those aren't expected to push.
	if cfg.GitRef == "" {
		if err := checkoutPushBranchForAgentRun(ctx, workspaceDir, cfg.GitBranch); err != nil {
			return err
		}
	}

	return nil
}

func checkoutPushBranchForAgentRun(ctx context.Context, workspaceDir, baseBranch string) error {
	pushBranch := strings.TrimSpace(os.Getenv(workerenv.PushBranch))
	if pushBranch == "" {
		return nil
	}
	args := []string{"checkout", "-B", pushBranch}
	if remoteBase := remoteBranchStartPoint(ctx, workspaceDir, baseBranch); remoteBase != "" {
		args = append(args, remoteBase)
	}
	if err := execGitContext(ctx, workspaceDir, args...); err != nil {
		return fmt.Errorf("pre-checkout push branch %q failed: %w", pushBranch, err)
	}
	fmt.Printf("Pre-checked out push branch %s before agent run\n", pushBranch)
	return nil
}

func remoteBranchStartPoint(ctx context.Context, workspaceDir, branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		if err := execGitContext(ctx, workspaceDir, "fetch", "origin", "HEAD"); err != nil {
			return ""
		}
		return "FETCH_HEAD"
	}
	branch, ok := gitBranchNameFromRef(ctx, workspaceDir, branch)
	if !ok {
		return ""
	}
	remoteRef := "refs/remotes/origin/" + branch
	if err := execGitContext(ctx, workspaceDir, "rev-parse", "--verify", "--quiet", remoteRef); err != nil {
		return ""
	}
	return remoteRef
}

type gitRefFetchMode int

const (
	gitRefFetchDirect gitRefFetchMode = iota
	gitRefFetchRemoteBranch
	gitRefFetchRemoteHeads
)

func validateReusedGitRemote(ctx context.Context, workspaceDir, expectedRepo string) error {
	if strings.TrimSpace(expectedRepo) == "" {
		return nil
	}
	remoteURL, err := execGitOutputContext(ctx, workspaceDir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("git inspect origin remote on reused workspace failed: %w", err)
	}
	if strings.TrimSpace(remoteURL) != strings.TrimSpace(expectedRepo) {
		return fmt.Errorf(
			"existing git remote origin does not match configured repo (actual %q, expected %q)",
			gitRemoteForError(remoteURL),
			gitRemoteForError(expectedRepo),
		)
	}
	return nil
}

func gitRemoteForError(remote string) string {
	remote = strings.TrimSpace(remote)
	if parsed, err := url.Parse(remote); err == nil && parsed.Scheme != "" && parsed.User != nil {
		parsed.User = url.User("redacted")
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return redact.SensitiveText(parsed.String())
	}
	if at := strings.Index(remote, "@"); at > 0 && strings.Contains(remote[:at], ":") {
		return redact.SensitiveText("redacted" + remote[at:])
	}
	if parsed, err := url.Parse(remote); err == nil && parsed.Scheme != "" {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		remote = parsed.String()
	}
	return redact.SensitiveText(remote)
}

func fetchGitRef(ctx context.Context, workspaceDir, ref string) (gitRefFetchMode, error) {
	if branch, ok := gitBranchNameFromRef(ctx, workspaceDir, ref); ok {
		refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)
		if err := execGitContext(ctx, workspaceDir, "fetch", "origin", refspec); err == nil {
			return gitRefFetchRemoteBranch, nil
		}
	}
	if err := execGitContext(ctx, workspaceDir, "fetch", "origin", ref); err == nil {
		return gitRefFetchDirect, nil
	}
	if err := execGitContext(ctx, workspaceDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return gitRefFetchDirect, fmt.Errorf("git fetch ref %q failed: %w", ref, err)
	}
	return gitRefFetchRemoteHeads, nil
}

func checkoutGitRef(ctx context.Context, workspaceDir, ref string, fetchMode gitRefFetchMode) error {
	if fetchMode == gitRefFetchRemoteBranch || fetchMode == gitRefFetchRemoteHeads {
		if err := checkoutRemoteGitBranch(ctx, workspaceDir, ref); err == nil {
			return nil
		}
	}
	if fetchMode == gitRefFetchDirect {
		if err := execGitContext(ctx, workspaceDir, "checkout", "FETCH_HEAD"); err != nil {
			return fmt.Errorf("git checkout fetched ref %q failed: %w", ref, err)
		}
		return nil
	}

	if isHexGitObjectID(ref) && remoteBranchesContainRef(ctx, workspaceDir, ref) {
		if err := execGitContext(ctx, workspaceDir, "checkout", ref); err != nil {
			return fmt.Errorf("git checkout fetched commit ref %q failed: %w", ref, err)
		}
		return nil
	}
	return fmt.Errorf("git checkout ref %q failed", ref)
}

func gitBranchNameFromRef(ctx context.Context, workspaceDir, ref string) (string, bool) {
	branch := strings.TrimPrefix(ref, "refs/heads/")
	branch = strings.TrimPrefix(branch, "refs/remotes/origin/")
	branch = strings.TrimPrefix(branch, "origin/")
	if branch == "" || strings.HasPrefix(branch, "-") {
		return "", false
	}
	if _, err := execGitOutputContext(ctx, workspaceDir, "check-ref-format", "--branch", branch); err != nil {
		return "", false
	}
	return branch, true
}

func checkoutRemoteGitBranch(ctx context.Context, workspaceDir, ref string) error {
	branch, ok := gitBranchNameFromRef(ctx, workspaceDir, ref)
	if !ok {
		return fmt.Errorf("git ref %q is not a branch name", ref)
	}
	return execGitContext(ctx, workspaceDir, "checkout", "-B", branch, "origin/"+branch)
}

func isHexGitObjectID(ref string) bool {
	if len(ref) < 7 || len(ref) > 64 {
		return false
	}
	for _, r := range ref {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func remoteBranchesContainRef(ctx context.Context, workspaceDir, ref string) bool {
	out, err := execGitOutputContext(ctx, workspaceDir, "branch", "-r", "--contains", ref)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		branch := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if strings.HasPrefix(branch, "origin/") {
			return true
		}
	}
	return false
}

func refreshReusedGitBranch(ctx context.Context, workspaceDir, branch string) error {
	branch = strings.TrimSpace(branch)
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)
	if err := execGitContext(ctx, workspaceDir, "fetch", "origin", refspec); err != nil {
		return fmt.Errorf("git fetch branch %q on reused workspace failed: %w", branch, err)
	}

	currentBranch, err := execGitOutputContext(ctx, workspaceDir, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("git inspect current branch on reused workspace failed: %w", err)
	}
	if currentBranch != strings.TrimSpace(branch) {
		if currentBranch == "" {
			fmt.Printf("Reused git repo is detached; fetched origin/%s without switching\n", branch)
		} else {
			fmt.Printf("Reused git repo remains on branch %q; fetched origin/%s without switching\n", currentBranch, branch)
		}
		return nil
	}

	if err := execGitContext(ctx, workspaceDir, "merge", "--ff-only", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("git fast-forward branch %q on reused workspace failed: %w", branch, err)
	}
	return nil
}

func gitSafeDirectoryArgs(dir string, args ...string) []string {
	if strings.TrimSpace(dir) == "" {
		return args
	}

	safeDir := dir
	if absDir, err := filepath.Abs(dir); err == nil {
		safeDir = absDir
	}

	return append([]string{"-c", "safe.directory=" + safeDir, "-c", "core.hooksPath=/dev/null"}, args...)
}

func execGitOutputContext(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", gitSafeDirectoryArgs(dir, args...)...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func execGitContext(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", gitSafeDirectoryArgs(dir, args...)...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// AgentExecutor is a function that runs the agent and returns its output.
type AgentExecutor func(ctx context.Context, cfg *AgentConfig) (string, error)

const (
	agentSandboxWorkerUploadPath              = "orka-agent-worker"
	agentSandboxWorkerExecPath                = "/app/" + agentSandboxWorkerUploadPath
	agentSandboxSATokenUploadPath             = "orka-sa-token"
	agentSandboxSATokenExecPath               = "/app/" + agentSandboxSATokenUploadPath
	agentSandboxTransactionTokenUploadPath    = "orka-transaction-token"
	agentSandboxTransactionTokenExecPath      = "/app/" + agentSandboxTransactionTokenUploadPath
	agentSandboxContextSubjectTokenUploadPath = "orka-context-subject-token"
	agentSandboxContextSubjectTokenExecPath   = "/app/" + agentSandboxContextSubjectTokenUploadPath
	agentSandboxGitAskpassUploadPath          = "orka-git-askpass"
	agentSandboxGitAskpassExecPath            = "/app/" + agentSandboxGitAskpassUploadPath
	agentSandboxResultMarkerUploadPath        = "orka-result-marker"
	agentSandboxResultMarkerExecPath          = "/app/" + agentSandboxResultMarkerUploadPath
	agentSandboxResultTokenPrefix             = "ORKA_RESULT_TOKEN:"
	agentSandboxWorkerStatusUploadPath        = "orka-worker-status"
	agentSandboxWorkerStatusExecPath          = "/app/" + agentSandboxWorkerStatusUploadPath
	workspaceHandoffTokenUploadPath           = "orka-workspace-handoff-token"
	workspaceHandoffTokenDefaultPath          = "/app/" + workspaceHandoffTokenUploadPath
	agentSandboxExecMaxOutputBytes            = 2000
	agentSandboxStdoutResultMaxOutputBytes    = 256 * 1024
	workerEnvFalse                            = "false"
	workspaceHandoffTokenEnv                  = "ORKA_WORKSPACE_HANDOFF_TOKEN"
	workspaceHandoffTokenFileEnv              = "ORKA_WORKSPACE_HANDOFF_TOKEN_FILE"
	workspaceBootstrapTokenEnv                = "ORKA_WORKSPACE_BOOTSTRAP_TOKEN"
)

var (
	agentSandboxWorkspaceExecutorMu sync.RWMutex
	agentSandboxWorkspaceExecutor   workspace.WorkspaceExecutor = workspace.NewAgentSandboxExecutor()
	substrateWorkspaceExecutorMu    sync.RWMutex
	substrateWorkspaceExecutor      workspace.WorkspaceExecutor
	substrateWorkspaceExecutorErr   error
	setupGitCredentialsForRunAgent  = SetupGitCredentials
)

var errExecutionWorkspaceSecretScrubFailed = errors.New("execution workspace secret scrub failed")

func getAgentSandboxWorkspaceExecutor() workspace.WorkspaceExecutor {
	agentSandboxWorkspaceExecutorMu.RLock()
	defer agentSandboxWorkspaceExecutorMu.RUnlock()
	return agentSandboxWorkspaceExecutor
}

var newEventRecorderFromEnv = NewHTTPEventRecorderFromEnv

func setAgentSandboxWorkspaceExecutorForTest(executor workspace.WorkspaceExecutor) func() {
	agentSandboxWorkspaceExecutorMu.Lock()
	previous := agentSandboxWorkspaceExecutor
	agentSandboxWorkspaceExecutor = executor
	agentSandboxWorkspaceExecutorMu.Unlock()

	return func() {
		agentSandboxWorkspaceExecutorMu.Lock()
		agentSandboxWorkspaceExecutor = previous
		agentSandboxWorkspaceExecutorMu.Unlock()
	}
}

func setSubstrateWorkspaceExecutorForTest(executor workspace.WorkspaceExecutor, err error) func() {
	substrateWorkspaceExecutorMu.Lock()
	previousExecutor := substrateWorkspaceExecutor
	previousErr := substrateWorkspaceExecutorErr
	substrateWorkspaceExecutor = executor
	substrateWorkspaceExecutorErr = err
	substrateWorkspaceExecutorMu.Unlock()

	return func() {
		substrateWorkspaceExecutorMu.Lock()
		substrateWorkspaceExecutor = previousExecutor
		substrateWorkspaceExecutorErr = previousErr
		substrateWorkspaceExecutorMu.Unlock()
	}
}

func getSubstrateWorkspaceExecutor() (workspace.WorkspaceExecutor, error) {
	substrateWorkspaceExecutorMu.RLock()
	executor := substrateWorkspaceExecutor
	err := substrateWorkspaceExecutorErr
	substrateWorkspaceExecutorMu.RUnlock()
	if executor != nil || err != nil {
		return executor, err
	}

	substrateWorkspaceExecutorMu.Lock()
	defer substrateWorkspaceExecutorMu.Unlock()
	if substrateWorkspaceExecutor != nil || substrateWorkspaceExecutorErr != nil {
		return substrateWorkspaceExecutor, substrateWorkspaceExecutorErr
	}
	substrateEnv := workerenv.ParseSubstrateEnv(os.Getenv)
	substrateWorkspaceExecutor, substrateWorkspaceExecutorErr = workspace.NewSubstrateExecutor(workspace.SubstrateConfig{
		APIEndpoint:             substrateEnv.APIEndpoint,
		APICAFile:               substrateEnv.APICAFile,
		APIInsecureSkipVerify:   substrateEnv.APIInsecureSkipVerify,
		RouterURL:               substrateEnv.RouterURL,
		ActorDNSSuffix:          substrateEnv.ActorDNSSuffix,
		SessionIdentityToken:    substrateEnv.SessionIdentityToken,
		SessionIdentityAudience: strings.Split(substrateEnv.SessionIdentityAudience, ","),
		SessionIdentityAppID:    substrateEnv.SessionIdentityAppID,
		SessionIdentityUserID:   substrateEnv.SessionIdentityUserID,
		SessionIdentityRequired: substrateEnv.SessionIdentityRequired,
		SessionIdentityMintCert: substrateEnv.SessionIdentityMintCert,
	})
	return substrateWorkspaceExecutor, substrateWorkspaceExecutorErr
}

// RunAgent orchestrates the common agent worker lifecycle: signal handling,
// config loading, git setup, workspace preparation, agent execution, and
// result submission.
func RunAgent(name, workspaceDir string, defaultMaxTurns int, executor AgentExecutor) (err error) {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGTERM, syscall.SIGINT,
	)
	defer cancel()
	eventRecorder := newEventRecorderFromEnv()
	taskName := os.Getenv(workerenv.TaskName)

	// Populate git credential env vars before the sandbox handoff so the inner
	// worker can clone private repositories without mounting the outer secret.
	setupGitCredentialsForRunAgent()

	if workspaceEnv := workerenv.ParseExecutionWorkspaceEnv(os.Getenv); workspaceEnv.Enabled {
		if workspaceEnv.Depth > 0 {
			err := fmt.Errorf(
				"execution workspace recursion detected: %s=%d",
				workerenv.ExecutionWorkspaceDepth,
				workspaceEnv.Depth,
			)
			recordAgentWorkerFailedEvent(eventRecorder, name, taskName, err)
			return err
		}
		if err := runAgentInWorkspace(ctx, name, workspaceDir, workspaceEnv); err != nil {
			recordAgentWorkerFailedEvent(eventRecorder, name, taskName, err)
			return err
		}
		return nil
	}

	if sandboxEnv := workerenv.ParseAgentSandboxEnv(os.Getenv); sandboxEnv.Enabled {
		if depth := agentSandboxDepth(os.Getenv(workerenv.AgentSandboxDepth)); depth > 0 {
			err := fmt.Errorf("agent sandbox recursion detected: %s=%d", workerenv.AgentSandboxDepth, depth)
			recordAgentWorkerFailedEvent(eventRecorder, name, taskName, err)
			return err
		}
		if err := runAgentInSandbox(ctx, name, workspaceDir, sandboxEnv); err != nil {
			recordAgentWorkerFailedEvent(eventRecorder, name, taskName, err)
			return err
		}
		return nil
	}

	defer func() {
		if err != nil {
			recordAgentWorkerFailedEvent(eventRecorder, name, taskName, err)
			return
		}
		RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeWorkerCompleted, 0,
			WithEventTaskName(taskName),
			WithEventAgentName(name),
			WithEventSummary("agent worker completed"),
		)
	}()

	cfg, err := LoadConfig(defaultMaxTurns)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	taskName = cfg.TaskName

	fmt.Printf("Worker %s started task=%s/%s%s\n",
		name, cfg.TaskNamespace, cfg.TaskName, workerenv.TransactionLogFields(cfg.TransactionID, cfg.TransactionProfile))
	RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkerStarted,
		WithEventTaskName(cfg.TaskName),
		WithEventAgentName(name),
		WithEventSummary("agent worker started"),
		WithEventContent(agentRuntimeEventContent(map[string]any{
			"runtime": name,
			"model":   cfg.Model,
		})),
	)
	RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkspacePreparationStarted,
		WithEventTaskName(cfg.TaskName),
		WithEventAgentName(name),
		WithEventSummary("workspace preparation started"),
	)
	preparedWorkspace, err := prepareGitWorkspaceForRun(ctx, cfg, workspaceDir)
	if err != nil {
		RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkspacePreparationFailed,
			WithEventSeverity(events.ExecutionEventSeverityError),
			WithEventTaskName(cfg.TaskName),
			WithEventAgentName(name),
			WithEventSummary(err.Error()),
		)
		return err
	}

	// Apply prior task diff if iterating
	if !preparedWorkspace {
		if err := PrepareWorkspace(workspaceDir); err != nil {
			RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkspacePreparationFailed,
				WithEventSeverity(events.ExecutionEventSeverityError),
				WithEventTaskName(cfg.TaskName),
				WithEventAgentName(name),
				WithEventSummary(err.Error()),
			)
			return fmt.Errorf("workspace preparation failed: %w", err)
		}
		if err := PreparePullRequestReviewContext(workspaceDir, cfg); err != nil {
			RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkspacePreparationFailed,
				WithEventSeverity(events.ExecutionEventSeverityError),
				WithEventTaskName(cfg.TaskName),
				WithEventAgentName(name),
				WithEventSummary(err.Error()),
			)
			return fmt.Errorf("pull request review context preparation failed: %w", err)
		}
	}
	if err := EnsureWorkspaceArtifactsLink(workspaceDir); err != nil {
		RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkspacePreparationFailed,
			WithEventSeverity(events.ExecutionEventSeverityError),
			WithEventTaskName(cfg.TaskName),
			WithEventAgentName(name),
			WithEventSummary(err.Error()),
		)
		return fmt.Errorf("artifact workspace setup failed: %w", err)
	}
	if err := PrepareSecurityReviewContext(workspaceDir, cfg); err != nil {
		RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkspacePreparationFailed,
			WithEventSeverity(events.ExecutionEventSeverityError),
			WithEventTaskName(cfg.TaskName),
			WithEventAgentName(name),
			WithEventSummary(err.Error()),
		)
		return fmt.Errorf("security review context preparation failed: %w", err)
	}
	RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeWorkspacePreparationCompleted,
		WithEventTaskName(cfg.TaskName),
		WithEventAgentName(name),
		WithEventSummary("workspace preparation completed"),
	)

	RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeAgentRuntimeStarted,
		WithEventTaskName(cfg.TaskName),
		WithEventAgentName(name),
		WithEventSummary("agent runtime started"),
		WithEventContent(agentRuntimeEventContent(map[string]any{
			"runtime": name,
			"model":   cfg.Model,
		})),
	)
	RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeAgentRuntimeCommandStarted,
		WithEventTaskName(cfg.TaskName),
		WithEventAgentName(name),
		WithEventSummary("agent runtime command started"),
		WithEventContent(agentRuntimeEventContent(map[string]any{
			"runtime": name,
		})),
	)
	result, err := executor(ctx, cfg)
	if err != nil {
		RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeAgentRuntimeFailed, 0,
			WithEventSeverity(events.ExecutionEventSeverityError),
			WithEventTaskName(cfg.TaskName),
			WithEventAgentName(name),
			WithEventSummary(err.Error()),
		)
		// On failure, still try to submit partial result with any diffs
		errorOutput := fmt.Sprintf("Error: %v\n\n%s", err, result)
		resultDir := ""
		if cfg.GitRepo != "" {
			resultDir = workspaceDir
		}
		resultBytes, finalizeErr := finalizeAgentResult(resultDir, errorOutput)
		if finalizeErr != nil {
			fmt.Fprintf(os.Stderr, "failed to finalize error result: %v\n", finalizeErr)
			resultBytes = []byte(errorOutput)
		}
		if submitErr := SubmitResult(resultBytes); submitErr != nil {
			fmt.Fprintf(os.Stderr, "failed to submit error result: %v\n", submitErr)
		} else {
			RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeResultSubmitted, 0,
				WithEventTaskName(cfg.TaskName),
				WithEventAgentName(name),
				WithEventSummary("agent worker submitted partial result"),
				WithEventContent(agentRuntimeEventContent(map[string]any{"resultBytes": len(resultBytes)})),
			)
		}
		if restoreErr := RestoreSecurityReviewContextArtifact(cfg); restoreErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to restore security review context artifact: %v\n", restoreErr)
		}
		if artifactErr := UploadArtifacts(); artifactErr != nil {
			fmt.Fprintf(os.Stderr, "warning: artifact upload failed: %v\n", artifactErr)
			recordAgentArtifactUploadEvent(eventRecorder, name, cfg.TaskName, false, artifactErr)
		} else {
			recordAgentArtifactUploadEvent(eventRecorder, name, cfg.TaskName, true, nil)
		}
		return fmt.Errorf("%s execution failed: %w", name, err)
	}
	RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeAgentRuntimeCompleted,
		WithEventTaskName(cfg.TaskName),
		WithEventAgentName(name),
		WithEventSummary("agent runtime completed"),
		WithEventContent(agentRuntimeEventContent(map[string]any{"resultChars": len([]rune(result))})),
	)

	// Build structured result with diff if workspace has changes
	if result == "" {
		fmt.Fprintf(os.Stderr, "warning: %s executor returned empty result\n", name)
		result = fmt.Sprintf("%s completed without a final message", name)
	}
	resultDir := ""
	if cfg.GitRepo != "" {
		resultDir = workspaceDir
	}
	resultBytes, err := finalizeAgentResult(resultDir, result)
	if err != nil {
		return fmt.Errorf("failed to finalize result: %w", err)
	}
	if err := SubmitResult(resultBytes); err != nil {
		return fmt.Errorf("failed to submit result: %w", err)
	}
	RecordEvent(ctx, eventRecorder, events.ExecutionEventTypeResultSubmitted,
		WithEventTaskName(cfg.TaskName),
		WithEventAgentName(name),
		WithEventSummary("agent worker submitted result"),
		WithEventContent(agentRuntimeEventContent(map[string]any{"resultBytes": len(resultBytes)})),
	)
	if err := RestoreSecurityReviewContextArtifact(cfg); err != nil {
		return fmt.Errorf("failed to restore security review context artifact: %w", err)
	}
	if err := UploadArtifacts(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: artifact upload failed: %v\n", err)
		recordAgentArtifactUploadEvent(eventRecorder, name, cfg.TaskName, false, err)
	} else {
		recordAgentArtifactUploadEvent(eventRecorder, name, cfg.TaskName, true, nil)
	}

	fmt.Printf("Task %s/%s completed successfully%s\n",
		cfg.TaskNamespace, cfg.TaskName, workerenv.TransactionLogFields(cfg.TransactionID, cfg.TransactionProfile))
	return nil
}

func agentRuntimeEventContent(values map[string]any) json.RawMessage {
	data, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	return json.RawMessage(data)
}

func recordAgentArtifactUploadEvent(recorder EventRecorder, agentName, taskName string, success bool, err error) {
	eventType := events.ExecutionEventTypeArtifactUploadCompleted
	severity := events.ExecutionEventSeverityInfo
	summary := "agent worker artifact upload completed"
	content := map[string]any{"artifact": "all"}
	if !success {
		eventType = events.ExecutionEventTypeArtifactUploadFailed
		severity = events.ExecutionEventSeverityWarning
		summary = "agent worker artifact upload failed"
		if err != nil {
			content["error"] = err.Error()
		}
	}
	RecordEventWithTimeout(recorder, eventType, 0,
		WithEventSeverity(severity),
		WithEventTaskName(taskName),
		WithEventAgentName(agentName),
		WithEventSummary(summary),
		WithEventContent(agentRuntimeEventContent(content)),
	)
}

func recordAgentWorkerFailedEvent(recorder EventRecorder, agentName, taskName string, err error) {
	if err == nil {
		return
	}
	RecordEventWithTimeout(recorder, events.ExecutionEventTypeWorkerFailed, 0,
		WithEventSeverity(events.ExecutionEventSeverityError),
		WithEventTaskName(taskName),
		WithEventAgentName(agentName),
		WithEventSummary(err.Error()),
	)
}

func prepareGitWorkspaceForRun(ctx context.Context, cfg *AgentConfig, workspaceDir string) (bool, error) {
	if cfg.GitRepo == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); err == nil {
		return prepareExistingGitWorkspaceForRun(ctx, cfg, workspaceDir)
	} else if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("stat workspace: %w", err)
	}
	if err := CloneRepo(ctx, cfg, workspaceDir); err != nil {
		return false, fmt.Errorf("git clone failed: %w", err)
	}
	return false, nil
}

func prepareExistingGitWorkspaceForRun(ctx context.Context, cfg *AgentConfig, workspaceDir string) (bool, error) {
	if workerenv.IsTrue(os.Getenv(workerenv.WorkspacePrepared)) {
		fmt.Printf("Using prepared git workspace at %s\n", workspaceDir)
		return true, nil
	}
	if !managedExecutionWorkspaceGitCheckout() {
		return false, fmt.Errorf(
			"workspace %s already contains a git checkout but %s is not true",
			workspaceDir,
			workerenv.WorkspacePrepared,
		)
	}
	if err := validateManagedReusedGitDir(workspaceDir); err != nil {
		return false, err
	}
	disableUntrustedGitConfigForRun()
	if err := sanitizeReusedGitConfig(ctx, workspaceDir); err != nil {
		return false, err
	}
	if err := validateReusedGitTopLevel(ctx, workspaceDir); err != nil {
		return false, err
	}
	if err := validateReusedGitRemote(ctx, workspaceDir, cfg.GitRepo); err != nil {
		return false, err
	}
	if err := cleanManagedPushWorkspace(ctx, workspaceDir); err != nil {
		return false, err
	}
	if err := CloneRepo(ctx, cfg, workspaceDir); err != nil {
		return false, fmt.Errorf("git clone failed: %w", err)
	}
	return false, nil
}

func sanitizeReusedGitConfig(ctx context.Context, workspaceDir string) error {
	remoteURL, err := execGitOutputContext(ctx, workspaceDir, "config", "--local", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(remoteURL) == "" {
		return fmt.Errorf("inspect reused git origin remote failed: %w", err)
	}
	if strings.ContainsAny(remoteURL, "\r\n") {
		return fmt.Errorf("reused git origin remote contains a newline")
	}
	repositoryFormatVersion := reusedGitConfigValue(ctx, workspaceDir, "core.repositoryformatversion", "0")
	extensions, err := reusedGitExtensions(ctx, workspaceDir)
	if err != nil {
		return err
	}
	configPath := filepath.Join(workspaceDir, ".git", "config")
	var minimalConfig strings.Builder
	fmt.Fprintf(&minimalConfig, `[core]
	repositoryformatversion = %s
	filemode = true
	bare = false
	logallrefupdates = true
[remote "origin"]
	url = %s
	fetch = +refs/heads/*:refs/remotes/origin/*
`, repositoryFormatVersion, remoteURL)
	if len(extensions) > 0 {
		minimalConfig.WriteString("[extensions]\n")
		for _, extension := range extensions {
			fmt.Fprintf(&minimalConfig, "\t%s = %s\n", extension.key, extension.value)
		}
	}
	if err := removeReusedGitWorktreeConfig(workspaceDir); err != nil {
		return err
	}
	if err := os.WriteFile(configPath, []byte(minimalConfig.String()), 0o600); err != nil {
		return fmt.Errorf("rewrite reused git config: %w", err)
	}
	return nil
}

type reusedGitExtension struct {
	key   string
	value string
}

func reusedGitConfigValue(ctx context.Context, workspaceDir, name, fallback string) string {
	value, err := execGitOutputContext(ctx, workspaceDir, "config", "--local", "--get", name)
	if err != nil || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
		return fallback
	}
	return value
}

func reusedGitExtensions(ctx context.Context, workspaceDir string) ([]reusedGitExtension, error) {
	out, err := execGitOutputContext(ctx, workspaceDir, "config", "--local", "--get-regexp", "^extensions\\.")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, nil
	}
	extensions := []reusedGitExtension{}
	for line := range strings.Lines(out) {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.HasPrefix(strings.ToLower(name), "extensions.") || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("invalid reused git extension config %q", line)
		}
		key := strings.TrimPrefix(name, "extensions.")
		if strings.EqualFold(key, "worktreeConfig") {
			continue
		}
		extensions = append(extensions, reusedGitExtension{
			key:   key,
			value: strings.TrimSpace(value),
		})
	}
	return extensions, nil
}

func removeReusedGitWorktreeConfig(workspaceDir string) error {
	path := filepath.Join(workspaceDir, ".git", "config.worktree")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove reused git worktree config: %w", err)
	}
	return nil
}

func validateManagedReusedGitDir(workspaceDir string) error {
	gitPath := filepath.Join(workspaceDir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return fmt.Errorf("inspect reused git dir: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("managed reused workspace requires %s to be an in-workspace directory", gitPath)
	}
	if _, err := os.Lstat(filepath.Join(gitPath, "commondir")); err == nil {
		return fmt.Errorf("managed reused workspace does not support git common-dir indirection")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect reused git common-dir: %w", err)
	}
	return nil
}

func validateReusedGitTopLevel(ctx context.Context, workspaceDir string) error {
	topLevel, err := execGitOutputContext(ctx, workspaceDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("inspect reused git top-level failed: %w", err)
	}
	want, err := filepath.Abs(workspaceDir)
	if err != nil {
		return fmt.Errorf("resolve workspace path: %w", err)
	}
	got, err := filepath.Abs(topLevel)
	if err != nil {
		return fmt.Errorf("resolve reused git top-level: %w", err)
	}
	if resolvedWant, err := filepath.EvalSymlinks(want); err == nil {
		want = resolvedWant
	}
	if resolvedGot, err := filepath.EvalSymlinks(got); err == nil {
		got = resolvedGot
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		return fmt.Errorf("reused git top-level %q does not match workspace %q", got, want)
	}
	return nil
}

func cleanManagedPushWorkspace(ctx context.Context, workspaceDir string) error {
	if strings.TrimSpace(os.Getenv(workerenv.PushBranch)) == "" {
		return nil
	}
	if err := execGitContext(ctx, workspaceDir, "reset", "--hard"); err != nil {
		return fmt.Errorf("reset managed reused workspace failed: %w", err)
	}
	if err := execGitContext(ctx, workspaceDir, "clean", "-ffdx"); err != nil {
		return fmt.Errorf("clean managed reused workspace failed: %w", err)
	}
	return nil
}

func managedExecutionWorkspaceGitCheckout() bool {
	return workerenv.ParseExecutionWorkspaceEnv(os.Getenv).Depth > 0
}

func disableUntrustedGitConfigForRun() {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	appendGitConfigEnv("core.hooksPath", "/dev/null")
}

func appendGitConfigEnv(key, value string) {
	count, err := strconv.Atoi(strings.TrimSpace(os.Getenv(workerenv.GitConfigCount)))
	if err != nil || count < 0 {
		count = 0
	}
	_ = os.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", count), key)
	_ = os.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", count), value)
	_ = os.Setenv(workerenv.GitConfigCount, strconv.Itoa(count+1))
}

func finalizeAgentResult(resultDir string, result string) ([]byte, error) {
	if workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return []byte(result), nil
	}
	return FinalizeResult(resultDir, result)
}

func runAgentInWorkspace(
	ctx context.Context,
	name string,
	workspaceDir string,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
) error {
	handoffToken, err := ensureWorkspaceHandoffToken(workspaceEnv)
	if err != nil {
		return err
	}
	executor, err := executionWorkspaceExecutor(workspaceEnv)
	if err != nil {
		return err
	}
	if executor == nil {
		return fmt.Errorf("execution workspace executor is not configured for provider %q", workspaceEnv.Provider)
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve %s executable for workspace: %w", name, err)
	}
	taskNamespace := os.Getenv(workerenv.TaskNamespace)
	taskName := os.Getenv(workerenv.TaskName)
	templateNamespace := workspaceTemplateNamespace(workspaceEnv, taskNamespace)
	claimNamespace := workspaceClaimNamespace(workspaceEnv, taskNamespace, templateNamespace)
	claimName := workspaceClaimName(workspaceEnv, claimNamespace, taskNamespace, templateNamespace)

	claim, err := executor.Claim(ctx, workspace.ClaimRequest{
		Namespace:       claimNamespace,
		TaskName:        taskName,
		ClaimName:       claimName,
		CreateIfMissing: true,
		Template: workspace.TemplateRef{
			Namespace: templateNamespace,
			Name:      workspaceEnv.TemplateName,
		},
		ReuseKey:       workspaceEnv.ReuseKey,
		WarmPoolPolicy: workspaceWarmPoolPolicy(workspaceEnv),
		Timeout:        workspaceEnv.ClaimTimeout,
		// Size the workspace executor's HTTP transport for long single Exec
		// calls. Without this, the SDK transport's per-attempt response-header
		// timeout is sized for the short claim window and a multi-minute agent
		// exec fails with "timeout awaiting response headers". Mirrors the
		// agent-sandbox-specific path in runAgentInSandbox.
		MaxRequestTimeout: workspaceEnv.CommandTimeout,
	})
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonClaimFailed,
			false,
			"workspace claim failed",
		)
		return fmt.Errorf("claim execution workspace: %w", err)
	}
	ref := claim.Ref
	cleaned := false
	substrateHandoffBootstrapped := false
	defer func() {
		if cleaned {
			return
		}
		cleanupCtx, cleanupCancel := agentSandboxCleanupContext(workspaceEnv.ClaimTimeout)
		defer cleanupCancel()
		cleanupEnv, cleanupOptions := preTerminalExecutionWorkspaceCleanup(
			workspaceEnv,
			substrateHandoffBootstrapped,
			claim.Created && !claim.Reused,
		)
		if err := cleanupExecutionWorkspaceWithOptions(
			cleanupCtx,
			executor,
			ref,
			cleanupEnv,
			claim.Reused,
			executionWorkspaceDeferredCleanupSubmitsStatus(cleanupEnv),
			cleanupOptions,
		); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to clean up execution workspace: %v\n", err)
		}
	}()
	submitExecutionWorkspaceStatus(
		workspaceEnv,
		corev1alpha1.ExecutionWorkspacePhasePending,
		corev1alpha1.ExecutionWorkspaceReasonClaimed,
		claim.Reused,
		"workspace claimed",
	)

	ready, err := executor.WaitReady(ctx, workspace.WaitReadyRequest{
		Ref:                ref,
		Timeout:            workspaceEnv.ClaimTimeout,
		Boot:               workspaceEnv.Boot,
		SnapshotRestoreURI: workspaceEnv.SnapshotRestoreURI,
	})
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonReadinessFailed,
			claim.Reused,
			"workspace readiness failed",
		)
		return fmt.Errorf("wait for execution workspace: %w", err)
	}
	readyStatusOptions := []executionWorkspaceStatusOption{withExecutionWorkspaceReadyResult(ready)}
	submitExecutionWorkspaceStatus(
		workspaceEnv,
		corev1alpha1.ExecutionWorkspacePhaseReady,
		corev1alpha1.ExecutionWorkspaceReasonReady,
		claim.Reused,
		"workspace ready",
		readyStatusOptions...,
	)

	if handoffToken != "" {
		if err := bootstrapWorkspaceHandoffToken(ctx, executor, ref, handoffToken, workspaceEnv); err != nil {
			submitExecutionWorkspaceStatus(
				workspaceEnv,
				corev1alpha1.ExecutionWorkspacePhaseFailed,
				corev1alpha1.ExecutionWorkspaceReasonHandoffFailed,
				claim.Reused,
				"workspace handoff failed",
				readyStatusOptions...,
			)
			return err
		}
		substrateHandoffBootstrapped = true
	}

	command, innerEnv, err := stageAgentSandboxExecutable(
		ctx,
		executor,
		ref,
		executable,
		os.Args[1:],
		workspaceInnerEnv(os.Environ(), workspaceEnv),
		workspaceEnv.CommandTimeout,
	)
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonHandoffFailed,
			claim.Reused,
			"workspace handoff failed",
			readyStatusOptions...,
		)
		return err
	}
	stdoutResultToken := innerEnv[workerenv.ResultStdoutToken]

	execResult, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:            ref,
		Command:        command,
		Env:            innerEnv,
		WorkDir:        workspaceDir,
		Timeout:        workspaceEnv.CommandTimeout,
		MaxOutputBytes: sandboxExecMaxOutputBytes(),
		Resident:       executionWorkspaceResidentProcess(workspaceEnv),
		ResidentKey:    executionWorkspaceResidentKey(workspaceEnv, ref),
	})
	if err != nil {
		forwardWorkspaceStdoutResultMarkerIfPresent(
			ctx, executor, ref, workspaceEnv.CommandTimeout, execResult, stdoutResultToken,
		)
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
			readyStatusOptions...,
		)
		return fmt.Errorf("%s workspace execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}
	if execResult != nil && !execResult.Succeeded() {
		forwardWorkspaceStdoutResultMarkerIfPresent(
			ctx, executor, ref, workspaceEnv.CommandTimeout, execResult, stdoutResultToken,
		)
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
			readyStatusOptions...,
		)
		return fmt.Errorf(
			"%s workspace execution failed: command exited with code %d%s",
			name,
			execResult.ExitCode,
			formatSandboxExecOutput(execResult),
		)
	}

	marker, err := workspaceStdoutResultMarker(
		ctx, executor, ref, workspaceEnv.CommandTimeout, execResult, stdoutResultToken,
	)
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
			readyStatusOptions...,
		)
		return fmt.Errorf("%s workspace execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}

	cleanupCtx, cleanupCancel := agentSandboxCleanupContext(workspaceEnv.ClaimTimeout)
	defer cleanupCancel()
	if err := cleanupExecutionWorkspaceWithOptions(
		cleanupCtx,
		executor,
		ref,
		workspaceEnv,
		claim.Reused,
		true,
		executionWorkspaceCleanupOptions{statusOptions: readyStatusOptions},
	); err != nil {
		reason := corev1alpha1.ExecutionWorkspaceReasonCleanupFailed
		if errors.Is(err, errExecutionWorkspaceSecretScrubFailed) {
			reason = corev1alpha1.ExecutionWorkspaceReasonSecretScrubFailed
		}
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			reason,
			claim.Reused,
			"workspace cleanup failed",
			readyStatusOptions...,
		)
		return fmt.Errorf("execution workspace cleanup failed: %w", err)
	}
	cleaned = true

	if marker != "" {
		fmt.Println(marker)
	}

	fmt.Println(executionWorkspaceCompletionMessage(taskNamespace, taskName, workspaceEnv, ref))
	return nil
}

func executionWorkspaceCompletionMessage(
	taskNamespace string,
	taskName string,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	ref workspace.WorkspaceRef,
) string {
	provider := strings.TrimSpace(workspaceEnv.Provider)
	if provider == "" {
		provider = string(corev1alpha1.WorkspaceProviderAgentSandbox)
	}
	if provider == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return fmt.Sprintf("Task %s/%s completed in %s workspace", taskNamespace, taskName, provider)
	}
	if claimName := strings.TrimSpace(ref.ClaimName); claimName != "" {
		return fmt.Sprintf("Task %s/%s completed in %s workspace %s", taskNamespace, taskName, provider, claimName)
	}
	return fmt.Sprintf("Task %s/%s completed in %s workspace", taskNamespace, taskName, provider)
}

func runAgentInSandbox(ctx context.Context, name, workspaceDir string, sandboxEnv workerenv.AgentSandboxEnv) error {
	executor := getAgentSandboxWorkspaceExecutor()
	if executor == nil {
		return fmt.Errorf("agent sandbox workspace executor is not configured")
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve %s executable for sandbox: %w", name, err)
	}
	taskNamespace := os.Getenv(workerenv.TaskNamespace)
	taskName := os.Getenv(workerenv.TaskName)
	templateNamespace := agentSandboxTemplateNamespace(sandboxEnv, taskNamespace)
	claimNamespace := agentSandboxClaimNamespace(sandboxEnv, taskNamespace, templateNamespace)

	claim, err := executor.Claim(ctx, workspace.ClaimRequest{
		Namespace:       claimNamespace,
		TaskName:        taskName,
		ClaimName:       agentSandboxSessionClaimName(sandboxEnv, claimNamespace, taskNamespace, templateNamespace),
		CreateIfMissing: true,
		Template: workspace.TemplateRef{
			Namespace: templateNamespace,
			Name:      sandboxEnv.TemplateName,
		},
		ReuseKey:       sandboxEnv.ReuseKey,
		WarmPoolPolicy: agentSandboxClaimWarmPoolPolicy(sandboxEnv.WarmPoolPolicy),
		Timeout:        sandboxEnv.ClaimTimeout,
		// The same HTTP client built during Claim will be reused for the
		// subsequent Exec call, which can run for up to CommandTimeout.
		// Surface the larger budget so the SDK's transport-level timeouts
		// (PerAttemptTimeout / ResponseHeaderTimeout) are sized for the
		// longest expected request, not just the claim handshake.
		MaxRequestTimeout: sandboxEnv.CommandTimeout,
	})
	if err != nil {
		return fmt.Errorf("claim agent sandbox workspace: %w", err)
	}
	ref := claim.Ref
	defer func() {
		cleanupCtx, cleanupCancel := agentSandboxCleanupContext(sandboxEnv.ClaimTimeout)
		defer cleanupCancel()
		cleanupAgentSandboxWorkspace(cleanupCtx, executor, ref, sandboxEnv)
	}()

	if _, err := executor.WaitReady(ctx, workspace.WaitReadyRequest{
		Ref:     ref,
		Timeout: sandboxEnv.ClaimTimeout,
	}); err != nil {
		return fmt.Errorf("wait for agent sandbox workspace: %w", err)
	}

	command, innerEnv, err := stageAgentSandboxExecutable(
		ctx,
		executor,
		ref,
		executable,
		os.Args[1:],
		agentSandboxInnerEnv(os.Environ()),
		sandboxEnv.CommandTimeout,
	)
	if err != nil {
		return err
	}
	stdoutResultToken := innerEnv[workerenv.ResultStdoutToken]

	execResult, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:            ref,
		Command:        command,
		Env:            innerEnv,
		WorkDir:        workspaceDir,
		Timeout:        sandboxEnv.CommandTimeout,
		MaxOutputBytes: sandboxExecMaxOutputBytes(),
	})
	if err != nil {
		forwardWorkspaceStdoutResultMarkerIfPresent(
			ctx, executor, ref, sandboxEnv.CommandTimeout, execResult, stdoutResultToken,
		)
		return fmt.Errorf("%s sandbox execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}
	if execResult != nil && !execResult.Succeeded() {
		forwardWorkspaceStdoutResultMarkerIfPresent(
			ctx, executor, ref, sandboxEnv.CommandTimeout, execResult, stdoutResultToken,
		)
		return fmt.Errorf(
			"%s sandbox execution failed: command exited with code %d%s",
			name,
			execResult.ExitCode,
			formatSandboxExecOutput(execResult),
		)
	}
	if err := forwardWorkspaceStdoutResultMarker(
		ctx, executor, ref, sandboxEnv.CommandTimeout, execResult, stdoutResultToken,
	); err != nil {
		return fmt.Errorf("%s sandbox execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}

	fmt.Printf("Task %s/%s completed in sandbox workspace %s\n", taskNamespace, taskName, ref.ClaimName)
	return nil
}

func forwardWorkspaceStdoutResultMarker(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	result *workspace.ExecResult,
	expectedToken string,
) error {
	if !workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return nil
	}
	marker, err := workspaceStdoutResultMarker(ctx, executor, ref, timeout, result, expectedToken)
	if err != nil {
		return err
	}
	if marker == "" {
		return fmt.Errorf(
			"%s is true but inner worker did not write %s",
			workerenv.ResultStdout,
			workerenv.ResultStdoutPrefix,
		)
	}
	fmt.Println(marker)
	return nil
}

func forwardWorkspaceStdoutResultMarkerIfPresent(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	result *workspace.ExecResult,
	expectedToken string,
) {
	if !workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return
	}
	marker, err := workspaceStdoutResultMarker(ctx, executor, ref, timeout, result, expectedToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to forward stdout result marker: %v\n", err)
		return
	}
	if marker != "" {
		fmt.Println(marker)
	}
}

func workspaceStdoutResultMarker(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	result *workspace.ExecResult,
	expectedToken string,
) (string, error) {
	if !workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return "", nil
	}

	if result != nil {
		if result.StdoutTruncated {
			marker, downloadErr := downloadStdoutResultMarker(ctx, executor, ref, timeout, expectedToken)
			if marker != "" {
				return marker, nil
			}
			if downloadErr != nil {
				return "", fmt.Errorf("download stdout result marker after truncated stdout: %w", downloadErr)
			}
			return "", fmt.Errorf(
				"inner worker stdout was truncated before %s could be forwarded and marker file was not available",
				workerenv.ResultStdoutPrefix,
			)
		}
		if marker, ok := stdoutResultMarker(result.Stdout); ok {
			return marker, nil
		}
	}
	marker, downloadErr := downloadStdoutResultMarker(ctx, executor, ref, timeout, expectedToken)
	if marker != "" {
		return marker, nil
	}
	if downloadErr != nil && !workspace.IsKind(downloadErr, workspace.ErrorKindNotFound) {
		return "", fmt.Errorf("download stdout result marker: %w", downloadErr)
	}
	return "", nil
}

func downloadStdoutResultMarker(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	expectedToken string,
) (string, error) {
	if executor == nil || ref.IsZero() {
		return "", workspace.NewError(
			"download",
			workspace.ErrorKindNotFound,
			"workspace reference is unavailable",
			false,
			nil,
		)
	}
	result, err := executor.Download(ctx, workspace.DownloadRequest{
		Ref:     ref,
		Paths:   []string{agentSandboxResultMarkerUploadPath},
		Timeout: timeout,
	})
	if err != nil {
		return "", err
	}
	for _, artifact := range result.Artifacts {
		if artifact.Path != agentSandboxResultMarkerUploadPath {
			continue
		}
		data := string(artifact.Data)
		if err := validateStdoutResultToken(data, expectedToken); err != nil {
			return "", err
		}
		if marker, ok := stdoutResultMarker(data); ok {
			return marker, nil
		}
		return "", fmt.Errorf("downloaded stdout result marker did not contain %s", workerenv.ResultStdoutPrefix)
	}
	return "", workspace.NewError(
		"download",
		workspace.ErrorKindNotFound,
		"stdout result marker artifact not found",
		false,
		nil,
	)
}

func stdoutResultMarker(stdout string) (string, bool) {
	var marker string
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if _, ok := strings.CutPrefix(line, workerenv.ResultStdoutPrefix); ok {
			marker = line
		}
	}
	return marker, marker != ""
}

func validateStdoutResultToken(data, expectedToken string) error {
	expectedToken = strings.TrimSpace(expectedToken)
	if expectedToken == "" {
		return nil
	}
	for line := range strings.SplitSeq(data, "\n") {
		if token, ok := strings.CutPrefix(strings.TrimSpace(line), agentSandboxResultTokenPrefix); ok {
			if token == expectedToken {
				return nil
			}
			return fmt.Errorf("downloaded stdout result marker token did not match current execution")
		}
	}
	return fmt.Errorf("downloaded stdout result marker is missing current execution token")
}

func sandboxExecMaxOutputBytes() int64 {
	if workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return agentSandboxStdoutResultMaxOutputBytes
	}
	return agentSandboxExecMaxOutputBytes
}

func executionWorkspaceExecutor(workspaceEnv workerenv.ExecutionWorkspaceEnv) (workspace.WorkspaceExecutor, error) {
	switch strings.TrimSpace(workspaceEnv.Provider) {
	case "", string(corev1alpha1.WorkspaceProviderAgentSandbox):
		return getAgentSandboxWorkspaceExecutor(), nil
	case string(corev1alpha1.WorkspaceProviderSubstrate):
		return getSubstrateWorkspaceExecutor()
	default:
		return nil, fmt.Errorf("unsupported execution workspace provider %q", workspaceEnv.Provider)
	}
}

func workspaceTemplateNamespace(workspaceEnv workerenv.ExecutionWorkspaceEnv, taskNamespace string) string {
	if ns := strings.TrimSpace(workspaceEnv.TemplateNamespace); ns != "" {
		return ns
	}
	return taskNamespace
}

func workspaceClaimNamespace(
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	taskNamespace string,
	templateNamespace string,
) string {
	if ns := strings.TrimSpace(workspaceEnv.ClaimNamespace); ns != "" {
		return ns
	}
	if strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return templateNamespace
	}
	legacy := workerenv.ParseAgentSandboxEnv(os.Getenv)
	return agentSandboxClaimNamespace(legacy, taskNamespace, templateNamespace)
}

func workspaceClaimName(
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	claimNamespace string,
	taskNamespace string,
	templateNamespace string,
) string {
	if claimName := strings.TrimSpace(workspaceEnv.ClaimName); claimName != "" {
		return claimName
	}
	if strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return ""
	}
	legacy := workerenv.ParseAgentSandboxEnv(os.Getenv)
	legacy.ReusePolicy = workspaceEnv.ReusePolicy
	legacy.ReuseKey = workspaceEnv.ReuseKey
	legacy.TemplateName = workspaceEnv.TemplateName
	return agentSandboxSessionClaimName(legacy, claimNamespace, taskNamespace, templateNamespace)
}

func workspaceWarmPoolPolicy(workspaceEnv workerenv.ExecutionWorkspaceEnv) string {
	if strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return ""
	}
	return agentSandboxClaimWarmPoolPolicy(workerenv.ParseAgentSandboxEnv(os.Getenv).WarmPoolPolicy)
}

func executionWorkspaceResidentProcess(workspaceEnv workerenv.ExecutionWorkspaceEnv) bool {
	return strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) &&
		strings.TrimSpace(workspaceEnv.ProcessMode) == string(corev1alpha1.ExecutionWorkspaceProcessModeResident)
}

func executionWorkspaceResidentKey(workspaceEnv workerenv.ExecutionWorkspaceEnv, ref workspace.WorkspaceRef) string {
	if key := strings.TrimSpace(workspaceEnv.ResidentKey); key != "" {
		return key
	}
	if key := strings.TrimSpace(workspaceEnv.ReuseKey); key != "" {
		return key
	}
	if key := strings.TrimSpace(ref.ID); key != "" {
		return key
	}
	return strings.TrimSpace(ref.ClaimName)
}

func stageAgentSandboxExecutable(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	executable string,
	args []string,
	innerEnv map[string]string,
	timeout time.Duration,
) ([]string, map[string]string, error) {
	data, err := os.ReadFile(executable)
	if err != nil {
		return nil, nil, fmt.Errorf("read agent executable for sandbox: %w", err)
	}

	tokenUploaded := false
	tokenCleanupPaths := make([]string, 0, 3)
	artifacts := []workspace.UploadArtifact{
		{
			Path: agentSandboxWorkerUploadPath,
			Data: data,
			Mode: 0o700,
		},
	}
	if workerenv.IsTrue(innerEnv[workerenv.ResultStdout]) {
		resultToken := strings.TrimSpace(innerEnv[workerenv.ResultStdoutToken])
		if resultToken == "" {
			resultToken, err = generateWorkspaceStdoutResultToken()
			if err != nil {
				return nil, nil, err
			}
			innerEnv[workerenv.ResultStdoutToken] = resultToken
		}
		artifacts = append(artifacts, workspace.UploadArtifact{
			Path: agentSandboxResultMarkerUploadPath,
			Data: []byte(agentSandboxResultTokenPrefix + resultToken + "\n"),
			Mode: 0o600,
		})
	}
	if token := workerServiceAccountToken(); token != "" && !workerenv.IsTrue(innerEnv[workerenv.AgentReadOnly]) {
		tokenUploaded = true
		artifacts = append(artifacts, workspace.UploadArtifact{
			Path: agentSandboxSATokenUploadPath,
			Data: []byte(token),
			Mode: 0o600,
		})
		innerEnv[workerenv.ServiceAccountTokenPath] = agentSandboxSATokenExecPath
		tokenCleanupPaths = append(tokenCleanupPaths, agentSandboxSATokenExecPath)
	}
	transactionArtifacts, transactionCleanupPaths, err := agentSandboxTransactionTokenArtifacts(innerEnv)
	if err != nil {
		return nil, nil, err
	}
	artifacts = append(artifacts, transactionArtifacts...)
	tokenCleanupPaths = append(tokenCleanupPaths, transactionCleanupPaths...)
	gitAskpassUploaded := false
	if strings.TrimSpace(innerEnv[workerenv.GitToken]) != "" {
		gitAskpassUploaded = true
		artifacts = append(artifacts, workspace.UploadArtifact{
			Path: agentSandboxGitAskpassUploadPath,
			Data: []byte("#!/bin/sh\nprintf '%s\\n' \"$GIT_TOKEN\"\n"),
			Mode: 0o700,
		})
		innerEnv[workerenv.GitAskpass] = agentSandboxGitAskpassExecPath
	}

	if _, err := executor.Upload(ctx, workspace.UploadRequest{
		Ref:       ref,
		Artifacts: artifacts,
		Timeout:   timeout,
	}); err != nil {
		return nil, nil, fmt.Errorf("stage agent executable in sandbox: %w", err)
	}

	command := []string{
		"sh",
		"-c",
		agentSandboxWorkerCommand(tokenUploaded, gitAskpassUploaded, tokenCleanupPaths...),
		agentSandboxWorkerUploadPath,
	}
	command = append(command, args...)
	return command, innerEnv, nil
}

func generateWorkspaceStdoutResultToken() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate stdout result token: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}

func agentSandboxWorkerCommand(tokenUploaded, gitAskpassUploaded bool, tokenCleanupPaths ...string) string {
	chmodTargets := []string{agentSandboxWorkerExecPath}
	if gitAskpassUploaded {
		chmodTargets = append(chmodTargets, agentSandboxGitAskpassExecPath)
	}
	setup := "chmod 0700 " + strings.Join(chmodTargets, " ")
	cleanupPaths := append([]string(nil), tokenCleanupPaths...)
	if tokenUploaded {
		cleanupPaths = appendUniqueString(cleanupPaths, agentSandboxSATokenExecPath)
	}
	if len(cleanupPaths) > 0 {
		setup += " && chmod 0600 " + strings.Join(cleanupPaths, " ")
	}
	if len(cleanupPaths) == 0 {
		return setup + " && exec " + agentSandboxWorkerExecPath + " \"$@\""
	}
	return strings.Join([]string{
		setup,
		"&&",
		agentSandboxWorkerExecPath + " \"$@\";",
		"status=$?;",
		"rm -f " + strings.Join(cleanupPaths, " ") + ";",
		"exit $status",
	}, " ")
}

func agentSandboxTransactionTokenArtifacts(innerEnv map[string]string) ([]workspace.UploadArtifact, []string, error) {
	tokenFiles := []struct {
		envName     string
		description string
		uploadPath  string
		execPath    string
	}{
		{
			envName:     workerenv.TransactionTokenFile,
			description: "transaction token",
			uploadPath:  agentSandboxTransactionTokenUploadPath,
			execPath:    agentSandboxTransactionTokenExecPath,
		},
		{
			envName:     workerenv.ContextTokenSubjectTokenFile,
			description: "context token subject token",
			uploadPath:  agentSandboxContextSubjectTokenUploadPath,
			execPath:    agentSandboxContextSubjectTokenExecPath,
		},
	}

	artifacts := make([]workspace.UploadArtifact, 0, len(tokenFiles))
	cleanupPaths := make([]string, 0, len(tokenFiles))
	stagedBySourcePath := make(map[string]string, len(tokenFiles))
	for _, tokenFile := range tokenFiles {
		outerPath := strings.TrimSpace(innerEnv[tokenFile.envName])
		if outerPath == "" {
			continue
		}
		if stagedPath := stagedBySourcePath[outerPath]; stagedPath != "" {
			innerEnv[tokenFile.envName] = stagedPath
			continue
		}
		token, err := workerenv.ReadTokenFile(outerPath, tokenFile.description)
		if err != nil {
			return nil, nil, err
		}
		artifacts = append(artifacts, workspace.UploadArtifact{
			Path: tokenFile.uploadPath,
			Data: []byte(token),
			Mode: 0o600,
		})
		innerEnv[tokenFile.envName] = tokenFile.execPath
		stagedBySourcePath[outerPath] = tokenFile.execPath
		cleanupPaths = append(cleanupPaths, tokenFile.execPath)
	}
	return artifacts, cleanupPaths, nil
}

func appendUniqueString(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func agentSandboxClaimWarmPoolPolicy(policy string) string {
	switch strings.TrimSpace(strings.ToLower(policy)) {
	case "", "disabled":
		return "none"
	case "template":
		return "default"
	default:
		return "none"
	}
}

func agentSandboxTemplateNamespace(sandboxEnv workerenv.AgentSandboxEnv, taskNamespace string) string {
	if ns := strings.TrimSpace(sandboxEnv.TemplateNamespace); ns != "" {
		return ns
	}
	return taskNamespace
}

func agentSandboxClaimNamespace(sandboxEnv workerenv.AgentSandboxEnv, taskNamespace, templateNamespace string) string {
	if ns := strings.TrimSpace(sandboxEnv.ClaimNamespace); ns != "" {
		return ns
	}
	if strings.EqualFold(strings.TrimSpace(sandboxEnv.NamespaceStrategy), "controller") &&
		strings.TrimSpace(templateNamespace) != "" {
		return strings.TrimSpace(templateNamespace)
	}
	return taskNamespace
}

func agentSandboxSessionClaimName(
	sandboxEnv workerenv.AgentSandboxEnv,
	claimNamespace string,
	taskNamespace string,
	templateNamespace string,
) string {
	if !strings.EqualFold(strings.TrimSpace(sandboxEnv.ReusePolicy), "session") ||
		strings.TrimSpace(sandboxEnv.ReuseKey) == "" {
		return ""
	}
	parts := []string{
		strings.TrimSpace(claimNamespace),
		strings.TrimSpace(taskNamespace),
		strings.TrimSpace(templateNamespace),
		strings.TrimSpace(sandboxEnv.TemplateName),
		strings.TrimSpace(sandboxEnv.ReuseKey),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "orka-session-" + hex.EncodeToString(sum[:])[:32]
}

func cleanupAgentSandboxWorkspace(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	sandboxEnv workerenv.AgentSandboxEnv,
) {
	if ref.IsZero() || executor == nil {
		return
	}

	switch strings.TrimSpace(strings.ToLower(sandboxEnv.CleanupPolicy)) {
	case "retain":
		retainAgentSandboxWorkspace(ctx, executor, ref, sandboxEnv, "agent sandbox cleanup policy retain")
	case "", "delete":
		deleteAgentSandboxWorkspace(ctx, executor, ref, sandboxEnv)
	default:
		fmt.Fprintf(
			os.Stderr,
			"warning: unsupported sandbox cleanup policy %q; retaining workspace to avoid unintended deletion\n",
			sandboxEnv.CleanupPolicy,
		)
		retainAgentSandboxWorkspace(ctx, executor, ref, sandboxEnv, "unsupported agent sandbox cleanup policy")
	}
}

func agentSandboxCleanupContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), timeout)
}

func retainAgentSandboxWorkspace(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	sandboxEnv workerenv.AgentSandboxEnv,
	reason string,
) {
	scrubAgentSandboxServiceAccountToken(ctx, executor, ref, sandboxEnv)
	if _, err := executor.Release(ctx, workspace.ReleaseRequest{
		Ref:     ref,
		Retain:  true,
		Reason:  reason,
		Timeout: sandboxEnv.ClaimTimeout,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to retain sandbox workspace: %v\n", err)
	}
}

func scrubAgentSandboxServiceAccountToken(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	sandboxEnv workerenv.AgentSandboxEnv,
) {
	if _, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref: ref,
		Command: []string{
			"rm",
			"-f",
			agentSandboxSATokenExecPath,
			agentSandboxTransactionTokenExecPath,
			agentSandboxContextSubjectTokenExecPath,
		},
		Timeout:        sandboxEnv.ClaimTimeout,
		MaxOutputBytes: agentSandboxExecMaxOutputBytes,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to scrub sandbox token files before retaining workspace: %v\n", err)
	}
}

func deleteAgentSandboxWorkspace(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	sandboxEnv workerenv.AgentSandboxEnv,
) {
	if _, err := executor.Delete(ctx, workspace.DeleteRequest{
		Ref:     ref,
		Reason:  "agent sandbox cleanup policy delete",
		Timeout: sandboxEnv.ClaimTimeout,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to delete sandbox workspace: %v\n", err)
	}
}

func formatSandboxExecOutput(result *workspace.ExecResult) string {
	if result == nil {
		return ""
	}

	parts := []string{
		"stdout=" + formatSandboxExecStream(result.Stdout, agentSandboxExecMaxOutputBytes),
		fmt.Sprintf("stdout_truncated=%t", result.StdoutTruncated),
		"stderr=" + formatSandboxExecStream(result.Stderr, agentSandboxExecMaxOutputBytes),
		fmt.Sprintf("stderr_truncated=%t", result.StderrTruncated),
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func formatSandboxExecStream(value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "<empty>"
	}
	return truncateForError(value, max)
}

func truncateForError(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "...<truncated>"
}

func agentSandboxInnerEnv(environ []string) map[string]string {
	env := environToMap(environ)
	depth := agentSandboxDepth(env[workerenv.AgentSandboxDepth])
	workspaceDepth := agentSandboxDepth(env[workerenv.ExecutionWorkspaceDepth])
	scrubInnerExecutionWorkspaceEnv(env)
	env[workerenv.ExecutionWorkspaceEnabled] = workerEnvFalse
	env[workerenv.ExecutionWorkspaceDepth] = strconv.Itoa(workspaceDepth + 1)
	env[workerenv.AgentSandboxEnabled] = workerEnvFalse
	env[workerenv.AgentSandboxDepth] = strconv.Itoa(depth + 1)
	delete(env, workerenv.ServiceAccountToken)
	delete(env, workerenv.ServiceAccountTokenPath)
	delete(env, workspaceHandoffTokenEnv)
	delete(env, workspaceBootstrapTokenEnv)
	return env
}

func agentSandboxDepth(value string) int {
	depth, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || depth < 0 {
		return 0
	}
	return depth
}

func environToMap(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		env[name] = value
	}
	return env
}
