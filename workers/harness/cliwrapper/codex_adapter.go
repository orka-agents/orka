package cliwrapper

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	defaultCodexPath              = "codex"
	defaultCodexMaxTurns          = 50
	defaultCodexSandboxMode       = "workspace-write"
	defaultCodexAutoCompactTokens = "240000"
	codexWebSearchDisabled        = "disabled"
)

type CodexAdapter struct {
	config CodexAdapterConfig
}

func NewCodexAdapter(config CodexAdapterConfig) *CodexAdapter { return &CodexAdapter{config: config} }

func (a *CodexAdapter) Name() string { return RuntimeCodex }

func (a *CodexAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	agentCfg := agentConfigFromTurn(turn)
	if !agentCfg.AllowBash {
		return nil, fmt.Errorf(
			"codex runtime requires %s=true because the Codex CLI cannot disable shell execution",
			workerenv.AllowBash,
		)
	}

	outputFile, err := os.CreateTemp("", "codex-last-message-*")
	if err != nil {
		return nil, fmt.Errorf("create codex output temp file: %w", err)
	}
	outputPath := outputFile.Name()
	if err := outputFile.Close(); err != nil {
		_ = os.Remove(outputPath)
		return nil, fmt.Errorf("close codex output temp file: %w", err)
	}
	if err := prepareControlFileForChild(outputPath, 0o660); err != nil {
		_ = os.Remove(outputPath)
		return nil, fmt.Errorf("chown codex output temp file: %w", err)
	}

	instructionsPath, cleanupInstructions, err := writeCodexInstructionsFile(agentCfg)
	if err != nil {
		_ = os.Remove(outputPath)
		return nil, err
	}
	tempFiles := []string{outputPath}
	if instructionsPath != "" {
		if err := prepareControlFileForChild(instructionsPath, 0o640); err != nil {
			_ = os.Remove(outputPath)
			cleanupInstructions()
			return nil, fmt.Errorf("chown codex instructions temp file: %w", err)
		}
		tempFiles = append(tempFiles, instructionsPath)
		_ = cleanupInstructions // cleanup is represented by TempFiles for the command runner.
	}

	dir := firstNonEmpty(turn.WorkDir, a.config.WorkDir)
	if dir == "" {
		dir = DefaultWrapperWorkDir
	}
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			_ = os.Remove(outputPath)
			cleanupInstructions()
			return nil, fmt.Errorf("stat codex workspace directory: %w", err)
		}
		if wd, wdErr := os.Getwd(); wdErr == nil {
			dir = wd
		}
	}

	baseURL := firstNonEmpty(codexOpenAIBaseURL(), envEntryValue(turn.Env, workerenv.OpenAIBaseURL))
	return &CommandSpec{
		Path:       firstNonEmpty(a.config.Path, os.Getenv(workerenv.CodexCLIPath), defaultCodexPath),
		Args:       buildCodexArgs(agentCfg, outputPath, instructionsPath, false, baseURL),
		Env:        buildCodexEnv(turn.Env),
		Dir:        dir,
		Stdin:      []byte(turn.Prompt),
		ResultFile: outputPath,
		TempFiles:  tempFiles,
	}, nil
}

func (a *CodexAdapter) ParseResult(_ context.Context, _ TurnContext, run CommandResult) (TurnResult, error) {
	if strings.TrimSpace(run.ResultFile) != "" {
		data, err := readBoundedResultFile(run.ResultFile)
		if err != nil {
			return TurnResult{Result: run.Stdout}, err
		}
		if data.contents != "" {
			return TurnResult{Result: data.contents, Metadata: map[string]string{"adapter": RuntimeCodex}}, nil
		}
	}
	return TurnResult{Result: run.ExactStdout(), Metadata: map[string]string{"adapter": RuntimeCodex}}, nil
}

func buildCodexArgs(
	cfg *agentEnvConfig,
	outputPath string,
	instructionsPath string,
	bypassSandbox bool,
	baseURL string,
) []string {
	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--ephemeral",
		"--color", "never",
		"--output-last-message", outputPath,
		"--config", "approval_policy=never",
		"--config", "model_auto_compact_token_limit=" + codexAutoCompactTokenLimit(),
	}
	disableSandbox := os.Getenv(workerenv.CodexDisableSandbox)
	if bypassSandbox || workerenv.IsTrue(disableSandbox) {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		sandboxMode := codexSandboxMode()
		args = append(args, "--sandbox", sandboxMode)
		if sandboxMode == defaultCodexSandboxMode {
			args = append(args, "--config", "sandbox_workspace_write.network_access=true")
		}
	}
	if cfg != nil && strings.TrimSpace(cfg.Model) != "" {
		args = append(args, "--model", strings.TrimSpace(cfg.Model))
	}
	if instructionsPath != "" {
		args = append(args, "--config", "model_instructions_file="+instructionsPath)
	}
	if strings.TrimSpace(baseURL) != "" {
		args = append(args, "--config", "openai_base_url="+strings.TrimSpace(baseURL))
	}
	if webSearchSetting, ok := codexWebSearchSetting(cfg); ok {
		args = append(args, "--config", "web_search="+webSearchSetting)
	}
	args = append(args, "-")
	return args
}

func writeCodexInstructionsFile(cfg *agentEnvConfig) (string, func(), error) {
	instructions := buildCodexInstructions(cfg)
	if instructions == "" {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "codex-instructions-*.md")
	if err != nil {
		return "", func() {}, fmt.Errorf("create codex instructions temp file: %w", err)
	}
	if _, err := f.WriteString(instructions); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("write codex instructions temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("close codex instructions temp file: %w", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

func buildCodexInstructions(cfg *agentEnvConfig) string {
	if cfg == nil {
		cfg = &agentEnvConfig{MaxTurns: defaultCodexMaxTurns}
	}
	var sections []string
	if systemPrompt := strings.TrimSpace(cfg.SystemPrompt); systemPrompt != "" {
		sections = append(sections, systemPrompt)
	}
	var guidance []string
	if cfg.MaxTurns > 0 {
		guidance = append(guidance, fmt.Sprintf("Try to complete this task within %d turns.", cfg.MaxTurns))
	}
	if !cfg.AllowBash {
		guidance = append(guidance,
			"Do not use shell commands unless absolutely necessary. "+
				"Prefer built-in file inspection and editing tools.",
		)
	}
	allowedTools := trimmedTools(cfg.AllowedTools)
	if cfg.AllowedToolsSet && len(allowedTools) == 0 {
		guidance = append(guidance, "Do not use runtime tools for this task.")
	} else if len(allowedTools) > 0 {
		guidance = append(guidance, fmt.Sprintf(
			"Respect this requested tool allowlist when possible: %s.",
			strings.Join(allowedTools, ", "),
		))
	}
	if len(trimmedTools(cfg.DisallowedTools)) > 0 {
		guidance = append(guidance, fmt.Sprintf(
			"Do not use these tools: %s.",
			strings.Join(trimmedTools(cfg.DisallowedTools), ", "),
		))
	}
	if webSearchSetting, ok := codexWebSearchSetting(cfg); ok {
		if webSearchSetting == codexWebSearchDisabled {
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

func buildCodexEnv(extra []string) []string {
	baseURL := firstNonEmpty(codexOpenAIBaseURL(), envEntryValue(extra, workerenv.OpenAIBaseURL))
	env := removeTurnEnv(append([]string(nil), extra...), workerenv.OpenAIBaseURL)
	env = setEnv(env, "HOME", firstNonEmpty(envEntryValue(env, "HOME"), "/home/worker"))
	if baseURL != "" {
		env = setEnv(env, workerenv.OpenAIBaseURL, baseURL)
	}
	if envEntryValue(env, workerenv.CodexAPIKey) == "" {
		if apiKey := strings.TrimSpace(os.Getenv(workerenv.OpenAIAPIKey)); apiKey != "" {
			env = setEnv(env, workerenv.CodexAPIKey, apiKey)
		}
	}
	return env
}

func codexOpenAIBaseURL() string {
	return strings.TrimSpace(os.Getenv(workerenv.OpenAIBaseURL))
}

func codexAutoCompactTokenLimit() string {
	if limit := strings.TrimSpace(os.Getenv(workerenv.CodexAutoCompactTokenLimit)); limit != "" {
		return limit
	}
	return defaultCodexAutoCompactTokens
}

func codexSandboxMode() string {
	if mode := strings.TrimSpace(os.Getenv(workerenv.CodexSandboxMode)); mode != "" {
		return mode
	}
	return defaultCodexSandboxMode
}

func codexWebSearchSetting(cfg *agentEnvConfig) (string, bool) {
	if cfg == nil {
		return "", false
	}
	if hasWebSearchTool(cfg.DisallowedTools) {
		return codexWebSearchDisabled, true
	}
	if cfg.AllowedToolsSet {
		if hasWebSearchTool(cfg.AllowedTools) {
			return "live", true
		}
		return codexWebSearchDisabled, true
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
	replacer := strings.NewReplacer("_", "-", " ", "")
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(replacer.Replace(tool))), "-", "")
}

func trimmedTools(tools []string) []string {
	trimmed := make([]string, 0, len(tools))
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if tool != "" {
			trimmed = append(trimmed, tool)
		}
	}
	return trimmed
}
