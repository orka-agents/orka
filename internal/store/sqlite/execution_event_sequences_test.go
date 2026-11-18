package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventStoreLatestSeqsScopesAndRefreshesHeads(t *testing.T) {
	s := setupDiskStore(t)
	appendSequenceBatchEvents(t, s, "default", "task-a", 3)
	appendSequenceBatchEvents(t, s, "default", "task-b", 1)
	appendSequenceBatchEvents(t, s, "default", "unrequested", 4)
	appendSequenceBatchEvents(t, s, "other", "task-a", 5)
	requested := []string{" task-a ", "missing", "task-b", "task-a"}

	first, err := s.GetLatestExecutionEventSeqs(t.Context(), " default ", " task ", requested)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"task-a": 3, "task-b": 1, "missing": 0}, first)

	appendSequenceBatchEvents(t, s, "default", "task-a", 1)
	require.NoError(t, s.DeleteExecutionEvents(t.Context(), "default", store.ExecutionEventStreamTypeTask, "task-b"))
	latest, err := s.GetLatestExecutionEventSeqs(t.Context(), "default", "", requested)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"task-a": 4, "task-b": 0, "missing": 0}, latest)
	require.Equal(t, map[string]int64{"task-a": 3, "task-b": 1, "missing": 0}, first,
		"the previously returned sequence snapshot must not change after a later append")

	other, err := s.GetLatestExecutionEventSeqs(t.Context(), "other", "", []string{"task-a", "task-b"})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"task-a": 5, "task-b": 0}, other)

	// Recovery reads persisted stream heads after a store reopen, without a
	// process-local append notification or sequence cache.
	require.NoError(t, s.db.Close())
	db, err := NewDB(s.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	reopened := NewStore(db, s.dbPath)
	persisted, err := reopened.GetLatestExecutionEventSeqs(t.Context(), "default", "", requested)
	require.NoError(t, err)
	require.Equal(t, latest, persisted)
}

func TestExecutionEventStoreLatestSeqsReturnsEveryBoundedBatch(t *testing.T) {
	s := setupDiskStore(t)
	count := sqliteExecutionEventSeqBatchSize*2 + 3
	requested := make([]string, 0, count+2)
	want := make(map[string]int64, count)
	for i := range count {
		streamID := fmt.Sprintf("task-batch-%d", i)
		requested = append(requested, streamID)
		want[streamID] = 0
		if i%sqliteExecutionEventSeqBatchSize == 0 || i == count-1 {
			appendSequenceBatchEvents(t, s, "default", streamID, 2)
			want[streamID] = 2
		}
	}
	requested = append(requested, requested[0], " "+requested[count-1]+" ")
	latest, err := s.GetLatestExecutionEventSeqs(t.Context(), "default", "", requested)
	require.NoError(t, err)
	require.Equal(t, want, latest)

	// Query rows must release the single connection before projection writes.
	appendSequenceBatchEvents(t, s, "default", requested[0], 1)
	refreshed, err := s.GetLatestExecutionEventSeqs(t.Context(), "default", "", requested)
	require.NoError(t, err)
	want[requested[0]]++
	require.Equal(t, want, refreshed)
}

func TestExecutionEventStoreLatestSeqsValidationAndReadFailure(t *testing.T) {
	t.Run("empty input requires no database query", func(t *testing.T) {
		s := NewStore(nil, "")
		latest, err := s.GetLatestExecutionEventSeqs(t.Context(), "default", "", nil)
		require.NoError(t, err)
		require.Empty(t, latest)
	})
	t.Run("invalid stream type", func(t *testing.T) {
		s := NewStore(nil, "")
		latest, err := s.GetLatestExecutionEventSeqs(t.Context(), "default", "session", []string{"task-a"})
		require.ErrorIs(t, err, store.ErrValidation)
		require.Nil(t, latest)
	})
	t.Run("cancelled read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		s := NewStore(nil, "")
		latest, err := s.GetLatestExecutionEventSeqs(ctx, "default", "", []string{"task-a"})
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, latest)
	})
	t.Run("unavailable database does not return zero sequences", func(t *testing.T) {
		s := setupDiskStore(t)
		require.NoError(t, s.db.Close())
		latest, err := s.GetLatestExecutionEventSeqs(t.Context(), "default", "", []string{"task-a"})
		require.Error(t, err)
		require.Nil(t, latest)
	})
}

func appendSequenceBatchEvents(t *testing.T, s *Store, namespace, streamID string, count int) {
	t.Helper()
	for range count {
		_, err := s.AppendExecutionEvent(t.Context(), &store.ExecutionEvent{
			Namespace: namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: streamID,
			TaskName: streamID, Type: events.ExecutionEventTypeModelMessage,
		})
		require.NoError(t, err)
	}
}
