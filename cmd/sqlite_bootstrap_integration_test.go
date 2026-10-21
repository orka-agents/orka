package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func bootstrapHistoricalDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prior.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	for _, name := range []string{
		"testdata/schema/0e567418.sql", "migrations/149-to-150.sql", "migrations/150-to-162.sql",
		"migrations/162-to-166.sql", "testdata/bootstrap-snapshot.sql",
	} {
		data, err := os.ReadFile(filepath.Join("../internal/store/sqlite", name))
		require.NoError(t, err)
		_, err = db.Exec(string(data))
		require.NoError(t, err)
	}
	return path
}

func TestControllerSQLiteBootstrapBothKeyModes(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "key file", true: "managed Secret"}[managed], func(t *testing.T) {
			path := bootstrapHistoricalDatabase(t)
			material := bytes.Repeat([]byte{0x42}, 32)
			wrong := bytes.Repeat([]byte{0x43}, 32)
			keyPath := filepath.Join(t.TempDir(), "operator-key")
			c := fake.NewClientBuilder().WithScheme(snapshotSecretTestScheme(t)).WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "orka-system"},
			}).Build()
			setKey := func(key []byte) {
				if !managed {
					require.NoError(t, os.WriteFile(keyPath, key, 0o600))
					return
				}
				secret := &corev1.Secret{}
				require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "orka-system", Name: "snapshot"}, secret))
				secret.Data = map[string][]byte{"key": key}
				require.NoError(t, c.Update(t.Context(), secret))
			}
			load := func(info sqlite.DatasetInfo) ([]byte, error) {
				require.False(t, info.CanBootstrapSnapshotKey, "adoption must never mint a key")
				if managed {
					return ensureAgentExecutionSnapshotKey(t.Context(), c, c, "orka-system",
						agentExecutionSnapshotSecretOptions{Name: "snapshot", Key: "key"}, info.CanBootstrapSnapshotKey)
				}
				return loadAgentExecutionSnapshotKey(keyPath)
			}
			var identity string
			for _, candidate := range []string{"missing", "wrong", "right"} {
				switch candidate {
				case "wrong":
					setKey(wrong)
				case "right":
					setKey(material)
				}
				s, key, err := openControllerSQLiteStore(path, load)
				if candidate != "right" {
					require.Error(t, err)
					require.Nil(t, s)
					require.Empty(t, key)
					continue
				}
				require.NoError(t, err)
				require.True(t, bytes.Equal(material, key), "key changed")
				info, err := s.DatasetInfo()
				require.NoError(t, err)
				identity = info.ID
				body := []byte(`{"prompt":"retained encrypted prompt"}`)
				snapshot, err := s.GetAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshotKey{
					TaskUID: "fixture-task-uid", Digest: store.CanonicalAgentExecutionSnapshotDigest(body),
				})
				require.NoError(t, err)
				require.True(t, bytes.Equal(body, snapshot.Body), "snapshot changed")
				require.NoError(t, s.SaveResult(t.Context(), "ns", "new", []byte("after adoption")))
				require.NoError(t, s.Close())
			}
			s, _, err := openControllerSQLiteStore(path, load)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()
			info, err := s.DatasetInfo()
			require.NoError(t, err)
			require.Equal(t, identity, info.ID)
			data, err := s.GetResult(t.Context(), "ns", "new")
			require.NoError(t, err)
			require.Equal(t, "after adoption", string(data))
		})
	}
}

func TestControllerSQLiteBootstrapRealProcessLockAndCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	command := func(mode string) {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestControllerSQLiteBootstrapChildProcess$")
		cmd.Env = append(os.Environ(), "ORKA_TEST_BOOTSTRAP_PATH="+path, "ORKA_TEST_BOOTSTRAP_MODE="+mode)
		out, err := cmd.CombinedOutput()
		if mode == "crash" {
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "%s", out)
			require.Equal(t, 31, exit.ExitCode(), "%s", out)
		} else {
			require.NoError(t, err, "%s", out)
		}
	}
	command("crash") // Schema+identity committed, key loading interrupted without cleanup.
	var identity string
	s, _, err := openControllerSQLiteStore(path, func(info sqlite.DatasetInfo) ([]byte, error) {
		identity = info.ID
		require.True(t, info.CanBootstrapSnapshotKey)
		return bytes.Repeat([]byte{0x42}, 32), nil
	})
	require.NoError(t, err)
	command("locked") // An actual second process must not enter its key loader.
	require.NoError(t, s.Close())
	s, _, err = openControllerSQLiteStore(path, func(info sqlite.DatasetInfo) ([]byte, error) {
		require.Equal(t, identity, info.ID)
		require.False(t, info.CanBootstrapSnapshotKey)
		return bytes.Repeat([]byte{0x42}, 32), nil
	})
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestControllerSQLiteBootstrapChildProcess(t *testing.T) {
	path := os.Getenv("ORKA_TEST_BOOTSTRAP_PATH")
	if path == "" {
		t.Skip("test subprocess")
	}
	mode := os.Getenv("ORKA_TEST_BOOTSTRAP_MODE")
	called := false
	s, _, err := openControllerSQLiteStore(path, func(sqlite.DatasetInfo) ([]byte, error) {
		called = true
		if mode == "crash" {
			os.Exit(31)
		}
		return bytes.Repeat([]byte{0x42}, 32), nil
	})
	if s != nil {
		_ = s.Close()
	}
	require.Equal(t, "locked", mode)
	require.ErrorContains(t, err, "process lock")
	require.False(t, called)
}

type bootstrapConflictWriter struct {
	client.Client
	winner    []byte
	conflicts int
}

func (w *bootstrapConflictWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	winner := &corev1.Secret{}
	if err := w.Get(ctx, client.ObjectKeyFromObject(obj), winner); err != nil {
		return err
	}
	winner.Data = map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString(w.winner))}
	if err := w.Client.Update(ctx, winner); err != nil {
		return err
	}
	err := w.Client.Update(ctx, obj, opts...)
	if apierrors.IsConflict(err) {
		w.conflicts++
	}
	return err
}

func TestControllerSQLiteBootstrapSecretConflictWithAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server Secret conflict proof")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	api, err := client.New(config, client.Options{Scheme: snapshotSecretTestScheme(t)})
	require.NoError(t, err)
	require.NoError(t, api.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-test"}}))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "bootstrap-test"}}
	require.NoError(t, api.Create(t.Context(), secret))
	winner := bytes.Repeat([]byte{0x42}, 32)
	writer := &bootstrapConflictWriter{Client: api, winner: winner}
	path := filepath.Join(t.TempDir(), "store.db")
	load := func(info sqlite.DatasetInfo) ([]byte, error) {
		return ensureAgentExecutionSnapshotKey(t.Context(), api, writer, "bootstrap-test",
			agentExecutionSnapshotSecretOptions{Name: "snapshot", Key: "key"}, info.CanBootstrapSnapshotKey)
	}
	s, key, err := openControllerSQLiteStore(path, load)
	require.NoError(t, err)
	require.Equal(t, 1, writer.conflicts, "must exercise real resourceVersion conflict")
	require.True(t, bytes.Equal(winner, key), "loser must use API-committed winner key")
	require.NoError(t, s.Close())
	s, key, err = openControllerSQLiteStore(path, load)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.True(t, bytes.Equal(winner, key), "reopen replaced winner key")
	require.Equal(t, 1, writer.conflicts)
}
