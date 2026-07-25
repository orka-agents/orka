package cliwrapper

import (
	"context"
	"fmt"
	"strings"
)

const RuntimeMulti = "multi"

type MultiAdapter struct {
	adapters map[string]RuntimeAdapter
}

func NewMultiAdapter(cfg Config) *MultiAdapter {
	adapters := map[string]RuntimeAdapter{
		RuntimeCodex:    NewCodexAdapter(cfg.Codex),
		RuntimeClaude:   NewClaudeAdapter(cfg.Claude),
		RuntimeCopilot:  NewCopilotAdapter(cfg.Copilot),
		RuntimeOpencode: NewOpencodeAdapter(cfg.Opencode),
	}
	if strings.TrimSpace(cfg.Generic.Command) != "" {
		adapters[RuntimeGeneric] = NewGenericAdapter(cfg.Generic)
	}
	return &MultiAdapter{adapters: adapters}
}

func (a *MultiAdapter) Name() string { return RuntimeMulti }

func (a *MultiAdapter) SupportedRuntimes() []string {
	if a == nil {
		return nil
	}
	ordered := []string{RuntimeGeneric, RuntimeCodex, RuntimeClaude, RuntimeCopilot, RuntimeOpencode}
	out := make([]string, 0, len(a.adapters))
	for _, runtime := range ordered {
		if _, ok := a.adapters[runtime]; ok {
			out = append(out, runtime)
		}
	}
	return out
}

func (a *MultiAdapter) BuildCommand(ctx context.Context, turn TurnContext) (*CommandSpec, error) {
	adapter, runtime, err := a.adapterFor(turn)
	if err != nil {
		return nil, err
	}
	turn.RuntimeName = runtime
	return adapter.BuildCommand(ctx, turn)
}

func (a *MultiAdapter) ParseResult(ctx context.Context, turn TurnContext, run CommandResult) (TurnResult, error) {
	adapter, runtime, err := a.adapterFor(turn)
	if err != nil {
		return TurnResult{}, err
	}
	turn.RuntimeName = runtime
	return adapter.ParseResult(ctx, turn, run)
}

func (a *MultiAdapter) adapterFor(turn TurnContext) (RuntimeAdapter, string, error) {
	if a == nil {
		return nil, "", fmt.Errorf("multi runtime adapter is required")
	}
	runtime := strings.ToLower(strings.TrimSpace(turn.Metadata["runtime"]))
	if runtime == "" || runtime == RuntimeMulti {
		runtime = strings.ToLower(strings.TrimSpace(turn.RuntimeName))
	}
	if runtime == "" || runtime == RuntimeMulti {
		return nil, "", fmt.Errorf("turn runtime is required for multi adapter")
	}
	adapter := a.adapters[runtime]
	if adapter == nil {
		return nil, runtime, fmt.Errorf("runtime adapter %q is not configured", runtime)
	}
	return adapter, runtime, nil
}
