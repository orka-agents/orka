package sqlite

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func fileBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return info.Size()
}

func TestDescribeReportsBindingFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	ctx := t.Context()

	fresh, err := s.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, storagev1.ContractRevision, fresh.ContractRevision)
	require.Equal(t, storagev1.DatasetReady, fresh.State)
	require.NotEmpty(t, fresh.Incarnation)
	require.Equal(t, time.UTC, fresh.ObservedAt.Location())
	require.WithinDuration(t, time.Now(), fresh.ObservedAt, time.Minute)
	require.Equal(t, storagev1.DatasetBootstrap{Empty: true}, fresh.Bootstrap, "a new dataset is empty and not bootstrapped")
	require.NotNil(t, fresh.Usage)
	require.Equal(t, fileBytes(t, path)+fileBytes(t, path+"-wal"), fresh.Usage.DatasetBytes,
		"usage counts the database file and its write-ahead log")

	require.NoError(t, s.SaveResult(ctx, "ns", "task", make([]byte, 64<<10)))
	written, err := s.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, storagev1.DatasetBootstrap{}, written.Bootstrap, "a dataset with a record is not empty")
	require.Equal(t, fileBytes(t, path)+fileBytes(t, path+"-wal"), written.Usage.DatasetBytes)
	require.Equal(t, fresh.Incarnation, written.Incarnation, "the incarnation is stable while the store stays open")

	require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
	bootstrapped, err := s.Describe(ctx)
	require.NoError(t, err)
	require.True(t, bootstrapped.Bootstrap.Complete, "key activation completes the bootstrap")
	require.NoError(t, s.Close())

	s, err = OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	reopened, err := s.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, fresh.DatasetID, reopened.DatasetID)
	require.NotEqual(t, fresh.Incarnation, reopened.Incarnation, "reopening the database is a new incarnation")
	require.True(t, reopened.Bootstrap.Complete)
}

func TestDescribeCompletedEmptyDataset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
	info, err := s.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, storagev1.DatasetBootstrap{Complete: true, Empty: true}, info.Bootstrap,
		"completion and emptiness are reported independently")
	require.NoError(t, s.Close())
}

func TestDescribeWithoutUsageForInMemoryDatabase(t *testing.T) {
	info, err := setupTestStore(t).Describe(t.Context())
	require.NoError(t, err)
	require.Nil(t, info.Usage, "an in-memory database reports no usage")
}

func TestDescribeReportsUnsupportedSchemaAsIncompatible(t *testing.T) {
	s := setupDiskStore(t)
	_, err := s.db.Exec(`PRAGMA user_version=2`)
	require.NoError(t, err)
	_, err = s.Describe(t.Context())
	require.ErrorIs(t, err, storagev1.ErrIncompatible)
}
