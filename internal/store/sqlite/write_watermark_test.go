package sqlite

import (
	"path/filepath"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// Describe reports a new incarnation each time the store opens and the same
// one while it stays open, and derives the durability class from the
// synchronous mode the connection uses.
func TestDescribeReportsIncarnationAndDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orka.db")
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	first, err := s.Describe(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, first.Incarnation)
	require.Equal(t, storagev1.DurabilityLocalBuffered, first.Durability)
	require.Equal(t, storagev1.DatasetReady, first.State)
	again, err := s.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.Incarnation, again.Incarnation, "the incarnation is stable while the store stays open")
	require.NoError(t, s.Close())

	s, err = OpenLockedStore(path, WithSynchronous(SynchronousFull))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	reopened, err := s.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.DatasetID, reopened.DatasetID)
	require.NotEqual(t, first.Incarnation, reopened.Incarnation, "a reopened store is a new incarnation")
	require.Equal(t, storagev1.DurabilityLocalFsync, reopened.Durability)
	var mode int
	require.NoError(t, s.db.QueryRow(`PRAGMA synchronous`).Scan(&mode))
	require.Equal(t, 2, mode, "the store commits with synchronous=FULL")
}

func TestOpenLockedStoreRefusesUnknownSynchronousMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orka.db")
	s, err := OpenLockedStore(path, WithSynchronous("extra"))
	require.Error(t, err)
	require.Nil(t, s)
	again, err := OpenLockedStore(path)
	require.NoError(t, err, "a refused option leaves no process lock behind")
	require.NoError(t, again.Close())
}

func TestParseSynchronous(t *testing.T) {
	for value, want := range map[string]Synchronous{"normal": SynchronousNormal, " FULL ": SynchronousFull, "Full": SynchronousFull} {
		got, err := ParseSynchronous(value)
		require.NoError(t, err, value)
		require.Equal(t, want, got)
	}
	for _, value := range []string{"", "off", "extra"} {
		_, err := ParseSynchronous(value)
		require.Error(t, err, value)
	}
}

// The write watermark is part of the dataset's identity row, so it survives
// a reopen, and a copy of the file taken earlier holds the earlier value.
func TestWriteWatermarkSurvivesReopenAndMovesWithTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orka.db")
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	ctx := t.Context()
	first, err := s.AdvanceWriteWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), first.Value)
	snapshot := filepath.Join(dir, "snapshot.db")
	_, err = s.db.ExecContext(ctx, `VACUUM INTO ?`, snapshot)
	require.NoError(t, err)
	second, err := s.AdvanceWriteWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), second.Value)
	require.NoError(t, s.Close())

	s, err = OpenLockedStore(path)
	require.NoError(t, err)
	stored, err := s.GetWriteWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, second, stored)
	require.NoError(t, s.Close())

	restored, err := OpenLockedStore(snapshot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restored.Close() })
	older, err := restored.GetWriteWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, first, older, "a restored copy holds the watermark it had when it was taken")
	require.Equal(t, second.DatasetID, older.DatasetID, "and keeps the dataset identity")
}
