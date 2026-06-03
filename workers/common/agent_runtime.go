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
	"errors"
	"fmt"
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
	SubPath            string
	TimeoutSeconds     int
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

	return nil
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
		return fmt.Errorf("existing git remote origin does not match configured repo")
	}
	return nil
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
		if err := execGitContext(ctx, workspaceDir, "checkout", "FETCH_HEAD"); err == nil {
			return nil
		} else {
			return fmt.Errorf("git checkout fetched ref %q failed: %w", ref, err)
		}
	}

	if isHexGitObjectID(ref) && remoteBranchesContainRef(ctx, workspaceDir, ref) {
		if err := execGitContext(ctx, workspaceDir, "checkout", ref); err == nil {
			return nil
		} else {
			return fmt.Errorf("git checkout fetched commit ref %q failed: %w", ref, err)
		}
	}
	return fmt.Errorf("git checkout ref %q failed", ref)
}

func gitBranchNameFromRef(ctx context.Context, workspaceDir, ref string) (string, bool) {
	branch := strings.TrimPrefix(ref, "refs/heads/")
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
	if err := execGitContext(ctx, workspaceDir, "fetch", "origin", branch); err != nil {
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

	return append([]string{"-c", "safe.directory=" + safeDir}, args...)
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
	workspaceHandoffTokenUploadPath           = "orka-workspace-handoff-token"
	workspaceHandoffTokenDefaultPath          = "/app/" + workspaceHandoffTokenUploadPath
	agentSandboxExecMaxOutputBytes            = 2000
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
		APIEndpoint:           substrateEnv.APIEndpoint,
		APICAFile:             substrateEnv.APICAFile,
		APIInsecureSkipVerify: substrateEnv.APIInsecureSkipVerify,
		RouterURL:             substrateEnv.RouterURL,
		ActorDNSSuffix:        substrateEnv.ActorDNSSuffix,
	})
	return substrateWorkspaceExecutor, substrateWorkspaceExecutorErr
}

// RunAgent orchestrates the common agent worker lifecycle: signal handling,
// config loading, git setup, workspace preparation, agent execution, and
// result submission.
func RunAgent(name, workspaceDir string, defaultMaxTurns int, executor AgentExecutor) error {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGTERM, syscall.SIGINT,
	)
	defer cancel()

	// Populate git credential env vars before the sandbox handoff so the inner
	// worker can clone private repositories without mounting the outer secret.
	setupGitCredentialsForRunAgent()

	if workspaceEnv := workerenv.ParseExecutionWorkspaceEnv(os.Getenv); workspaceEnv.Enabled {
		if workspaceEnv.Depth > 0 {
			return fmt.Errorf(
				"execution workspace recursion detected: %s=%d",
				workerenv.ExecutionWorkspaceDepth,
				workspaceEnv.Depth,
			)
		}
		return runAgentInWorkspace(ctx, name, workspaceDir, workspaceEnv)
	}

	if sandboxEnv := workerenv.ParseAgentSandboxEnv(os.Getenv); sandboxEnv.Enabled {
		if depth := agentSandboxDepth(os.Getenv(workerenv.AgentSandboxDepth)); depth > 0 {
			return fmt.Errorf("agent sandbox recursion detected: %s=%d", workerenv.AgentSandboxDepth, depth)
		}
		return runAgentInSandbox(ctx, name, workspaceDir, sandboxEnv)
	}

	cfg, err := LoadConfig(defaultMaxTurns)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	fmt.Printf("Worker %s started task=%s/%s%s\n",
		name, cfg.TaskNamespace, cfg.TaskName, workerenv.TransactionLogFields(cfg.TransactionID, cfg.TransactionProfile))

	// Clone git repo if configured
	if cfg.GitRepo != "" {
		if err := CloneRepo(ctx, cfg, workspaceDir); err != nil {
			return fmt.Errorf("git clone failed: %w", err)
		}
	}

	// Apply prior task diff if iterating
	if err := PrepareWorkspace(workspaceDir); err != nil {
		return fmt.Errorf("workspace preparation failed: %w", err)
	}
	if err := EnsureWorkspaceArtifactsLink(workspaceDir); err != nil {
		return fmt.Errorf("artifact workspace setup failed: %w", err)
	}

	result, err := executor(ctx, cfg)
	if err != nil {
		// On failure, still try to submit partial result with any diffs
		errorOutput := fmt.Sprintf("Error: %v\n\n%s", err, result)
		resultDir := ""
		if cfg.GitRepo != "" {
			resultDir = workspaceDir
		}
		resultBytes, finalizeErr := FinalizeResult(resultDir, errorOutput)
		if finalizeErr != nil {
			fmt.Fprintf(os.Stderr, "failed to finalize error result: %v\n", finalizeErr)
			resultBytes = []byte(errorOutput)
		}
		if submitErr := SubmitResult(resultBytes); submitErr != nil {
			fmt.Fprintf(os.Stderr, "failed to submit error result: %v\n", submitErr)
		}
		if artifactErr := UploadArtifacts(); artifactErr != nil {
			fmt.Fprintf(os.Stderr, "warning: artifact upload failed: %v\n", artifactErr)
		}
		return fmt.Errorf("%s execution failed: %w", name, err)
	}

	// Build structured result with diff if workspace has changes
	if result == "" {
		fmt.Fprintf(os.Stderr, "warning: %s executor returned empty result\n", name)
		result = fmt.Sprintf("%s completed without a final message", name)
	}
	resultDir := ""
	if cfg.GitRepo != "" {
		resultDir = workspaceDir
	}
	resultBytes, err := FinalizeResult(resultDir, result)
	if err != nil {
		return fmt.Errorf("failed to finalize result: %w", err)
	}
	if err := SubmitResult(resultBytes); err != nil {
		return fmt.Errorf("failed to submit result: %w", err)
	}
	if err := UploadArtifacts(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: artifact upload failed: %v\n", err)
	}

	fmt.Printf("Task %s/%s completed successfully%s\n",
		cfg.TaskNamespace, cfg.TaskName, workerenv.TransactionLogFields(cfg.TransactionID, cfg.TransactionProfile))
	return nil
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
			false,
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

	if _, err := executor.WaitReady(ctx, workspace.WaitReadyRequest{
		Ref:     ref,
		Timeout: workspaceEnv.ClaimTimeout,
	}); err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonReadinessFailed,
			claim.Reused,
			"workspace readiness failed",
		)
		return fmt.Errorf("wait for execution workspace: %w", err)
	}
	submitExecutionWorkspaceStatus(
		workspaceEnv,
		corev1alpha1.ExecutionWorkspacePhaseReady,
		corev1alpha1.ExecutionWorkspaceReasonReady,
		claim.Reused,
		"workspace ready",
	)

	if handoffToken != "" {
		if err := bootstrapWorkspaceHandoffToken(ctx, executor, ref, handoffToken, workspaceEnv); err != nil {
			submitExecutionWorkspaceStatus(
				workspaceEnv,
				corev1alpha1.ExecutionWorkspacePhaseFailed,
				corev1alpha1.ExecutionWorkspaceReasonHandoffFailed,
				claim.Reused,
				"workspace handoff failed",
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
		)
		return err
	}

	execResult, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:            ref,
		Command:        command,
		Env:            innerEnv,
		WorkDir:        workspaceDir,
		Timeout:        workspaceEnv.CommandTimeout,
		MaxOutputBytes: agentSandboxExecMaxOutputBytes,
	})
	if err != nil {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
		)
		return fmt.Errorf("%s workspace execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}
	if execResult != nil && !execResult.Succeeded() {
		submitExecutionWorkspaceStatus(
			workspaceEnv,
			corev1alpha1.ExecutionWorkspacePhaseFailed,
			corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
			claim.Reused,
			"workspace command failed",
		)
		return fmt.Errorf(
			"%s workspace execution failed: command exited with code %d%s",
			name,
			execResult.ExitCode,
			formatSandboxExecOutput(execResult),
		)
	}

	cleanupCtx, cleanupCancel := agentSandboxCleanupContext(workspaceEnv.ClaimTimeout)
	defer cleanupCancel()
	if err := cleanupExecutionWorkspace(cleanupCtx, executor, ref, workspaceEnv, claim.Reused, true); err != nil {
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
		)
		return fmt.Errorf("execution workspace cleanup failed: %w", err)
	}
	cleaned = true

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

type executionWorkspaceCleanupOptions struct {
	skipSubstrateDeleteScrub  bool
	skipSubstrateReleaseScrub bool
}

func preTerminalExecutionWorkspaceCleanup(
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	substrateHandoffBootstrapped bool,
	claimedNewWorkspace bool,
) (workerenv.ExecutionWorkspaceEnv, executionWorkspaceCleanupOptions) {
	if substrateHandoffBootstrapped ||
		strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return workspaceEnv, executionWorkspaceCleanupOptions{}
	}
	options := executionWorkspaceCleanupOptions{
		skipSubstrateDeleteScrub:  true,
		skipSubstrateReleaseScrub: true,
	}
	if claimedNewWorkspace && strings.EqualFold(
		strings.TrimSpace(workspaceEnv.CleanupPolicy),
		string(corev1alpha1.WorkspaceCleanupPolicyRetain),
	) {
		workspaceEnv.CleanupPolicy = string(corev1alpha1.WorkspaceCleanupPolicyDelete)
	}
	return workspaceEnv, options
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

	execResult, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:            ref,
		Command:        command,
		Env:            innerEnv,
		WorkDir:        workspaceDir,
		Timeout:        sandboxEnv.CommandTimeout,
		MaxOutputBytes: agentSandboxExecMaxOutputBytes,
	})
	if err != nil {
		return fmt.Errorf("%s sandbox execution failed: %w%s", name, err, formatSandboxExecOutput(execResult))
	}
	if execResult != nil && !execResult.Succeeded() {
		return fmt.Errorf(
			"%s sandbox execution failed: command exited with code %d%s",
			name,
			execResult.ExitCode,
			formatSandboxExecOutput(execResult),
		)
	}

	fmt.Printf("Task %s/%s completed in sandbox workspace %s\n", taskNamespace, taskName, ref.ClaimName)
	return nil
}

func ensureWorkspaceHandoffToken(workspaceEnv workerenv.ExecutionWorkspaceEnv) (string, error) {
	if strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return "", nil
	}
	if token := strings.TrimSpace(os.Getenv(workspaceHandoffTokenEnv)); token != "" {
		return token, nil
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate workspace handoff token: %w", err)
	}
	token := hex.EncodeToString(random[:])
	if err := os.Setenv(workspaceHandoffTokenEnv, token); err != nil {
		return "", fmt.Errorf("store workspace handoff token in environment: %w", err)
	}
	return token, nil
}

func bootstrapWorkspaceHandoffToken(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	token string,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
) error {
	if strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return nil
	}
	if _, err := executor.Upload(ctx, workspace.UploadRequest{
		Ref:              ref,
		BootstrapHandoff: true,
		Artifacts: []workspace.UploadArtifact{{
			Path: workspaceHandoffTokenUploadTarget(),
			Data: []byte(token),
			Mode: 0o600,
		}},
		Timeout: workspaceEnv.ClaimTimeout,
	}); err != nil {
		return fmt.Errorf("stage workspace handoff token: %w", err)
	}
	return nil
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
	if token := workerServiceAccountToken(); token != "" {
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

func cleanupExecutionWorkspace(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	reused bool,
	submitStatus bool,
) error {
	return cleanupExecutionWorkspaceWithOptions(
		ctx,
		executor,
		ref,
		workspaceEnv,
		reused,
		submitStatus,
		executionWorkspaceCleanupOptions{},
	)
}

func cleanupExecutionWorkspaceWithOptions(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	reused bool,
	submitStatus bool,
	options executionWorkspaceCleanupOptions,
) error {
	if ref.IsZero() || executor == nil {
		return nil
	}

	switch strings.TrimSpace(strings.ToLower(workspaceEnv.CleanupPolicy)) {
	case "retain":
		if shouldPreScrubExecutionWorkspaceSecrets(workspaceEnv) {
			if err := scrubExecutionWorkspaceSecrets(ctx, executor, ref, workspaceEnv); err != nil {
				return fmt.Errorf("%w: %w", errExecutionWorkspaceSecretScrubFailed, err)
			}
		}
		if _, err := executor.Release(ctx, workspace.ReleaseRequest{
			Ref:       ref,
			Retain:    true,
			Reason:    "execution workspace cleanup policy retain",
			Timeout:   workspaceEnv.ClaimTimeout,
			SkipScrub: options.skipSubstrateReleaseScrub,
		}); err != nil {
			return fmt.Errorf("retain workspace: %w", err)
		}
		if submitStatus {
			submitExecutionWorkspaceStatus(
				workspaceEnv,
				corev1alpha1.ExecutionWorkspacePhaseRetained,
				corev1alpha1.ExecutionWorkspaceReasonRetained,
				reused,
				"workspace retained",
			)
		}
		return nil
	case "", "delete":
		if _, err := executor.Delete(ctx, workspace.DeleteRequest{
			Ref:       ref,
			Reason:    "execution workspace cleanup policy delete",
			Timeout:   workspaceEnv.ClaimTimeout,
			SkipScrub: options.skipSubstrateDeleteScrub,
		}); err != nil {
			return fmt.Errorf("delete workspace: %w", err)
		}
		if submitStatus {
			submitExecutionWorkspaceStatus(
				workspaceEnv,
				corev1alpha1.ExecutionWorkspacePhaseDeleted,
				corev1alpha1.ExecutionWorkspaceReasonDeleted,
				reused,
				"workspace deleted",
			)
		}
		return nil
	default:
		fmt.Fprintf(
			os.Stderr,
			"warning: unsupported workspace cleanup policy %q; retaining workspace to avoid unintended deletion\n",
			workspaceEnv.CleanupPolicy,
		)
		if shouldPreScrubExecutionWorkspaceSecrets(workspaceEnv) {
			if err := scrubExecutionWorkspaceSecrets(ctx, executor, ref, workspaceEnv); err != nil {
				return fmt.Errorf("%w: %w", errExecutionWorkspaceSecretScrubFailed, err)
			}
		}
		if _, err := executor.Release(ctx, workspace.ReleaseRequest{
			Ref:       ref,
			Retain:    true,
			Reason:    "unsupported execution workspace cleanup policy",
			Timeout:   workspaceEnv.ClaimTimeout,
			SkipScrub: options.skipSubstrateReleaseScrub,
		}); err != nil {
			return fmt.Errorf("retain workspace after unsupported cleanup policy: %w", err)
		}
		if submitStatus {
			submitExecutionWorkspaceStatus(
				workspaceEnv,
				corev1alpha1.ExecutionWorkspacePhaseRetained,
				corev1alpha1.ExecutionWorkspaceReasonRetained,
				reused,
				"workspace retained",
			)
		}
		return nil
	}
}

func shouldPreScrubExecutionWorkspaceSecrets(workspaceEnv workerenv.ExecutionWorkspaceEnv) bool {
	return strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate)
}

func scrubExecutionWorkspaceSecrets(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
) error {
	paths := executionWorkspaceScrubPaths()
	if len(paths) == 0 {
		return nil
	}
	_, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:            ref,
		Command:        append([]string{"rm", "-f"}, paths...),
		Timeout:        workspaceEnv.ClaimTimeout,
		MaxOutputBytes: agentSandboxExecMaxOutputBytes,
	})
	if err != nil {
		return fmt.Errorf("scrub execution workspace staged credentials: %w", err)
	}
	return nil
}

func executionWorkspaceScrubPaths() []string {
	paths := []string{
		agentSandboxWorkerExecPath,
		agentSandboxSATokenExecPath,
		agentSandboxTransactionTokenExecPath,
		agentSandboxContextSubjectTokenExecPath,
		agentSandboxGitAskpassExecPath,
		workspaceHandoffTokenDefaultPath,
	}
	if custom := strings.TrimSpace(os.Getenv(workspaceHandoffTokenFileEnv)); custom != "" {
		paths = appendUniqueString(paths, custom)
	}
	return paths
}

func workspaceHandoffTokenUploadTarget() string {
	if custom := strings.TrimSpace(os.Getenv(workspaceHandoffTokenFileEnv)); custom != "" {
		return custom
	}
	return workspaceHandoffTokenUploadPath
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

func workspaceInnerEnv(environ []string, workspaceEnv workerenv.ExecutionWorkspaceEnv) map[string]string {
	env := environToMap(environ)
	depth := workspaceEnv.Depth
	scrubInnerExecutionWorkspaceEnv(env)
	env[workerenv.ExecutionWorkspaceEnabled] = workerEnvFalse
	env[workerenv.ExecutionWorkspaceDepth] = strconv.Itoa(depth + 1)
	legacyDepth := agentSandboxDepth(env[workerenv.AgentSandboxDepth])
	env[workerenv.AgentSandboxEnabled] = workerEnvFalse
	env[workerenv.AgentSandboxDepth] = strconv.Itoa(legacyDepth + 1)
	delete(env, workerenv.ServiceAccountToken)
	delete(env, workerenv.ServiceAccountTokenPath)
	delete(env, workspaceHandoffTokenEnv)
	delete(env, workspaceBootstrapTokenEnv)
	return env
}

func scrubInnerExecutionWorkspaceEnv(env map[string]string) {
	for _, name := range []string{
		workerenv.ExecutionWorkspaceProvider,
		workerenv.ExecutionWorkspaceTemplateName,
		workerenv.ExecutionWorkspaceTemplateNamespace,
		workerenv.ExecutionWorkspaceClaimNamespace,
		workerenv.ExecutionWorkspaceClaimName,
		workerenv.ExecutionWorkspaceReusePolicy,
		workerenv.ExecutionWorkspaceReuseKey,
		workerenv.ExecutionWorkspaceCleanupPolicy,
		workerenv.ExecutionWorkspaceClaimTimeoutSeconds,
		workerenv.ExecutionWorkspaceCommandTimeoutSeconds,
		workerenv.ExecutionWorkspaceStatusEndpoint,
		workerenv.SubstrateAPIEndpoint,
		workerenv.SubstrateAPICAFile,
		workerenv.SubstrateAPIInsecureSkipVerify,
		workerenv.SubstrateRouterURL,
		workerenv.SubstrateActorDNSSuffix,
		workerenv.WorkspaceBootstrapToken,
	} {
		delete(env, name)
	}
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
