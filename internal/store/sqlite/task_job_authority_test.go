package sqlite

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func TestTaskJobRevocationSerializesAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.db")
	db, err := NewDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	otherDB, err := NewDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = otherDB.Close() })
	_, err = otherDB.ExecContext(t.Context(), `PRAGMA busy_timeout=0`)
	require.NoError(t, err)
	data, controller := NewStore(db, path), NewStore(otherDB, path)
	identity := store.TaskJobIdentity{Namespace: "ns", TaskUID: "task-uid", JobUID: "old-job-uid"}
	require.NoError(t, data.WithTaskDataTransaction(t.Context(), func(ctx context.Context) error {
		require.NoError(t, data.CheckTaskJobAuthority(ctx, identity))
		err := controller.RevokeTaskJob(t.Context(), identity)
		require.Error(t, err)
		require.True(t, isSQLiteRetryableError(err), "the transition must wait for already-authorized data access")
		return data.SaveResult(ctx, "ns", "task", []byte("authorized result"))
	}))
	require.NoError(t, controller.RevokeTaskJob(t.Context(), identity))
	require.NoError(t, controller.RevokeTaskJob(t.Context(), identity))
	err = data.WithTaskDataTransaction(t.Context(), func(ctx context.Context) error {
		return data.CheckTaskJobAuthority(ctx, identity)
	})
	require.ErrorIs(t, err, store.ErrTaskJobRevoked)
	require.NoError(t, data.DeleteResult(t.Context(), "ns", "task"))
	require.ErrorIs(t, data.CheckTaskJobAuthority(t.Context(), identity), store.ErrTaskJobRevoked)
	identity.JobUID = "next-job-uid"
	require.NoError(t, data.CheckTaskJobAuthority(t.Context(), identity))
}

func TestTaskJobRevocationCleanupFencesAccess(t *testing.T) {
	for _, taskName := range []string{"task", ""} {
		t.Run("fence="+taskName, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cleanup.db")
			db, err := NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			otherDB, err := NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = otherDB.Close() })
			_, err = otherDB.ExecContext(t.Context(), `PRAGMA busy_timeout=0`)
			require.NoError(t, err)
			data, cleanup := NewStore(db, path), NewStore(otherDB, path)
			identity := store.TaskJobIdentity{Namespace: "ns", TaskUID: "task-uid", JobUID: "old-job-uid"}
			other := store.TaskJobIdentity{Namespace: "ns", TaskUID: "other-task-uid", JobUID: "other-job-uid"}
			require.NoError(t, data.RevokeTaskJob(t.Context(), identity))
			require.NoError(t, data.RevokeTaskJob(t.Context(), other))
			require.NoError(t, data.WithTaskDataTransaction(t.Context(), func(context.Context) error {
				err := cleanup.DeleteTaskJobRevocations(t.Context(), "ns", "task", identity.TaskUID)
				require.Error(t, err)
				require.True(t, isSQLiteRetryableError(err), "cleanup must serialize with data access")
				return nil
			}))
			require.ErrorIs(t, data.CheckTaskJobAuthority(t.Context(), identity), store.ErrTaskJobRevoked)
			accessed := false
			err = data.WithAuthorizedTaskDataTransaction(t.Context(), "ns", taskName, func(ctx context.Context) error {
				return cleanup.DeleteTaskJobRevocations(ctx, "ns", "task", identity.TaskUID)
			}, func(context.Context) error {
				accessed = true
				return nil
			})
			require.ErrorIs(t, err, store.ErrTaskDataCleanupChanged)
			require.False(t, accessed, "removing revocations must invalidate in-flight authorization")
			require.NoError(t, data.CheckTaskJobAuthority(t.Context(), identity))
			require.ErrorIs(t, data.CheckTaskJobAuthority(t.Context(), other), store.ErrTaskJobRevoked)
		})
	}
}

func TestTaskFinalizationReclaimsGenerationRows(t *testing.T) {
	s := newCoexistenceTestStore(t)
	allow := func(context.Context) error { return nil }
	require.NoError(t, s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "active", allow, allow))
	for index := range 25 {
		name := "finished-" + strconv.Itoa(index)
		require.NoError(t, s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", name, allow, allow))
		require.NoError(t, s.DeleteResult(t.Context(), "ns", name))
		require.NoError(t, s.DeleteTaskJobRevocations(t.Context(), "ns", name, "uid-"+name))
	}
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_data_task_generations WHERE namespace = 'ns'`).Scan(&count))
	require.Equal(t, 1, count, "only the live Task's generation should remain")
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_data_cleanup_generations WHERE namespace = 'ns'`).Scan(&count))
	require.Equal(t, 1, count, "reclamation uses one persistent generation per namespace")
}

func TestTaskGenerationReclamationDoesNotReuseAnAuthorization(t *testing.T) {
	for _, registered := range []bool{false, true} {
		for _, reused := range []bool{false, true} {
			t.Run("registered="+strconv.FormatBool(registered)+"/reused="+strconv.FormatBool(reused), func(t *testing.T) {
				s := newCoexistenceTestStore(t)
				allow := func(context.Context) error { return nil }
				if registered {
					require.NoError(t, s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "task", allow, allow))
					require.NoError(t, s.DeletePlan(t.Context(), "ns", "task"))
				}
				accessed := false
				err := s.WithAuthorizedTaskDataTransaction(t.Context(), "ns", "task", func(ctx context.Context) error {
					if err := s.DeleteTaskJobRevocations(ctx, "ns", "task", "old-task-uid"); err != nil {
						return err
					}
					if reused {
						// A new incarnation re-registers the same name before the old
						// request reaches its data transaction.
						return s.WithAuthorizedTaskDataTransaction(ctx, "ns", "task", allow, allow)
					}
					return nil
				}, func(context.Context) error {
					accessed = true
					return nil
				})
				require.ErrorIs(t, err, store.ErrTaskDataCleanupChanged)
				require.False(t, accessed)
			})
		}
	}
}
