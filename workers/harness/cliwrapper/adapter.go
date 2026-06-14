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
	return []string{RuntimeGeneric, RuntimeCodex}
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
	case RuntimeClaude, RuntimeCopilot:
		return nil, fmt.Errorf("runtime adapter %q is reserved but not implemented", runtime)
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
		Env:              cfg.CommandEnv,
		Deadline:         request.Deadline,
		Metadata:         metadata,
	}
}

func agentConfigFromEnv(defaultMaxTurns int) *agentEnvConfig {
	cfg := &agentEnvConfig{
		TaskName:        os.Getenv(workerenv.TaskName),
		TaskNamespace:   os.Getenv(workerenv.TaskNamespace),
		Prompt:          os.Getenv(workerenv.Prompt),
		Model:           os.Getenv(workerenv.Model),
		SystemPrompt:    os.Getenv(workerenv.SystemPrompt),
		AllowedTools:    splitCSV(os.Getenv(workerenv.AllowedTools)),
		DisallowedTools: splitCSV(os.Getenv(workerenv.DisallowedTools)),
		MaxTurns:        defaultMaxTurns,
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

type agentEnvConfig struct {
	TaskName        string
	TaskNamespace   string
	Prompt          string
	Model           string
	SystemPrompt    string
	AllowedTools    []string
	DisallowedTools []string
	MaxTurns        int
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
