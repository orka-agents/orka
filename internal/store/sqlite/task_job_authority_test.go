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
