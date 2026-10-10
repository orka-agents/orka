package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
)

func TestUsageRequestContextFreezesNamespaceBeforeFirstObservation(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			backend := newInternalExecutionEventStore(t)
			kube := testInternalExecutionEventClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}})
			ctx, cancel := context.WithCancel(usageRequestContext(t.Context(), backend, kube, "default", "session"))
			detached := detachedSpanContext(ctx)
			cancel()
			require.NoError(t, detached.Err())
			// Recreate after detachment, before the provider's first observation.
			require.NoError(t, kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
			require.NoError(t, kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "replacement-uid"}}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveUsagePersistenceFixture(w, r, true)
			}))
			t.Cleanup(server.Close)
			provider, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
			require.NoError(t, err)
			request := &llm.CompletionRequest{Model: "model", Messages: []llm.Message{{Role: "user", Content: "fixture"}}}
			if mode == "stream" {
				stream, err := provider.Stream(detached, request)
				require.NoError(t, err)
				for chunk := range stream {
					require.NoError(t, chunk.Error)
				}
			} else {
				_, err = provider.Complete(detached, request)
				require.NoError(t, err)
			}
			data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"default"}, AsOf: time.Now().UTC()})
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(data.Observations), 2)
			for _, observation := range data.Observations {
				require.Equal(t, "namespace-uid", observation.NamespaceUID)
				require.Equal(t, "session", observation.SessionName)
			}
			current, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"default"}, NamespaceUIDs: map[string]string{"default": "replacement-uid"}})
			require.NoError(t, err)
			require.Empty(t, current.Observations)
		})
	}
}

func TestUsageRequestContextRetainsNamespaceLookupFailure(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			backend := newInternalExecutionEventStore(t)
			kube := testInternalExecutionEventClient(t)
			ctx := detachedSpanContext(usageRequestContext(t.Context(), backend, kube, "missing", "session"))
			// A namespace that appears later must not acquire the failed request.
			require.NoError(t, kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "missing", UID: "replacement-uid"}}))
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				serveUsagePersistenceFixture(w, r, true)
			}))
			t.Cleanup(server.Close)
			provider, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
			require.NoError(t, err)
			request := &llm.CompletionRequest{Model: "model", Messages: []llm.Message{{Role: "user", Content: "fixture"}}}
			if mode == "stream" {
				stream, streamErr := provider.Stream(ctx, request)
				err = streamErr
				if stream != nil {
					for range stream {
					}
				}
			} else {
				_, err = provider.Complete(ctx, request)
			}
			require.True(t, llm.IsUsagePersistenceError(err), "identity failure must prevent the provider call: %v", err)
			require.Zero(t, calls.Load())
			data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"missing"}, AsOf: time.Now().UTC()})
			require.NoError(t, err)
			require.Empty(t, data.Observations)
		})
	}
}
