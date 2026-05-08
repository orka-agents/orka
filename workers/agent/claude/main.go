/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/sozercan/orka/internal/workerenv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sozercan/orka/workers/common"
)

const (
	defaultMaxTurns   = 50
	workspaceDir      = "/workspace"
	defaultClaudePath = "claude"
)

func main() {
	if err := common.RunAgent("claude", workspaceDir, defaultMaxTurns, executeClaude); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// executeClaude invokes the Claude Code CLI and returns its output.
func executeClaude(ctx context.Context, cfg *common.AgentConfig) (string, error) {
	result, err := executeClaudePrompt(ctx, cfg, cfg.Prompt)
	if err != nil {
		return result, err
	}

	return common.EnsureRequiredSecurityArtifacts(
		ctx,
		cfg,
		result,
		func(followUpCtx context.Context, prompt string) (string, error) {
			followUpCfg := *cfg
			return executeClaudePrompt(followUpCtx, &followUpCfg, prompt)
		},
	)
}

func executeClaudePrompt(ctx context.Context, cfg *common.AgentConfig, prompt string) (string, error) {
	args := buildClaudeArgs(cfg, prompt)

	fmt.Printf("Executing: claude %s\n", strings.Join(args, " "))

	// Apply timeout if configured
	execCtx := ctx
	if cfg.TimeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		execCtx, timeoutCancel = context.WithTimeout(
			ctx, time.Duration(cfg.TimeoutSeconds)*time.Second,
		)
		defer timeoutCancel()
	}

	cmd := exec.CommandContext(execCtx, claudePath(), args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)

	// Set working directory
	dir := workspaceDir
	if cfg.SubPath != "" {
		dir = filepath.Join(workspaceDir, cfg.SubPath)
	}
	cmd.Dir = dir

	// Inherit environment (includes ANTHROPIC_API_KEY from secret mount)
	cmd.Env = os.Environ()

	// Ensure HOME is writable for CLI config/cache
	cmd.Env = append(cmd.Env, "HOME=/home/worker")

	if err := cmd.Run(); err != nil {
		// Include stderr in the error for debugging
		stderrStr := stderr.String()
		if stderrStr != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, stderrStr)
		}
		return stdout.String(), err
	}

	return stdout.String(), nil
}

// buildClaudeArgs constructs the CLI arguments for the claude command.
func buildClaudeArgs(cfg *common.AgentConfig, prompt string) []string {
	args := []string{
		"--print",   // non-interactive: print output and exit
		"--verbose", // verbose output for logging
	}

	// Model
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	// System prompt
	if cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.SystemPrompt)
	}

	// Max turns
	args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))

	// Tool permissions
	if os.Getenv(workerenv.AllowBash) == "true" {
		args = append(args, "--dangerously-skip-permissions")
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", strings.TrimSpace(tool))
	}
	for _, tool := range cfg.DisallowedTools {
		args = append(args, "--disallowedTools", strings.TrimSpace(tool))
	}

	// Prompt (use -p flag to avoid ambiguity with variadic tool flags)
	args = append(args, "-p", prompt)

	return args
}

// claudePath returns the path to the claude CLI binary.
func claudePath() string {
	if p := os.Getenv(workerenv.ClaudeCLIPath); p != "" {
		return p
	}
	return defaultClaudePath
}
