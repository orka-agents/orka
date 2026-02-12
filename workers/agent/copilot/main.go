/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/sozercan/mercan/workers/common"
)

const (
	defaultMaxTurns    = 50
	workspaceDir       = "/workspace"
	defaultCopilotPath = "copilot"
	defaultTimeout     = 20 * time.Minute
)

func main() {
	if err := common.RunAgent("copilot", workspaceDir, defaultMaxTurns, executeCopilot); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// buildSessionConfig constructs a Copilot SDK SessionConfig from the worker config.
func buildSessionConfig(cfg *common.AgentConfig) *copilot.SessionConfig {
	dir := workspaceDir
	if cfg.SubPath != "" {
		dir = workspaceDir + "/" + cfg.SubPath
	}

	sessionCfg := &copilot.SessionConfig{
		Model:            cfg.Model,
		WorkingDirectory: dir,
		// Auto-approve all permission requests for autonomous operation
		OnPermissionRequest: func(
			_ copilot.PermissionRequest, _ copilot.PermissionInvocation,
		) (copilot.PermissionRequestResult, error) {
			return copilot.PermissionRequestResult{Kind: "approved"}, nil
		},
	}

	if cfg.SystemPrompt != "" {
		sessionCfg.SystemMessage = &copilot.SystemMessageConfig{
			Mode:    "append",
			Content: cfg.SystemPrompt,
		}
	}

	if len(cfg.AllowedTools) > 0 {
		tools := make([]string, len(cfg.AllowedTools))
		for i, t := range cfg.AllowedTools {
			tools[i] = strings.TrimSpace(t)
		}
		sessionCfg.AvailableTools = tools
	}

	if len(cfg.DisallowedTools) > 0 {
		tools := make([]string, len(cfg.DisallowedTools))
		for i, t := range cfg.DisallowedTools {
			tools[i] = strings.TrimSpace(t)
		}
		sessionCfg.ExcludedTools = tools
	}

	return sessionCfg
}

// executeCopilot runs the Copilot SDK session and returns the result text.
func executeCopilot(ctx context.Context, cfg *common.AgentConfig) (string, error) {
	// Always apply a timeout so the Copilot SDK doesn't fall back to its
	// built-in 60-second default (SendAndWait adds one when the context
	// has no deadline).
	timeout := defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	execCtx, timeoutCancel := context.WithTimeout(ctx, timeout)
	defer timeoutCancel()

	// Create and start the Copilot client
	opts := &copilot.ClientOptions{
		CLIPath: copilotCLIPath(),
		Cwd:     workspaceDir,
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		opts.GithubToken = token
	}
	client := copilot.NewClient(opts)
	if err := client.Start(execCtx); err != nil {
		return "", fmt.Errorf("failed to start copilot client: %w", err)
	}
	defer func() {
		if err := client.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to stop copilot client: %v\n", err)
		}
	}()

	// Build session config and create session
	sessionCfg := buildSessionConfig(cfg)

	fmt.Printf("Creating Copilot session (model=%s, workspace=%s)\n",
		cfg.Model, sessionCfg.WorkingDirectory)

	session, err := client.CreateSession(execCtx, sessionCfg)
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}

	// Send the prompt and wait for completion
	fmt.Printf("Sending prompt (maxTurns=%d, timeout=%s)\n", cfg.MaxTurns, timeout)

	response, err := session.SendAndWait(execCtx, copilot.MessageOptions{
		Prompt: cfg.Prompt,
	})
	if err != nil {
		return "", fmt.Errorf("send and wait failed: %w", err)
	}

	// Extract result text from the response event
	return extractResult(response), nil
}

// extractResult extracts the text content from a session event response.
func extractResult(event *copilot.SessionEvent) string {
	if event == nil {
		return ""
	}

	// Try Result.Content first (structured result from agent completion)
	if event.Data.Result != nil && event.Data.Result.Content != "" {
		return event.Data.Result.Content
	}

	// Fall back to direct Content field (assistant message)
	if event.Data.Content != nil {
		return *event.Data.Content
	}

	return ""
}

// copilotCLIPath returns the path to the copilot CLI binary.
func copilotCLIPath() string {
	if p := os.Getenv("COPILOT_CLI_PATH"); p != "" {
		return p
	}
	return defaultCopilotPath
}
