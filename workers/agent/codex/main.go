/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sozercan/orka/workers/common"
)

const (
	defaultMaxTurns  = 50
	workspaceDir     = "/workspace"
	defaultCodexPath = "codex"
)

var errCodexRequiresBash = errors.New(
	"codex runtime requires allowBash=true because the Codex CLI cannot disable shell execution",
)

func main() {
	if err := common.RunAgent("codex", workspaceDir, defaultMaxTurns, executeCodex); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// executeCodex invokes the Codex CLI and returns its final response.
func executeCodex(ctx context.Context, cfg *common.AgentConfig) (string, error) {
	if !allowBashEnabled() {
		return "", errCodexRequiresBash
	}

	outputFile, err := os.CreateTemp("", "codex-last-message-*")
	if err != nil {
		return "", fmt.Errorf("create output temp file: %w", err)
	}
	outputPath := outputFile.Name()
	if err := outputFile.Close(); err != nil {
		return "", fmt.Errorf("close output temp file: %w", err)
	}
	defer os.Remove(outputPath)

	instructionsPath, cleanupInstructions, err := writeCodexInstructionsFile(cfg)
	if err != nil {
		return "", err
	}
	defer cleanupInstructions()

	args := buildCodexArgs(cfg, outputPath, instructionsPath)
	fmt.Printf("Executing: %s %s\n", codexPath(), strings.Join(args, " "))

	execCtx := ctx
	if cfg.TimeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		execCtx, timeoutCancel = context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer timeoutCancel()
	}

	cmd := exec.CommandContext(execCtx, codexPath(), args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	cmd.Stdin = strings.NewReader(cfg.Prompt)

	dir := workspaceDir
	if cfg.SubPath != "" {
		dir = filepath.Join(workspaceDir, cfg.SubPath)
	}
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat workspace directory: %w", err)
		}
		fallbackDir, fallbackErr := os.Getwd()
		if fallbackErr != nil {
			return "", fmt.Errorf("determine fallback workspace directory: %w", fallbackErr)
		}
		dir = fallbackDir
	}
	cmd.Dir = dir
	cmd.Env = buildCodexEnv()

	if err := cmd.Run(); err != nil {
		result := readCodexResult(outputPath, stdout.String())
		if stderrStr := stderr.String(); stderrStr != "" {
			return result, fmt.Errorf("%w: %s", err, stderrStr)
		}
		return result, err
	}

	return readCodexResult(outputPath, stdout.String()), nil
}

func buildCodexArgs(cfg *common.AgentConfig, outputPath, instructionsPath string) []string {
	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--color", "never",
		"--output-last-message", outputPath,
		"--config", "approval_policy=never",
		"--sandbox", "workspace-write",
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if instructionsPath != "" {
		args = append(args, "--config", "model_instructions_file="+instructionsPath)
	}
	if baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); baseURL != "" {
		args = append(args, "--config", "openai_base_url="+baseURL)
	}
	args = append(args, "--config", "sandbox_workspace_write.network_access=true")
	if webSearchSetting, ok := codexWebSearchSetting(cfg); ok {
		args = append(args, "--config", "web_search="+webSearchSetting)
	}

	args = append(args, "-")
	return args
}

func buildCodexInstructions(cfg *common.AgentConfig) string {
	var sections []string

	if systemPrompt := strings.TrimSpace(cfg.SystemPrompt); systemPrompt != "" {
		sections = append(sections, systemPrompt)
	}

	var guidance []string
	if cfg.MaxTurns > 0 {
		guidance = append(guidance, fmt.Sprintf("Try to complete this task within %d turns.", cfg.MaxTurns))
	}
	if !allowBashEnabled() {
		guidance = append(guidance, "Do not use shell commands unless absolutely necessary. Prefer built-in file inspection and editing tools.")
	}

	allowedTools := trimmedTools(cfg.AllowedTools)
	if len(allowedTools) > 0 {
		guidance = append(guidance, fmt.Sprintf("Respect this requested tool allowlist when possible: %s.", strings.Join(allowedTools, ", ")))
	}

	disallowedTools := trimmedTools(cfg.DisallowedTools)
	if len(disallowedTools) > 0 {
		guidance = append(guidance, fmt.Sprintf("Do not use these tools: %s.", strings.Join(disallowedTools, ", ")))
	}

	if webSearchSetting, ok := codexWebSearchSetting(cfg); ok {
		if webSearchSetting == "disabled" {
			guidance = append(guidance, "Web search is disabled for this task.")
		} else {
			guidance = append(guidance, "Web search is available for this task when needed.")
		}
	}

	if len(guidance) > 0 {
		sections = append(sections, "Additional runtime guidance:\n- "+strings.Join(guidance, "\n- "))
	}

	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func writeCodexInstructionsFile(cfg *common.AgentConfig) (string, func(), error) {
	instructions := buildCodexInstructions(cfg)
	if instructions == "" {
		return "", func() {}, nil
	}

	f, err := os.CreateTemp("", "codex-instructions-*.md")
	if err != nil {
		return "", func() {}, fmt.Errorf("create instructions temp file: %w", err)
	}
	if _, err := f.WriteString(instructions); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("write instructions temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("close instructions temp file: %w", err)
	}

	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

func buildCodexEnv() []string {
	env := os.Environ()
	env = setEnvVar(env, "HOME", "/home/worker")

	if os.Getenv("CODEX_API_KEY") == "" {
		if apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); apiKey != "" {
			env = setEnvVar(env, "CODEX_API_KEY", apiKey)
		}
	}

	return env
}

func readCodexResult(outputPath, fallback string) string {
	data, err := os.ReadFile(outputPath)
	if err == nil && len(data) > 0 {
		return string(data)
	}
	return fallback
}

func codexPath() string {
	if p := os.Getenv("CODEX_CLI_PATH"); p != "" {
		return p
	}
	return defaultCodexPath
}

func allowBashEnabled() bool {
	return os.Getenv("ORKA_ALLOW_BASH") == "true"
}

func codexWebSearchSetting(cfg *common.AgentConfig) (string, bool) {
	if hasWebSearchTool(cfg.DisallowedTools) {
		return "disabled", true
	}
	if len(cfg.AllowedTools) > 0 {
		if hasWebSearchTool(cfg.AllowedTools) {
			return "live", true
		}
		return "disabled", true
	}
	return "", false
}

func hasWebSearchTool(tools []string) bool {
	for _, tool := range tools {
		if normalizeToolName(tool) == "websearch" {
			return true
		}
	}
	return false
}

func normalizeToolName(tool string) string {
	replacer := strings.NewReplacer("_", "", "-", "", " ", "")
	return replacer.Replace(strings.ToLower(strings.TrimSpace(tool)))
}

func trimmedTools(tools []string) []string {
	trimmed := make([]string, 0, len(tools))
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			continue
		}
		trimmed = append(trimmed, tool)
	}
	return trimmed
}

func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
