package controller

import (
	"context"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type sealedTestStore struct {
	*sqlite.Store
	client *storageclient.Client
}

func sealedStore(t storageclienttest.TB, s *sqlite.Store) *sealedTestStore {
	t.Helper()
	return &sealedTestStore{Store: s, client: storageclienttest.Client(t, s)}
}

func newSealedTestStore(s *sqlite.Store) (*sealedTestStore, error) {
	client, err := storageclienttest.New(s)
	if err != nil {
		return nil, err
	}
	return &sealedTestStore{Store: s, client: client}, nil
}

func (s *sealedTestStore) PersistAgentExecutionSnapshot(ctx context.Context, snapshot store.AgentExecutionSnapshot) error {
	return s.client.PersistAgentExecutionSnapshot(ctx, snapshot)
}

func (s *sealedTestStore) GetAgentExecutionSnapshot(ctx context.Context, key store.AgentExecutionSnapshotKey) (*store.AgentExecutionSnapshot, error) {
	return s.client.GetAgentExecutionSnapshot(ctx, key)
}

func (s *sealedTestStore) GetNativeSession(ctx context.Context, namespace, name, uid string) (*store.NativeSessionRecord, error) {
	return s.client.GetNativeSession(ctx, namespace, name, uid)
}

func (s *sealedTestStore) SaveNativeSession(ctx context.Context, record store.NativeSessionRecord) error {
	return s.client.SaveNativeSession(ctx, record)
}

func (s *sealedTestStore) StageNativeSessionImport(ctx context.Context, request store.NativeSessionImport) (*store.NativeSessionImportReceipt, error) {
	return s.client.StageNativeSessionImport(ctx, request)
}

func (s *sealedTestStore) CommitSessionTurnFinalization(ctx context.Context, request store.CommitSessionTurnFinalizationRequest) (*store.SessionTurn, error) {
	return s.client.CommitSessionTurnFinalization(ctx, request)
}
