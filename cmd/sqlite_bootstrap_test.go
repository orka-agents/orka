package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestControllerSQLiteBootstrapSecretLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	ref := types.NamespacedName{Namespace: "orka-system", Name: "snapshot"}
	c := fake.NewClientBuilder().WithScheme(snapshotSecretTestScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
	}).Build()
	loader := func(info sqlite.DatasetInfo) ([]byte, error) {
		return ensureAgentExecutionSnapshotKey(ctx, c, c, ref.Namespace,
			agentExecutionSnapshotSecretOptions{Name: ref.Name, Key: "key"}, info.CanBootstrapSnapshotKey)
	}
	// A failed key load after schema creation must release the lock and leave
	// resumable fresh history rather than misclassifying file existence.
	failed, _, err := openControllerSQLiteStore(path, func(sqlite.DatasetInfo) ([]byte, error) {
		return nil, errors.New("unavailable key service")
	})
	require.ErrorContains(t, err, "unavailable key service")
	require.Nil(t, failed)
	s, key, err := openControllerSQLiteStore(path, loader)
	require.NoError(t, err)
	first, err := s.DatasetInfo()
	require.NoError(t, err)
	require.False(t, first.CanBootstrapSnapshotKey)
	require.NoError(t, s.SaveResult(ctx, "ns", "task", []byte("retained result")))
	require.NoError(t, s.Close())
	s, again, err := openControllerSQLiteStore(path, loader)
	require.NoError(t, err)
	require.True(t, bytes.Equal(key, again), "restart replaced key")
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.Equal(t, first, info)
	data, err := s.GetResult(ctx, "ns", "task")
	require.NoError(t, err)
	require.Equal(t, "retained result", string(data))
	require.NoError(t, s.Close())
	secret := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, ref, secret))
	secret.Data = nil
	require.NoError(t, c.Update(ctx, secret))
	failed, _, err = openControllerSQLiteStore(path, loader)
	require.ErrorContains(t, err, "restore the Secret from backup")
	require.Nil(t, failed)
	require.NoError(t, c.Get(ctx, ref, secret))
	require.Empty(t, secret.Data)
	// A completed dataset remains an existing installation even after all
	// ordinary rows have been deleted.
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.Exec(`DELETE FROM results`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	failed, _, err = openControllerSQLiteStore(path, loader)
	require.Error(t, err)
	require.Nil(t, failed)
}

func TestControllerSQLiteBootstrapExcludesCompetingKeyMint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan *sqlite.Store, 1)
	errs := make(chan error, 1)
	go func() {
		s, _, err := openControllerSQLiteStore(path, func(info sqlite.DatasetInfo) ([]byte, error) {
			close(entered)
			<-release
			return bytes.Repeat([]byte{0x42}, 32), nil
		})
		errs <- err
		done <- s
	}()
	select {
	case <-entered:
	case err := <-errs:
		t.Fatalf("first store failed before loading its key: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("first store did not reach key loading")
	}
	called := false
	second, _, err := openControllerSQLiteStore(path, func(sqlite.DatasetInfo) ([]byte, error) {
		called = true
		return nil, nil
	})
	close(release)
	first := <-done
	require.NoError(t, <-errs)
	defer func() { _ = first.Close() }()
	require.ErrorContains(t, err, "process lock")
	require.Nil(t, second)
	require.False(t, called)
}

func TestControllerSQLiteBootstrapRejectsSchemaBeforeKeyLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := sqlite.NewDB(path)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version=2`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	called := false
	s, _, err := openControllerSQLiteStore(path, func(sqlite.DatasetInfo) ([]byte, error) {
		called = true
		return nil, nil
	})
	require.ErrorContains(t, err, "unsupported SQLite schema version")
	require.Nil(t, s)
	require.False(t, called)
}

func TestControllerSQLiteBootstrapResumesAfterKeyWriteLostResponse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	ref := types.NamespacedName{Namespace: "orka-system", Name: "snapshot"}
	c := fake.NewClientBuilder().WithScheme(snapshotSecretTestScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
	}).Build()
	opts := agentExecutionSnapshotSecretOptions{Name: ref.Name, Key: "key"}
	firstLoader := func(info sqlite.DatasetInfo) ([]byte, error) {
		_, err := ensureAgentExecutionSnapshotKey(ctx, c, c, ref.Namespace, opts, info.CanBootstrapSnapshotKey)
		require.NoError(t, err)
		// Simulate losing the startup result after the API server committed the key.
		return nil, errors.New("lost startup response")
	}
	failed, _, err := openControllerSQLiteStore(path, firstLoader)
	require.ErrorContains(t, err, "lost startup response")
	require.Nil(t, failed)
	secret := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, ref, secret))
	expected, err := decodeAgentExecutionSnapshotKey(secret.Data["key"])
	require.NoError(t, err)
	s, actual, err := openControllerSQLiteStore(path, func(info sqlite.DatasetInfo) ([]byte, error) {
		require.True(t, info.CanBootstrapSnapshotKey)
		return ensureAgentExecutionSnapshotKey(ctx, c, c, ref.Namespace, opts, info.CanBootstrapSnapshotKey)
	})
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.True(t, bytes.Equal(expected, actual), "retry replaced the committed key")
}
