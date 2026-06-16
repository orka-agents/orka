package cliwrapper

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/sozercan/orka/internal/workerenv"
)

const (
	defaultClaudePath     = "claude"
	defaultClaudeMaxTurns = 50
)

type ClaudeAdapter struct {
	config ClaudeAdapterConfig
}

func NewClaudeAdapter(config ClaudeAdapterConfig) *ClaudeAdapter {
	return &ClaudeAdapter{config: config}
}

func (a *ClaudeAdapter) Name() string { return RuntimeClaude }

func (a *ClaudeAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	agentCfg := agentConfigFromTurn(turn)
	dir := firstNonEmpty(turn.WorkDir, a.config.WorkDir)
	if dir == "" {
		dir = DefaultWrapperWorkDir
	}
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat claude workspace directory: %w", err)
		}
		if wd, wdErr := os.Getwd(); wdErr == nil {
			dir = wd
		}
	}

	return &CommandSpec{
		Path:  firstNonEmpty(a.config.Path, os.Getenv(workerenv.ClaudeCLIPath), defaultClaudePath),
		Args:  buildClaudeArgs(agentCfg, turn),
		Env:   buildClaudeEnv(turn.Env),
		Dir:   dir,
		Stdin: nil,
	}, nil
}

func (a *ClaudeAdapter) ParseResult(_ context.Context, _ TurnContext, run CommandResult) (TurnResult, error) {
	return TurnResult{Result: run.Stdout, Metadata: map[string]string{"adapter": RuntimeClaude}}, nil
}

func buildClaudeArgs(cfg *agentEnvConfig, turn TurnContext) []string {
	if cfg == nil {
		cfg = &agentEnvConfig{MaxTurns: defaultClaudeMaxTurns}
	}
	args := []string{"--print", "--verbose"}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	if systemPrompt := strings.TrimSpace(cfg.SystemPrompt); systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultClaudeMaxTurns
	}
	args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	if cfg.AllowBash {
		args = append(args, "--dangerously-skip-permissions")
	}
	if envEntryIsTrue(turn.Env, workerenv.ClaudeBare) {
		args = append(args, "--bare")
	}
	if envEntryIsTrue(turn.Env, workerenv.ClaudeDisableSettingSources) {
		args = append(args, "--setting-sources", "")
	}
	if permissionMode := strings.TrimSpace(envEntryValue(turn.Env, workerenv.ClaudePermissionMode)); permissionMode != "" {
		args = append(args, "--permission-mode", permissionMode)
	}
	if tools := claudeAvailableTools(cfg.AllowedTools); len(tools) > 0 {
		args = append(args, "--tools", strings.Join(tools, ","))
	}
	for _, tool := range cfg.AllowedTools {
		if tool = strings.TrimSpace(tool); tool != "" {
			args = append(args, "--allowedTools", tool)
		}
	}
	for _, tool := range cfg.DisallowedTools {
		if tool = strings.TrimSpace(tool); tool != "" {
			args = append(args, "--disallowedTools", tool)
		}
	}
	args = append(args, "-p", turn.Prompt)
	return args
}

func buildClaudeEnv(extra []string) []string {
	env := append([]string(nil), extra...)
	env = setEnv(env, "HOME", firstNonEmpty(envEntryValue(env, "HOME"), "/home/worker"))
	return env
}

func claudeAvailableTools(allowedTools []string) []string {
	seen := map[string]struct{}{}
	tools := make([]string, 0, len(allowedTools))
	for _, allowedTool := range allowedTools {
		tool := claudeToolName(allowedTool)
		if tool == "" {
			continue
		}
		if _, ok := seen[tool]; ok {
			continue
		}
		seen[tool] = struct{}{}
		tools = append(tools, tool)
	}
	return tools
}

func claudeToolName(toolSpec string) string {
	toolSpec = strings.TrimSpace(toolSpec)
	if name, _, ok := strings.Cut(toolSpec, "("); ok {
		toolSpec = name
	}
	return strings.TrimSpace(toolSpec)
}

func envEntryValue(env []string, key string) string {
	prefix := strings.TrimSpace(key) + "="
	for i := len(env) - 1; i >= 0; i-- {
		if after, ok := strings.CutPrefix(env[i], prefix); ok {
			return after
		}
	}
	return ""
}

func envEntryIsTrue(env []string, key string) bool {
	return strings.EqualFold(strings.TrimSpace(envEntryValue(env, key)), "true")
}
