package sqlite

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

var testSnapshotKey = bytes.Repeat([]byte{0x42}, storageclient.SnapshotKeyBytes)

func orkaClient(t *testing.T, s storageclient.EmbeddedStore) *storageclient.Client {
	t.Helper()
	return storageclienttest.NewEmbedded(t, s)
}

func activateSnapshotKey(t *testing.T, s *Store, key []byte) (*storageclient.Client, error) {
	t.Helper()
	client := orkaClient(t, s)
	if err := client.ActivateSnapshotKey(context.Background(), key); err != nil {
		return nil, err
	}
	info, err := s.DatasetInfo()
	if err != nil {
		return nil, err
	}
	if err := s.CompleteDatasetBootstrap(context.Background(), info.ID); err != nil {
		return nil, err
	}
	s.SetNativeSessionFinalizationSealer(client)
	return client, nil
}

func activateTestKey(t *testing.T, s *Store) *storageclient.Client {
	t.Helper()
	client, err := activateSnapshotKey(t, s, testSnapshotKey)
	if err != nil {
		t.Fatalf("activate snapshot key: %v", err)
	}
	return client
}

type scratchSealer struct {
	store  *Store
	client *storageclient.Client
}

func newScratchSealer(t *testing.T) scratchSealer {
	t.Helper()
	s := setupDiskStore(t)
	return scratchSealer{store: s, client: activateTestKey(t, s)}
}

func (s scratchSealer) persistSnapshot(t *testing.T, target *Store, snapshot store.AgentExecutionSnapshot) {
	t.Helper()
	require.NoError(t, s.client.PersistAgentExecutionSnapshot(t.Context(), snapshot))
	sealed, err := s.store.GetSealedAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest})
	require.NoError(t, err)
	require.NoError(t, target.CreateSealedAgentExecutionSnapshot(t.Context(), *sealed))
}

func (s scratchSealer) putConnectorCredential(t *testing.T, target *Store, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) {
	t.Helper()
	require.NoError(t, s.client.PutConnectorCredential(t.Context(), ref, credential))
	sealed, err := s.store.GetSealedConnectorCredential(t.Context(), ref.ConnectionUID)
	require.NoError(t, err)
	_, err = target.PutSealedConnectorCredential(t.Context(), storagev1.PutSealedConnectorCredentialRequest{Credential: *sealed, Now: time.Now()})
	require.NoError(t, err)
}

func (s scratchSealer) stageNativeImport(t *testing.T, target *Store, request store.NativeSessionImport) {
	t.Helper()
	receipt, err := s.client.StageNativeSessionImport(t.Context(), request)
	require.NoError(t, err)
	sealed, err := s.store.GetSealedNativeSession(t.Context(), request.Namespace, request.SessionName, "")
	require.NoError(t, err)
	_, err = target.StageSealedNativeSessionImport(t.Context(), storagev1.StageSealedNativeSessionImportRequest{Record: *sealed, Receipt: *receipt})
	require.NoError(t, err)
}
