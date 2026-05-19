/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
func CloneRepo(ctx context.Context, cfg *AgentConfig, workspaceDir string) error {
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
		fetchErr := execGitContext(ctx, workspaceDir, "fetch", "origin", cfg.GitRef)
		if fetchErr != nil {
			fetchErr = execGitContext(ctx, workspaceDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
		}
		if fetchErr != nil {
			return fmt.Errorf("git fetch ref failed: %w", fetchErr)
		}

		if err := execGitContext(ctx, workspaceDir, "checkout", cfg.GitRef); err != nil {
			// Ref may not exist as a local branch; fall back to FETCH_HEAD from a
			// direct ref fetch, then to origin/<ref> for branch-name refs.
			if fbErr := execGitContext(ctx, workspaceDir, "checkout", "FETCH_HEAD"); fbErr != nil {
				if branchErr := execGitContext(ctx, workspaceDir, "checkout", "origin/"+cfg.GitRef); branchErr != nil {
					return fmt.Errorf("git checkout ref failed: %w", err)
				}
			}
		}
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
	agentSandboxWorkerUploadPath     = "orka-agent-worker"
	agentSandboxWorkerExecPath       = "/app/" + agentSandboxWorkerUploadPath
	agentSandboxSATokenUploadPath    = "orka-sa-token"
	agentSandboxSATokenExecPath      = "/app/" + agentSandboxSATokenUploadPath
	agentSandboxGitAskpassUploadPath = "orka-git-askpass"
	agentSandboxGitAskpassExecPath   = "/app/" + agentSandboxGitAskpassUploadPath
	agentSandboxExecMaxOutputBytes   = 2000
)

var (
	agentSandboxWorkspaceExecutorMu sync.RWMutex
	agentSandboxWorkspaceExecutor   workspace.WorkspaceExecutor = workspace.NewAgentSandboxExecutor()
	setupGitCredentialsForRunAgent                              = SetupGitCredentials
)

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
		ReuseKey: sandboxEnv.ReuseKey,
		Timeout:  sandboxEnv.ClaimTimeout,
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
	}
	gitAskpassUploaded := false
	if strings.TrimSpace(innerEnv[workerenv.GitToken]) != "" {
		gitAskpassUploaded = true
		artifacts = append(artifacts, workspace.UploadArtifact{
			Path: agentSandboxGitAskpassUploadPath,
			Data: []byte("#!/bin/sh\nprintf '%s\n' \"$GIT_TOKEN\"\n"),
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
		agentSandboxWorkerCommand(tokenUploaded, gitAskpassUploaded),
		agentSandboxWorkerUploadPath,
	}
	command = append(command, args...)
	return command, innerEnv, nil
}

func agentSandboxWorkerCommand(tokenUploaded, gitAskpassUploaded bool) string {
	chmodTargets := []string{agentSandboxWorkerExecPath}
	if gitAskpassUploaded {
		chmodTargets = append(chmodTargets, agentSandboxGitAskpassExecPath)
	}
	chmod := "chmod 0700 " + strings.Join(chmodTargets, " ")
	if !tokenUploaded {
		return chmod + " && exec " + agentSandboxWorkerExecPath + " \"$@\""
	}
	return strings.Join([]string{
		chmod,
		"&&",
		agentSandboxWorkerExecPath + " \"$@\";",
		"status=$?;",
		"rm -f " + agentSandboxSATokenExecPath + ";",
		"exit $status",
	}, " ")
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
		Ref:            ref,
		Command:        []string{"rm", "-f", agentSandboxSATokenExecPath},
		Timeout:        sandboxEnv.ClaimTimeout,
		MaxOutputBytes: agentSandboxExecMaxOutputBytes,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to scrub sandbox service account token before retaining workspace: %v\n", err)
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
	env[workerenv.AgentSandboxEnabled] = "false"
	env[workerenv.AgentSandboxDepth] = strconv.Itoa(depth + 1)
	delete(env, workerenv.ServiceAccountToken)
	delete(env, workerenv.ServiceAccountTokenPath)
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
