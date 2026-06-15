package cliwrapper

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	RuntimeGeneric = "generic"
	RuntimeClaude  = "claude"
	RuntimeCodex   = "codex"
	RuntimeCopilot = "copilot"
)

// RuntimeAdapter translates a harness turn into one per-turn CLI command.
// Adapters are intentionally observed-mode bridges: the wrapper observes the
// subprocess lifecycle and output, but it does not broker individual tool calls
// for opaque CLI runtimes.
type RuntimeAdapter interface {
	Name() string
	BuildCommand(ctx context.Context, turn TurnContext) (*CommandSpec, error)
	ParseResult(ctx context.Context, turn TurnContext, run CommandResult) (TurnResult, error)
}

// EventingAdapter is an optional test/special adapter that emits harness frames
// directly instead of going through the command runner. Production CLI adapters
// should normally implement RuntimeAdapter only.
type EventingAdapter interface {
	RuntimeAdapter
	RunTurn(ctx context.Context, turn TurnContext, emit func(harness.HarnessEventFrame) error) (TurnResult, error)
}

// TurnContext is the internal, command-adapter-facing representation of a
// harness StartTurn request plus wrapper runtime configuration.
type TurnContext struct {
	RuntimeName      string
	Namespace        string
	TaskName         string
	SessionName      string
	RuntimeSessionID string
	TurnID           string
	CorrelationID    string
	Prompt           string
	WorkDir          string
	Env              []string
	Deadline         time.Time
	Metadata         map[string]string
}

// CommandSpec describes the subprocess invocation for a single turn.
type CommandSpec struct {
	Path       string
	Args       []string
	Env        []string
	Dir        string
	Stdin      []byte
	ResultFile string
	TempFiles  []string
}

// CommandResult captures bounded process output and lifecycle metadata.
type CommandResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	TimedOut   bool
	Cancelled  bool
	ResultFile string
}

// TurnResult is the adapter-normalized terminal result for a turn.
type TurnResult struct {
	Result    string
	OutputRef string
	Metadata  map[string]string
}

func SupportedRuntimeAdapters() []string {
	return []string{RuntimeGeneric, RuntimeCodex, RuntimeClaude, RuntimeCopilot, RuntimeMulti}
}

func NewRuntimeAdapter(cfg Config) (RuntimeAdapter, error) {
	runtime := strings.ToLower(strings.TrimSpace(cfg.Runtime))
	if runtime == "" {
		runtime = RuntimeGeneric
	}
	switch runtime {
	case RuntimeGeneric:
		adapter := NewGenericAdapter(cfg.Generic)
		if err := adapter.Validate(); err != nil {
			return nil, err
		}
		return adapter, nil
	case RuntimeCodex:
		return NewCodexAdapter(cfg.Codex), nil
	case RuntimeClaude:
		return NewClaudeAdapter(cfg.Claude), nil
	case RuntimeCopilot:
		return NewCopilotAdapter(cfg.Copilot), nil
	case RuntimeMulti:
		return NewMultiAdapter(cfg), nil
	default:
		return nil, fmt.Errorf(
			"unsupported runtime adapter %q (supported: %s)",
			cfg.Runtime,
			strings.Join(SupportedRuntimeAdapters(), ", "),
		)
	}
}

func turnContextFromRequest(runtimeName string, cfg Config, request harness.StartTurnRequest) TurnContext {
	workDir := strings.TrimSpace(cfg.WorkDir)
	metadata := make(map[string]string, len(request.Metadata)+4)
	maps.Copy(metadata, request.Metadata)
	metadata["toolExecutionMode"] = string(request.ToolExecutionMode)
	env := turnEnvFromRequest(cfg, request, metadata)
	return TurnContext{
		RuntimeName:      runtimeName,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: string(request.RuntimeSessionID),
		TurnID:           string(request.TurnID),
		CorrelationID:    request.CorrelationID,
		Prompt:           request.Input.Prompt,
		WorkDir:          workDir,
		Env:              env,
		Deadline:         request.Deadline,
		Metadata:         metadata,
	}
}

func turnEnvFromRequest(cfg Config, request harness.StartTurnRequest, metadata map[string]string) []string {
	env := append([]string(nil), cfg.CommandEnv...)
	for _, item := range request.Input.Env {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		env = setEnv(env, name, item.Value)
	}
	env = setEnv(env, workerenv.TaskName, request.TaskName)
	env = setEnv(env, workerenv.TaskNamespace, request.Namespace)
	env = setEnv(env, workerenv.Prompt, request.Input.Prompt)
	metadataEnv := map[string]string{
		"model":            workerenv.Model,
		"systemPrompt":     workerenv.SystemPrompt,
		"maxTurns":         workerenv.MaxTurns,
		"allowedTools":     workerenv.AllowedTools,
		"disallowedTools":  workerenv.DisallowedTools,
		"gitRepo":          workerenv.GitRepo,
		"gitBranch":        workerenv.GitBranch,
		"gitRef":           workerenv.GitRef,
		"workspaceSubPath": workerenv.WorkspaceSubpath,
		"forkRepo":         workerenv.ForkRepo,
		"prBaseBranch":     workerenv.PRBaseBranch,
		"prBaseRepo":       workerenv.PRBaseRepo,
		"prBaseSHA":        workerenv.PRBaseSHA,
		"pushBranch":       workerenv.PushBranch,
	}
	for key, envName := range metadataEnv {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			env = setEnv(env, envName, value)
		}
	}
	if strings.EqualFold(strings.TrimSpace(metadata["allowBash"]), "true") {
		env = setEnv(env, workerenv.AllowBash, "true")
	}
	if strings.TrimSpace(metadata["pushBranch"]) != "" {
		env = setEnv(env, workerenv.RequirePushBranch, "true")
	}
	if strings.EqualFold(strings.TrimSpace(metadata["claudeBare"]), "true") {
		env = setEnv(env, workerenv.ClaudeBare, "true")
	}
	if strings.EqualFold(strings.TrimSpace(metadata["claudeDisableSettingSources"]), "true") {
		env = setEnv(env, workerenv.ClaudeDisableSettingSources, "true")
	}
	if value := strings.TrimSpace(metadata["claudePermissionMode"]); value != "" {
		env = setEnv(env, workerenv.ClaudePermissionMode, value)
	}
	return env
}

func agentConfigFromEnv(defaultMaxTurns int) *agentEnvConfig {
	cfg := &agentEnvConfig{
		TaskName:        os.Getenv(workerenv.TaskName),
		TaskNamespace:   os.Getenv(workerenv.TaskNamespace),
		Prompt:          os.Getenv(workerenv.Prompt),
		Model:           os.Getenv(workerenv.Model),
		SystemPrompt:    os.Getenv(workerenv.SystemPrompt),
		AllowedTools:    splitCSV(os.Getenv(workerenv.AllowedTools)),
		AllowedToolsSet: strings.TrimSpace(os.Getenv(workerenv.AllowedTools)) != "",
		DisallowedTools: splitCSV(os.Getenv(workerenv.DisallowedTools)),
		MaxTurns:        defaultMaxTurns,
		AllowBash:       workerenv.IsTrue(os.Getenv(workerenv.AllowBash)),
	}
	if v := strings.TrimSpace(os.Getenv(workerenv.MaxTurns)); v != "" {
		var parsed int
		_, _ = fmt.Sscanf(v, "%d", &parsed)
		if parsed > 0 {
			cfg.MaxTurns = parsed
		}
	}
	return cfg
}

func agentConfigFromTurn(turn TurnContext) *agentEnvConfig {
	cfg := agentConfigFromEnv(50)
	if value := strings.TrimSpace(turn.Metadata["model"]); value != "" {
		cfg.Model = value
	}
	if value := strings.TrimSpace(turn.Metadata["systemPrompt"]); value != "" {
		cfg.SystemPrompt = value
	}
	if value := strings.TrimSpace(turn.Metadata["maxTurns"]); value != "" {
		var parsed int
		_, _ = fmt.Sscanf(value, "%d", &parsed)
		if parsed > 0 {
			cfg.MaxTurns = parsed
		}
	}
	if value := strings.TrimSpace(turn.Metadata["allowedTools"]); value != "" {
		metadataAllowed := splitCSV(value)
		if cfg.AllowedToolsSet {
			cfg.AllowedTools = intersectTools(cfg.AllowedTools, metadataAllowed)
		} else {
			cfg.AllowedTools = metadataAllowed
		}
		cfg.AllowedToolsSet = true
	}
	if value := strings.TrimSpace(turn.Metadata["disallowedTools"]); value != "" {
		cfg.DisallowedTools = unionTools(cfg.DisallowedTools, splitCSV(value))
	}
	if value := strings.TrimSpace(turn.Metadata["allowBash"]); value != "" {
		cfg.AllowBash = cfg.AllowBash && strings.EqualFold(value, "true")
	}
	return cfg
}

type agentEnvConfig struct {
	TaskName        string
	TaskNamespace   string
	Prompt          string
	Model           string
	SystemPrompt    string
	AllowedTools    []string
	AllowedToolsSet bool
	DisallowedTools []string
	MaxTurns        int
	AllowBash       bool
}

func intersectTools(left, right []string) []string {
	rightSet := map[string]struct{}{}
	for _, tool := range right {
		rightSet[normalizeToolName(tool)] = struct{}{}
	}
	out := make([]string, 0, len(left))
	seen := map[string]struct{}{}
	for _, tool := range left {
		normalized := normalizeToolName(tool)
		if _, ok := rightSet[normalized]; !ok {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, strings.TrimSpace(tool))
	}
	return out
}

func unionTools(left, right []string) []string {
	out := make([]string, 0, len(left)+len(right))
	seen := map[string]struct{}{}
	for _, tools := range [][]string{left, right} {
		for _, tool := range tools {
			tool = strings.TrimSpace(tool)
			if tool == "" {
				continue
			}
			normalized := normalizeToolName(tool)
			if _, ok := seen[normalized]; ok {
				continue
			}
			seen[normalized] = struct{}{}
			out = append(out, tool)
		}
	}
	return out
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
