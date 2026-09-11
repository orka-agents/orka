package sqlite

import (
	"context"
	"path/filepath"
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
