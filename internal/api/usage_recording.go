package api

import (
	"context"

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
	// Freeze ownership before streaming can detach and outlive the namespace.
	// Keep lookup failures too, so a later namespace cannot claim this request.
	namespaceUID, identityErr := usage.NamespaceUID(ctx, reader, namespace)
	return llm.WithUsageRecorder(ctx, func(ctx context.Context, observation store.UsageObservation) error {
		if identityErr != nil {
			return identityErr
		}
		observation.Namespace, observation.NamespaceUID, observation.SessionName = namespace, namespaceUID, session
		return usageStore.RecordUsage(ctx, observation)
	})
}
