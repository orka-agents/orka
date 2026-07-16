package cliwrapper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	defaultCodexPath              = "codex"
	defaultCodexMaxTurns          = 50
	defaultCodexSandboxMode       = "workspace-write"
	defaultCodexAutoCompactTokens = "240000"
	codexWebSearchDisabled        = "disabled"
	codexReasoningEffortEnv       = "ORKA_CODEX_REASONING_EFFORT"
)

type CodexAdapter struct {
	config CodexAdapterConfig
}

func NewCodexAdapter(config CodexAdapterConfig) *CodexAdapter { return &CodexAdapter{config: config} }

func (a *CodexAdapter) Name() string { return RuntimeCodex }

func (a *CodexAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	agentCfg := agentConfigFromTurn(turn)
	readOnly := strings.EqualFold(strings.TrimSpace(turn.Metadata["readOnly"]), "true")
	if !agentCfg.AllowBash && !readOnly {
		return nil, fmt.Errorf(
			"codex runtime requires %s=true unless trusted read-only metadata forces the read-only sandbox",
			workerenv.AllowBash,
		)
	}
	reasoningEffort, err := codexReasoningEffort(turn.Metadata)
	if err != nil {
		return nil, err
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
	if readOnly || strings.EqualFold(strings.TrimSpace(turn.Metadata["runtimeAuthOnly"]), "true") {
		baseURL = firstNonEmpty(envEntryValue(turn.Env, workerenv.OpenAIBaseURL), codexOpenAIBaseURL())
	}
	return &CommandSpec{
		Path:       firstNonEmpty(a.config.Path, os.Getenv(workerenv.CodexCLIPath), defaultCodexPath),
		Args:       buildCodexArgs(agentCfg, outputPath, instructionsPath, false, baseURL, reasoningEffort, readOnly),
		Env:        buildCodexEnv(turn.Env, baseURL, readOnly),
		UnsetEnv:   codexUnsetEnv(readOnly),
		ClearEnv:   readOnly,
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
	reasoningEffort string,
	forceReadOnly bool,
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
	if forceReadOnly {
		args = append(args,
			"--sandbox", "read-only",
			"--ignore-user-config",
			"--ignore-rules",
			"--disable", "hooks",
			"--disable", "shell_snapshot",
			"--disable", "apps",
			"--disable", "plugins",
			"--disable", "remote_plugin",
			"--disable", "enable_mcp_apps",
			"--disable", "skill_mcp_dependency_install",
			"--disable", "tool_call_mcp_elicitation",
			"--config", "project_doc_max_bytes=0",
		)
	} else if bypassSandbox || workerenv.IsTrue(disableSandbox) {
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
	if reasoningEffort != "" {
		args = append(args, "--config", "model_reasoning_effort="+reasoningEffort)
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

func buildCodexEnv(extra []string, baseURL string, readOnly bool) []string {
	if readOnly {
		return buildReadOnlyCodexEnv(extra, baseURL)
	}
	// Codex receives the turn prompt on stdin. Remove the explicit copy here;
	// BuildCommand also unsets any inherited copy after the final environment merge.
	env := removeTurnEnv(
		append([]string(nil), extra...),
		workerenv.OpenAIBaseURL,
		workerenv.Prompt,
	)
	home := firstNonEmpty(envEntryValue(env, "HOME"), "/home/worker")
	env = setEnv(env, "HOME", home)
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

func buildReadOnlyCodexEnv(extra []string, baseURL string) []string {
	home := firstNonEmpty(envEntryValue(extra, "HOME"), "/home/worker")
	env := []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"PATH=" + wrapperSafeCommandPath,
		"TMPDIR=/tmp",
		"USER=node",
		"LOGNAME=node",
		"SHELL=/bin/sh",
		"TERM=dumb",
		"GIT_TERMINAL_PROMPT=0",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	}
	if baseURL = strings.TrimSpace(baseURL); baseURL != "" {
		env = setEnv(env, workerenv.OpenAIBaseURL, baseURL)
	}
	if value := strings.TrimSpace(envEntryValue(extra, workerenv.OpenAIAPIKey)); value != "" {
		env = setEnv(env, workerenv.OpenAIAPIKey, value)
	}
	if value := strings.TrimSpace(envEntryValue(extra, workerenv.CodexAPIKey)); value != "" {
		env = setEnv(env, workerenv.CodexAPIKey, value)
	}
	return env
}

func codexOpenAIBaseURL() string {
	return strings.TrimSpace(os.Getenv(workerenv.OpenAIBaseURL))
}

func codexReasoningEffort(metadata map[string]string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		metadata["reasoningEffort"],
		os.Getenv(codexReasoningEffortEnv),
	)))
	if effort == "" {
		return "", nil
	}
	switch effort {
	case "low", "medium", "high", "xhigh":
		return effort, nil
	default:
		return "", fmt.Errorf("invalid %s %q: expected low, medium, high, or xhigh", codexReasoningEffortEnv, effort)
	}
}

func codexUnsetEnv(readOnly bool) []string {
	unset := []string{workerenv.Prompt}
	if !readOnly {
		return unset
	}
	return append(unset,
		"NODE_OPTIONS", "NODE_PATH", "BASH_ENV", "ENV", "ZDOTDIR",
		"LD_PRELOAD", "LD_AUDIT", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
		"PYTHONPATH", "PERL5OPT", "RUBYOPT", "NPM_CONFIG_USERCONFIG",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME",
	)
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
