package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestTaskDataTransactionSerializesCleanupAcrossConnections(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "task-data.db")
	writerDB, err := NewDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writerDB.Close() })
	cleanupDB, err := NewDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanupDB.Close() })
	writer, cleanup := NewStore(writerDB, dbPath), NewStore(cleanupDB, dbPath)
	// A zero busy timeout makes writer exclusion deterministic without sleeps
	// or timing assertions, even when cleanup uses another controller connection.
	_, err = cleanupDB.ExecContext(ctx, `PRAGMA busy_timeout=0`)
	require.NoError(t, err)
	require.NoError(t, writer.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		// The cleanup writer is already excluded before authority is checked
		// and before any payload has been written.
		cleanupErr := cleanup.DeleteArtifacts(ctx, "ns", "task")
		require.Error(t, cleanupErr)
		require.True(t, isSQLiteRetryableError(cleanupErr))
		if err := writer.SaveResult(txCtx, "ns", "task", []byte("result")); err != nil {
			return err
		}
		if err := writer.SaveArtifact(txCtx, "ns", "task", "output.txt", "text/plain", []byte("artifact")); err != nil {
			return err
		}
		if err := writer.SavePlan(txCtx, "ns", "task", &store.PlanState{Summary: "plan"}); err != nil {
			return err
		}
		return writer.SendMessage(txCtx, &store.Message{Namespace: "ns", FromTask: "task", ToTask: "peer", ParentTask: "parent", Content: "message"})
	}))
	result, err := cleanup.GetResult(ctx, "ns", "task")
	require.NoError(t, err)
	require.Equal(t, "result", string(result))
	artifacts, err := cleanup.ListArtifacts(ctx, "ns", "task")
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	plan, err := cleanup.GetPlan(ctx, "ns", "task")
	require.NoError(t, err)
	require.Equal(t, "plan", plan.Summary)
	messages, err := cleanup.GetMessages(ctx, "ns", "peer", "parent", false)
	require.NoError(t, err)
	require.Len(t, messages, 1)

	require.NoError(t, cleanup.DeleteResult(ctx, "ns", "task"))
	require.NoError(t, cleanup.DeleteArtifacts(ctx, "ns", "task"))
	require.NoError(t, cleanup.DeletePlan(ctx, "ns", "task"))
	require.NoError(t, cleanup.DeleteTaskMessages(ctx, "ns", "task"))
	_, err = cleanup.GetResult(ctx, "ns", "task")
	require.ErrorIs(t, err, store.ErrNotFound)
	artifacts, err = cleanup.ListArtifacts(ctx, "ns", "task")
	require.NoError(t, err)
	require.Empty(t, artifacts)
	_, err = cleanup.GetPlan(ctx, "ns", "task")
	require.ErrorIs(t, err, store.ErrNotFound)
	messages, err = cleanup.GetMessages(ctx, "ns", "peer", "parent", false)
	require.NoError(t, err)
	require.Empty(t, messages)
}

func TestTaskDataTransactionRollsBackRejectedMutation(t *testing.T) {
	s := newCoexistenceTestStore(t)
	ctx := context.Background()
	denied := errors.New("task identity changed")
	err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		if err := s.SaveResult(txCtx, "ns", "task", []byte("stale")); err != nil {
			return err
		}
		return denied
	})
	require.ErrorIs(t, err, denied)
	_, err = s.GetResult(ctx, "ns", "task")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestTaskDataTransactionEventAndInboxCommitRollback(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			s := newCoexistenceTestStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, s.SendMessage(ctx, &store.Message{
				Namespace: "ns", FromTask: "peer", ToTask: "task", ParentTask: "parent", Content: "message",
			}))
			denied := errors.New("task identity changed")
			err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
				messages, err := s.GetMessages(txCtx, "ns", "task", "parent", true)
				if err != nil {
					return err
				}
				require.Len(t, messages, 1)
				event := &store.ExecutionEvent{
					Namespace: "ns", StreamType: "task", StreamID: "task", TaskName: "task", SessionName: "session",
					Type: events.ExecutionEventTypeWorkerStarted,
				}
				first, err := s.AppendExecutionEvent(txCtx, event)
				if err != nil {
					return err
				}
				require.EqualValues(t, 1, first.Seq)
				second, added, err := s.AppendExecutionEventWithPlanIfAbsent(txCtx, event, "event-key", &store.PlanState{
					Namespace: "ns", TaskName: "task", Summary: "plan",
				})
				if err != nil {
					return err
				}
				require.True(t, added)
				require.EqualValues(t, 2, second.Seq)
				duplicate, added, err := s.AppendExecutionEventIfAbsent(txCtx, event, "event-key")
				if err != nil {
					return err
				}
				require.False(t, added)
				require.Equal(t, second.ID, duplicate.ID)
				if !commit {
					return denied
				}
				return nil
			})
			if commit {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, denied)
			}
			messages, err := s.GetMessages(ctx, "ns", "task", "parent", false)
			require.NoError(t, err)
			listed, latest, err := s.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{Namespace: "ns", SessionName: "session"})
			require.NoError(t, err)
			plan, planErr := s.GetPlan(ctx, "ns", "task")
			if commit {
				require.Empty(t, messages)
				require.Len(t, listed, 2)
				require.EqualValues(t, 2, latest)
				require.NoError(t, planErr)
				require.Equal(t, "plan", plan.Summary)
			} else {
				require.Len(t, messages, 1, "aborted transaction must not consume inbox messages")
				require.Empty(t, listed)
				require.Zero(t, latest)
				require.ErrorIs(t, planErr, store.ErrNotFound)
			}
		})
	}
}
