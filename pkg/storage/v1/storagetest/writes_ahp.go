package storagetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The Agent Host Protocol write cases.

const (
	catalogAHPUser    = "ahp-user-a"
	catalogAHPChannel = "ahp-session://catalog/one"
	catalogAHPKind    = "project"
)

var catalogAHPAt = time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)

func catalogAHPToken() storagev1.AHPToken {
	return storagev1.AHPToken{ID: "token-1", TokenHash: "hash-1", UserKey: catalogAHPUser, Issuer: "https://issuer.example",
		Subject: "alice", Username: "alice", Namespace: workflowNamespace, Description: "laptop", CreatedAt: catalogAHPAt,
		ExpiresAt: catalogAHPAt.Add(time.Hour)}
}

func catalogAHPClient(dialect string) storagev1.AHPClient {
	return storagev1.AHPClient{ClientID: "client-1", UserKey: catalogAHPUser, ProtocolVersion: "0.9.0", Dialect: dialect,
		UpdatedAt: catalogAHPAt}
}

func catalogAHPSession(summary string) storagev1.AHPSession {
	return storagev1.AHPSession{UserKey: catalogAHPUser, Session: catalogAHPChannel, Backend: "orka", Summary: []byte(summary),
		CreatedAt: catalogAHPAt, ModifiedAt: catalogAHPAt}
}

func catalogAHPRecord(key, value string) storagev1.AHPRecord {
	return storagev1.AHPRecord{UserKey: catalogAHPUser, Kind: catalogAHPKind, Key: key, Value: []byte(value), UpdatedAt: catalogAHPAt}
}

func appendCatalogAHPJournal(t *testing.T, p storagev1.StateProvider, previous int64, state string) {
	t.Helper()
	require.NoError(t, p.AppendAHPJournal(t.Context(), catalogAHPUser, previous,
		[]storagev1.AHPJournalEntry{{Seq: previous + 1, Channel: catalogAHPChannel, Envelope: []byte(state)}},
		[]storagev1.AHPChannelState{{Channel: catalogAHPChannel, State: []byte(state), FromSeq: previous + 1}},
		storagev1.AHPJournalLimits{}))
}

func observeAHPTokens(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	tokens, err := p.ListAHPTokens(t.Context(), catalogAHPUser)
	require.NoError(t, err)
	return tokens
}

func observeAHPClient(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	client, err := p.GetAHPClient(t.Context(), "client-1")
	return observed(t, client, err)
}

func observeAHPJournal(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	seq, err := p.GetAHPServerSeq(t.Context(), catalogAHPUser)
	require.NoError(t, err)
	entries, complete, err := p.ReadAHPJournal(t.Context(), catalogAHPUser, 0)
	require.NoError(t, err)
	channels, err := p.ListAHPChannels(t.Context(), catalogAHPUser, "")
	require.NoError(t, err)
	state, err := p.GetAHPChannelState(t.Context(), catalogAHPUser, catalogAHPChannel)
	return []any{seq, entries, complete, channels, observed(t, state, err)}
}

func observeAHPSessions(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	sessions, err := p.ListAHPSessions(t.Context(), catalogAHPUser)
	require.NoError(t, err)
	return sessions
}

func observeAHPRecords(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	records, err := p.ListAHPRecords(t.Context(), catalogAHPUser, catalogAHPKind)
	require.NoError(t, err)
	return records
}

func init() {
	type ahpCase = writeCase[storagev1.StateProvider]
	putRecords := func(t *testing.T, p storagev1.StateProvider) {
		for _, key := range []string{"a/1", "a/2", "b/1"} {
			require.NoError(t, p.PutAHPRecord(t.Context(), catalogAHPRecord(key, "first")))
		}
	}
	stateWriteCases = append(stateWriteCases,
		ahpCase{
			method:  "CreateAHPToken",
			args:    fixed(catalogAHPToken()),
			observe: observeAHPTokens,
		},
		ahpCase{
			method: "RevokeAHPToken",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateAHPToken(t.Context(), catalogAHPToken()))
			},
			args:    fixed(catalogAHPUser, "token-1", catalogAHPAt.Add(time.Minute)),
			observe: observeAHPTokens,
		},
		ahpCase{
			method: "PutAHPClient",
			args:   fixed(catalogAHPClient("default")),
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.PutAHPClient(t.Context(), catalogAHPClient("later")))
			},
			observe: observeAHPClient,
		},
		ahpCase{
			method: "AppendAHPJournal",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				appendCatalogAHPJournal(t, p, 0, "first")
			},
			args: rebuiltArgs(func() []any {
				return []any{catalogAHPUser, int64(1),
					[]storagev1.AHPJournalEntry{{Seq: 2, Channel: catalogAHPChannel, Envelope: []byte("second")}},
					[]storagev1.AHPChannelState{{Channel: catalogAHPChannel, State: []byte("second"), FromSeq: 2}},
					storagev1.AHPJournalLimits{MaxEntries: 1}}
			}),
			observe: observeAHPJournal,
		},
		ahpCase{
			method: "DeleteAHPChannelStates",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				appendCatalogAHPJournal(t, p, 0, "first")
			},
			args: rebuiltArgs(func() []any { return []any{catalogAHPUser, []string{catalogAHPChannel, "ahp-session://catalog/none"}} }),
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				appendCatalogAHPJournal(t, p, 1, "again")
			},
			observe: observeAHPJournal,
		},
		ahpCase{
			method: "PutAHPSession",
			args:   fixed(catalogAHPSession("first")),
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.PutAHPSession(t.Context(), catalogAHPSession("later")))
			},
			observe: observeAHPSessions,
		},
		ahpCase{
			method: "DeleteAHPSession",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.PutAHPSession(t.Context(), catalogAHPSession("first")))
			},
			args: fixed(catalogAHPUser, catalogAHPChannel),
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.PutAHPSession(t.Context(), catalogAHPSession("again")))
			},
			observe: observeAHPSessions,
		},
		ahpCase{
			method: "PutAHPRecord",
			args:   fixed(catalogAHPRecord("a/1", "first")),
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.PutAHPRecord(t.Context(), catalogAHPRecord("a/1", "later")))
			},
			observe: observeAHPRecords,
		},
		ahpCase{
			method: "DeleteAHPRecords",
			setup:  putRecords,
			args:   fixed(catalogAHPUser, catalogAHPKind, "a/"),
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.PutAHPRecord(t.Context(), catalogAHPRecord("a/1", "again")))
			},
			observe: observeAHPRecords,
		},
		ahpCase{
			method: "DeleteAHPRecord",
			setup:  putRecords,
			args:   fixed(catalogAHPUser, catalogAHPKind, "a/1"),
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.PutAHPRecord(t.Context(), catalogAHPRecord("a/1", "again")))
			},
			observe: observeAHPRecords,
		},
	)
}
