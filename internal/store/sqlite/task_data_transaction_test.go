package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

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
