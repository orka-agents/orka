package main

import (
	"context"
	"encoding/json"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/workers/common"
)

func withWorkerUsage(ctx context.Context, recorder common.EventRecorder) context.Context {
	if _, noop := recorder.(common.NoopEventRecorder); noop || recorder == nil {
		return ctx
	}
	return llm.WithUsageRecorder(ctx, func(ctx context.Context, observation store.UsageObservation) error {
		content, err := json.Marshal(map[string]any{"usage": observation})
		if err != nil {
			return err
		}
		return common.RecordEventStrict(ctx, recorder, events.ExecutionEventTypeModelUsageUpdated,
			common.WithEventSummary("Model call usage recorded"), common.WithEventContent(content))
	})
}
