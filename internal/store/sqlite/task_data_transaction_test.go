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
		// The database callback excludes cleanup before any payload is read
		// or written. Network authorization runs before this callback.
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

func TestTaskDataAuthorizationDetectsCleanupAcrossConnections(t *testing.T) {
	for _, test := range []struct {
		name    string
		cleanup func(context.Context, *Store) error
	}{
		{"result", func(ctx context.Context, s *Store) error { return s.DeleteResult(ctx, "ns", "task") }},
		{"artifact", func(ctx context.Context, s *Store) error { return s.DeleteArtifacts(ctx, "ns", "task") }},
		{"plan", func(ctx context.Context, s *Store) error { return s.DeletePlan(ctx, "ns", "task") }},
		{"messages", func(ctx context.Context, s *Store) error { return s.DeleteTaskMessages(ctx, "ns", "task") }},
		{"parent messages", func(ctx context.Context, s *Store) error { return s.DeleteParentMessages(ctx, "ns", "parent") }},
		{"events", func(ctx context.Context, s *Store) error { return s.DeleteExecutionEvents(ctx, "ns", "task", "task") }},
		{"session", func(ctx context.Context, s *Store) error { return s.DeleteSession(ctx, "ns", "session") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "authorization.db")
			db, err := NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			cleanupDB, err := NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = cleanupDB.Close() })
			data, cleanup := NewStore(db, path), NewStore(cleanupDB, path)
			accessed := false
			err = data.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "", func(ctx context.Context) error {
				return test.cleanup(ctx, cleanup)
			}, func(context.Context) error {
				accessed = true
				return nil
			})
			require.ErrorIs(t, err, store.ErrTaskDataCleanupChanged)
			require.False(t, accessed)
		})
	}
}

func TestTaskDataAuthorizationAllowsUnrelatedWork(t *testing.T) {
	s := newCoexistenceTestStore(t)
	err := s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "", func(ctx context.Context) error {
		if err := s.SaveResult(ctx, "ns", "other-task", []byte("unrelated write")); err != nil {
			return err
		}
		return s.DeleteResult(ctx, "other-namespace", "task")
	}, func(ctx context.Context) error {
		return s.SaveResult(ctx, "ns", "task", []byte("authorized write"))
	})
	require.NoError(t, err)
	result, err := s.GetResult(t.Context(), "ns", "task")
	require.NoError(t, err)
	require.Equal(t, "authorized write", string(result))
}

func TestTaskGenerationRegistrationRequiresAuthorization(t *testing.T) {
	s := newCoexistenceTestStore(t)
	allow := func(context.Context) error { return nil }
	denied := errors.New("task identity unavailable")
	err := s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "missing", func(context.Context) error { return denied }, allow)
	require.ErrorIs(t, err, denied)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_data_task_generations`).Scan(&count))
	require.Zero(t, count, "rejected requests must not create persistent generation rows")

	require.NoError(t, s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "active", allow, allow))
	require.NoError(t, s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "active", func(ctx context.Context) error {
		return s.DeleteTaskJobRevocations(ctx, "ns", "unrelated", "other-uid")
	}, allow), "an authorized live Task must not retry because another Task was finalized")
}

func TestTaskGenerationRegistrationIsRemovedAfterRejectedAccess(t *testing.T) {
	for _, failure := range []string{"authorization", "cancellation", "data access"} {
		t.Run(failure, func(t *testing.T) {
			s := newCoexistenceTestStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			denied := errors.New("access denied")
			authorizations := 0
			err := s.WithAuthorizedTaskDataTransaction(ctx, "ns", "task", func(context.Context) error {
				authorizations++
				if authorizations == 1 || failure == "data access" {
					return nil
				}
				if failure == "cancellation" {
					cancel()
				}
				return denied
			}, func(context.Context) error { return denied })
			require.ErrorIs(t, err, denied)
			require.Equal(t, 2, authorizations, "registration must not reuse its initial authorization")
			var count int
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_data_task_generations`).Scan(&count))
			require.Zero(t, count)
		})
	}
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
