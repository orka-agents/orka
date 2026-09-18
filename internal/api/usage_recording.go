package api

import (
	"context"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
)

func usageRequestContext(ctx context.Context, backend any, namespace, session string) context.Context {
	usageStore, ok := backend.(store.UsageStore)
	if !ok {
		return ctx
	}
	return llm.WithUsageRecorder(ctx, func(ctx context.Context, observation store.UsageObservation) error {
		observation.Namespace, observation.SessionName = namespace, session
		return usageStore.RecordUsage(ctx, observation)
	})
}
