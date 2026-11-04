package sqlite

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// testSnapshotKey is the snapshot key tests activate unless they cover key
// rotation.
var testSnapshotKey = bytes.Repeat([]byte{0x42}, storageclient.SnapshotKeyBytes)

// orkaClient wraps s in the storage client. Orka's redaction, payload
// sanitization, usage attribution and sealing run in the client, so tests
// that cover those behaviors end to end write through it.
func orkaClient(t *testing.T, s *Store) *storageclient.Client {
	t.Helper()
	client, err := storageclient.NewEmbedded(s)
	if err != nil {
		t.Fatalf("NewEmbedded: %v", err)
	}
	return client
}

// activateSnapshotKey wraps s in the storage client and activates key, then
// records bootstrap completion, in the order startup uses. The client also
// seals native state for the duplicate SQL control path.
func activateSnapshotKey(t *testing.T, s *Store, key []byte) (*storageclient.Client, error) {
	t.Helper()
	client := orkaClient(t, s)
	if err := client.ActivateSnapshotKey(context.Background(), key); err != nil {
		return nil, err
	}
	if err := s.CompleteDatasetBootstrap(); err != nil {
		return nil, err
	}
	s.SetNativeSessionFinalizationSealer(client)
	return client, nil
}

// activateTestKey activates testSnapshotKey as activateSnapshotKey does.
func activateTestKey(t *testing.T, s *Store) *storageclient.Client {
	t.Helper()
	client, err := activateSnapshotKey(t, s, testSnapshotKey)
	if err != nil {
		t.Fatalf("activate snapshot key: %v", err)
	}
	return client
}

// scratchSealer seals records with testSnapshotKey through the storage client
// over a separate empty store, so tests can write sealed records into
// historical schemas that cannot activate a key.
type scratchSealer struct {
	store  *Store
	client *storageclient.Client
}

func newScratchSealer(t *testing.T) scratchSealer {
	t.Helper()
	s := setupDiskStore(t)
	return scratchSealer{store: s, client: activateTestKey(t, s)}
}

// persistSnapshot seals snapshot and creates it in target.
func (s scratchSealer) persistSnapshot(t *testing.T, target *Store, snapshot store.AgentExecutionSnapshot) {
	t.Helper()
	require.NoError(t, s.client.PersistAgentExecutionSnapshot(t.Context(), snapshot))
	sealed, err := s.store.GetSealedAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest})
	require.NoError(t, err)
	require.NoError(t, target.CreateSealedAgentExecutionSnapshot(t.Context(), *sealed))
}

// putConnectorCredential seals the credential as a first grant and stores it
// in target.
func (s scratchSealer) putConnectorCredential(t *testing.T, target *Store, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) {
	t.Helper()
	require.NoError(t, s.client.PutConnectorCredential(t.Context(), ref, credential))
	sealed, err := s.store.GetSealedConnectorCredential(t.Context(), ref.ConnectionUID)
	require.NoError(t, err)
	_, err = target.PutSealedConnectorCredential(t.Context(), storagev1.PutSealedConnectorCredentialRequest{Credential: *sealed, Now: time.Now()})
	require.NoError(t, err)
}

// stageNativeImport seals the import and stages it in target.
func (s scratchSealer) stageNativeImport(t *testing.T, target *Store, request store.NativeSessionImport) {
	t.Helper()
	receipt, err := s.client.StageNativeSessionImport(t.Context(), request)
	require.NoError(t, err)
	sealed, err := s.store.GetSealedNativeSession(t.Context(), request.Namespace, request.SessionName, "")
	require.NoError(t, err)
	_, err = target.StageSealedNativeSessionImport(t.Context(), storagev1.StageSealedNativeSessionImportRequest{Record: *sealed, Receipt: *receipt})
	require.NoError(t, err)
}
