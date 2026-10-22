package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func embeddedBindRequest(dataset string) storagev1.BindDatasetRequest {
	return storagev1.BindDatasetRequest{
		DatasetID: dataset, InstallationID: "installation-a", RecordUID: "record-a", SwitchGeneration: 1,
		KeyFingerprint: "hmac-sha256:key", Mode: storagev1.BindEmbedded,
	}
}

func lockedTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func describeDataset(t *testing.T, s *Store) string {
	t.Helper()
	info, err := s.Describe(t.Context())
	require.NoError(t, err)
	return info.DatasetID
}

// An embedded binding is accepted only from a store opened under its process
// lock, in every bootstrap state: new, completed, holding records or adopted
// from a historical layout.
func TestDatasetBindingEmbeddedNeedsTheProcessLock(t *testing.T) {
	unlocked := setupDiskStore(t)
	_, err := unlocked.BindDataset(t.Context(), embeddedBindRequest(describeDataset(t, unlocked)))
	require.ErrorIs(t, err, store.ErrValidation)
	_, err = unlocked.GetDatasetBinding(t.Context(), describeDataset(t, unlocked))
	require.ErrorIs(t, err, store.ErrNotFound)

	for name, prepare := range map[string]func(t *testing.T) *Store{
		"new": func(t *testing.T) *Store {
			s, _ := lockedTestStore(t)
			return s
		},
		"completed": func(t *testing.T) *Store {
			s, _ := lockedTestStore(t)
			require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
			return s
		},
		"holding records": func(t *testing.T) *Store {
			s, _ := lockedTestStore(t)
			require.NoError(t, s.SaveResult(t.Context(), "ns", "task", []byte("retained")))
			return s
		},
		"adopted": func(t *testing.T) *Store {
			path, db := historicalSchemaDB(t, "abd2c040")
			seedMigrationRecords(t, db, path)
			require.NoError(t, db.Close())
			s, err := OpenLockedStore(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := prepare(t)
			dataset := describeDataset(t, s)
			bound, err := s.BindDataset(t.Context(), embeddedBindRequest(dataset))
			require.NoError(t, err)
			require.Equal(t, "installation-a", bound.InstallationID)
			require.Equal(t, storagev1.DatasetBound, bound.State)
		})
	}
}

// A fresh binding needs a dataset SQLite created new: an adopted historical
// dataset is refused even when it holds no record and never completed its
// bootstrap, as a snapshot key is never minted for it.
func TestDatasetBindingFreshRefusesAnAdoptedDataset(t *testing.T) {
	path, db := historicalSchemaDB(t, "abd2c040")
	require.NoError(t, db.Close())
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.False(t, info.CanBootstrapSnapshotKey)
	request := embeddedBindRequest(info.ID)
	request.Mode = storagev1.BindFresh
	_, err = s.BindDataset(t.Context(), request)
	var conflict storagev1.BindingConflictError
	require.ErrorAs(t, err, &conflict)
	require.Contains(t, conflict.Reason, "adopted")
	require.Nil(t, conflict.Binding)
}

// The binding is checkpointed before BindDataset returns, as dataset identity
// is, and it survives a reopen.
func TestDatasetBindingIsSynchronizedAndSurvivesReopen(t *testing.T) {
	s, path := lockedTestStore(t)
	dataset := describeDataset(t, s)
	bound, err := s.BindDataset(t.Context(), embeddedBindRequest(dataset))
	require.NoError(t, err)

	// immutable=1 reads only the main database file, not the WAL.
	raw, err := sql.Open("sqlite", "file:"+path+"?immutable=1")
	require.NoError(t, err)
	var installation, state string
	var generation int64
	require.NoError(t, raw.QueryRow(`SELECT installation_id, binding_state, switch_generation FROM store_dataset`).Scan(
		&installation, &state, &generation))
	require.NoError(t, raw.Close())
	require.Equal(t, "installation-a", installation)
	require.Equal(t, "bound", state)
	require.EqualValues(t, 1, generation)

	require.NoError(t, s.Close())
	reopened, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	stored, err := reopened.GetDatasetBinding(t.Context(), dataset)
	require.NoError(t, err)
	require.Equal(t, bound, stored)
	repeat, err := reopened.BindDataset(t.Context(), embeddedBindRequest(dataset))
	require.NoError(t, err)
	require.Equal(t, bound, repeat)
}

// A bound dataset's controller epoch moves only through the advance, as for a
// dataset an advance recorded an installation in.
func TestDatasetBindingRefusesMirrorSync(t *testing.T) {
	s, _ := lockedTestStore(t)
	dataset := describeDataset(t, s)
	epoch := store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "holder",
		RequestDigest: store.CanonicalBytesDigest([]byte("holder")), Version: 1,
		AcquiredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err := s.BindDataset(t.Context(), embeddedBindRequest(dataset))
	require.NoError(t, err)
	require.ErrorIs(t, s.SyncControllerEpochMirror(t.Context(), epoch), store.ErrFenced)
	_, err = s.GetControllerEpochMirror(t.Context(), store.DefaultControllerEpochName)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// The binding methods record durable outcomes, so they refuse to run inside a
// task-data transaction that could still roll back.
func TestDatasetBindingRefusesAnEnclosingTransaction(t *testing.T) {
	s, _ := lockedTestStore(t)
	dataset := describeDataset(t, s)
	err := s.WithTaskDataTransaction(t.Context(), func(ctx context.Context) error {
		_, err := s.BindDataset(ctx, embeddedBindRequest(dataset))
		require.ErrorIs(t, err, store.ErrValidation)
		_, err = s.DetachDatasetBinding(ctx, storagev1.DetachDatasetRequest{
			DatasetID: dataset, InstallationID: "installation-a", RecordUID: "record-a", SwitchGeneration: 2,
		})
		require.ErrorIs(t, err, store.ErrValidation)
		_, err = s.UpdateBindingKeyFingerprint(ctx, storagev1.UpdateBindingKeyRequest{
			DatasetID: dataset, InstallationID: "installation-a", RecordUID: "record-a", SwitchGeneration: 1,
			KeyFingerprint: "hmac-sha256:other",
		})
		require.ErrorIs(t, err, store.ErrValidation)
		require.ErrorIs(t, s.CompleteDatasetBootstrap(ctx, dataset), store.ErrValidation)
		return nil
	})
	require.NoError(t, err)
	_, err = s.GetDatasetBinding(t.Context(), dataset)
	require.ErrorIs(t, err, store.ErrNotFound)
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.True(t, info.CanBootstrapSnapshotKey, "the refused completion recorded nothing")
}

// Completing the bootstrap through the contract has the effect the snapshot key
// activation has: SQLite no longer lets a key be minted for the dataset.
func TestDatasetBindingCompleteBootstrapEndsKeyMinting(t *testing.T) {
	s, path := lockedTestStore(t)
	dataset := describeDataset(t, s)
	require.NoError(t, s.CompleteDatasetBootstrap(t.Context(), dataset))
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.False(t, info.CanBootstrapSnapshotKey)
	requireCheckpointedDataset(t, path, dataset, true)
}
