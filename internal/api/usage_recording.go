package api

import (
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func usageRequestContext(ctx context.Context, backend any, reader client.Reader, namespace, session string) context.Context {
	usageStore, ok := backend.(store.UsageStore)
	if !ok {
		return ctx
	}
	var identity sync.Once
	var namespaceUID string
	var identityErr error
	return llm.WithUsageRecorder(ctx, func(ctx context.Context, observation store.UsageObservation) error {
		// Freeze ownership for the whole request, including detached streaming
		// and finish records written after namespace deletion or recreation.
		identity.Do(func() {
			namespaceUID, identityErr = usage.NamespaceUID(ctx, reader, namespace)
		})
		if identityErr != nil {
			return identityErr
		}
		observation.Namespace, observation.NamespaceUID, observation.SessionName = namespace, namespaceUID, session
		return usageStore.RecordUsage(ctx, observation)
	})
}
