/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sozercan/orka/internal/workerenv"
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

// RunAgent orchestrates the common agent worker lifecycle: signal handling,
// config loading, git setup, workspace preparation, agent execution, and
// result submission.
func RunAgent(name, workspaceDir string, defaultMaxTurns int, executor AgentExecutor) error {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGTERM, syscall.SIGINT,
	)
	defer cancel()

	cfg, err := LoadConfig(defaultMaxTurns)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	fmt.Printf("Worker %s started task=%s/%s%s\n",
		name, cfg.TaskNamespace, cfg.TaskName, workerenv.TransactionLogFields(cfg.TransactionID, cfg.TransactionProfile))

	// Configure git credentials globally (for both clone and agent push operations)
	SetupGitCredentials()

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
