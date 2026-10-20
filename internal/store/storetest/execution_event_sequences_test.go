package storetest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventStoreFakeLatestSeqsScopesAndRefreshesHeads(t *testing.T) {
	fake := NewFakeExecutionEventStore()
	for _, stream := range []struct {
		namespace string
		id        string
		count     int
	}{
		{namespace: "default", id: "task-a", count: 2},
		{namespace: "default", id: "task-b", count: 1},
		{namespace: "other", id: "task-a", count: 3},
	} {
		for range stream.count {
			_, err := fake.AppendExecutionEvent(t.Context(), &store.ExecutionEvent{
				Namespace: stream.namespace, StreamID: stream.id, Type: events.ExecutionEventTypeModelMessage,
			})
			require.NoError(t, err)
		}
	}
	requested := []string{" task-a ", "missing", "task-b", "task-a"}
	first, err := fake.GetLatestExecutionEventSeqs(t.Context(), " default ", " task ", requested)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"task-a": 2, "task-b": 1, "missing": 0}, first)

	_, err = fake.AppendExecutionEvent(t.Context(), &store.ExecutionEvent{
		Namespace: "default", StreamID: "task-a", Type: events.ExecutionEventTypeModelMessage,
	})
	require.NoError(t, err)
	require.NoError(t, fake.DeleteExecutionEvents(t.Context(), "default", "", "task-b"))
	latest, err := fake.GetLatestExecutionEventSeqs(t.Context(), "default", "", requested)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"task-a": 3, "task-b": 0, "missing": 0}, latest)
	require.Equal(t, map[string]int64{"task-a": 2, "task-b": 1, "missing": 0}, first)
}

func TestExecutionEventStoreFakeLatestSeqsValidation(t *testing.T) {
	fake := NewFakeExecutionEventStore()
	latest, err := fake.GetLatestExecutionEventSeqs(t.Context(), "default", "", nil)
	require.NoError(t, err)
	require.Empty(t, latest)
	latest, err = fake.GetLatestExecutionEventSeqs(t.Context(), "default", "session", []string{"task-a"})
	require.ErrorIs(t, err, store.ErrValidation)
	require.Nil(t, latest)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	latest, err = fake.GetLatestExecutionEventSeqs(ctx, "default", "", []string{"task-a"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, latest)
}
