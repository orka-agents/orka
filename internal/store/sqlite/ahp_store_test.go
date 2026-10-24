/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func TestAHPTokensAreScopedToTheirUser(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	token := store.AHPToken{
		ID: "tok-1", TokenHash: "hash-1", UserKey: "alice", Issuer: "https://issuer", Subject: "alice",
		Username: "alice@example.com", Namespace: "team",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(t, s.CreateAHPToken(ctx, token))
	require.Error(t, s.CreateAHPToken(ctx, token), "token hashes and ids are unique")

	got, err := s.GetAHPTokenByHash(ctx, "hash-1")
	require.NoError(t, err)
	require.Equal(t, "alice", got.UserKey)
	require.Nil(t, got.RevokedAt)
	_, err = s.GetAHPTokenByHash(ctx, "missing")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.ErrorIs(t, s.RevokeAHPToken(ctx, "bob", "tok-1", now), store.ErrNotFound, "only the owner revokes")
	require.NoError(t, s.RevokeAHPToken(ctx, "alice", "tok-1", now))
	require.NoError(t, s.RevokeAHPToken(ctx, "alice", "tok-1", now.Add(time.Minute)))
	got, err = s.GetAHPTokenByHash(ctx, "hash-1")
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt)
	require.True(t, got.RevokedAt.Equal(now), "the first revocation time is kept")

	list, err := s.ListAHPTokens(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, list, 1)
	list, err = s.ListAHPTokens(ctx, "bob")
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestAHPClientsStayBoundToTheirFirstUser(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	_, err := s.GetAHPClient(ctx, "client-1")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, s.PutAHPClient(ctx, store.AHPClient{ClientID: "client-1", UserKey: "alice", ProtocolVersion: "0.9.0", Dialect: "default", UpdatedAt: now}))
	require.NoError(t, s.PutAHPClient(ctx, store.AHPClient{ClientID: "client-1", UserKey: "alice", ProtocolVersion: "1.0.0", Dialect: "default", UpdatedAt: now}))
	require.ErrorIs(t, s.PutAHPClient(ctx, store.AHPClient{ClientID: "client-1", UserKey: "bob", ProtocolVersion: "1.0.0", Dialect: "default", UpdatedAt: now}), store.ErrConflict)
	client, err := s.GetAHPClient(ctx, "client-1")
	require.NoError(t, err)
	require.Equal(t, "alice", client.UserKey)
	require.Equal(t, "1.0.0", client.ProtocolVersion)
}

func ahpEntries(from, count int) []store.AHPJournalEntry {
	entries := make([]store.AHPJournalEntry, count)
	for i := range entries {
		seq := int64(from + i)
		entries[i] = store.AHPJournalEntry{Seq: seq, Channel: "ahp-root://", Envelope: []byte(fmt.Sprintf(`{"serverSeq":%d}`, seq))}
	}
	return entries
}

func TestAHPJournalAppendsPrunesAndReportsGaps(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	limits := store.AHPJournalLimits{MaxEntries: 5}
	require.NoError(t, s.AppendAHPJournal(ctx, "alice", 0, ahpEntries(1, 3), []store.AHPChannelState{{Channel: "ahp-root://", State: []byte(`{}`), FromSeq: 3}}, limits))
	require.ErrorIs(t, s.AppendAHPJournal(ctx, "alice", 0, ahpEntries(1, 1), nil, limits), store.ErrConflict, "a stale previous sequence is refused")
	require.Error(t, s.AppendAHPJournal(ctx, "alice", 3, ahpEntries(5, 1), nil, limits), "entries must continue the sequence")
	require.NoError(t, s.AppendAHPJournal(ctx, "alice", 3, ahpEntries(4, 4), nil, limits))

	seq, err := s.AHPServerSeq(ctx, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 7, seq)

	entries, complete, err := s.ReadAHPJournal(ctx, "alice", 4)
	require.NoError(t, err)
	require.True(t, complete)
	require.Len(t, entries, 3)
	require.EqualValues(t, 5, entries[0].Seq)

	_, complete, err = s.ReadAHPJournal(ctx, "alice", 1)
	require.NoError(t, err)
	require.False(t, complete, "entries 2 was pruned, so replay from 1 is incomplete")

	entries, complete, err = s.ReadAHPJournal(ctx, "alice", 7)
	require.NoError(t, err)
	require.True(t, complete)
	require.Empty(t, entries)

	_, complete, err = s.ReadAHPJournal(ctx, "alice", 9)
	require.NoError(t, err)
	require.False(t, complete, "a cursor from the future is never complete")

	_, complete, err = s.ReadAHPJournal(ctx, "bob", 0)
	require.NoError(t, err)
	require.True(t, complete, "a user with no journal has nothing to replay")

	state, err := s.GetAHPChannelState(ctx, "alice", "ahp-root://")
	require.NoError(t, err)
	require.EqualValues(t, 3, state.FromSeq)
	_, err = s.GetAHPChannelState(ctx, "bob", "ahp-root://")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, s.DeleteAHPChannelStates(ctx, "alice", []string{"ahp-root://"}))
	_, err = s.GetAHPChannelState(ctx, "alice", "ahp-root://")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestAHPJournalPrunesByBytes(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	entries := ahpEntries(1, 4)
	size := int64(len(entries[0].Envelope))
	require.NoError(t, s.AppendAHPJournal(ctx, "alice", 0, entries, nil, store.AHPJournalLimits{MaxBytes: 2 * size}))
	kept, complete, err := s.ReadAHPJournal(ctx, "alice", 2)
	require.NoError(t, err)
	require.True(t, complete)
	require.Len(t, kept, 2)
	_, complete, err = s.ReadAHPJournal(ctx, "alice", 1)
	require.NoError(t, err)
	require.False(t, complete)
}

func TestAHPSessionCatalogIsPerUser(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	base := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	for i, name := range []string{"ahp-session:/a", "ahp-session:/b"} {
		require.NoError(t, s.PutAHPSession(ctx, store.AHPSession{
			UserKey: "alice", Session: name, Backend: "fake", Summary: []byte(`{}`),
			CreatedAt: base, ModifiedAt: base.Add(time.Duration(i) * time.Minute),
		}))
	}
	sessions, err := s.ListAHPSessions(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	require.Equal(t, "ahp-session:/b", sessions[0].Session, "most recently modified first")
	_, err = s.GetAHPSession(ctx, "bob", "ahp-session:/a")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, s.DeleteAHPSession(ctx, "alice", "ahp-session:/a"))
	sessions, err = s.ListAHPSessions(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
}

func TestAHPRecordsAreScopedAndDeletedByLiteralPrefix(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	put := func(user, kind, key, value string) {
		t.Helper()
		require.NoError(t, s.PutAHPRecord(ctx, store.AHPRecord{UserKey: user, Kind: kind, Key: key, Value: []byte(value), UpdatedAt: now}))
	}
	put("alice", "content", "s1/a", "one")
	put("alice", "content", "s1/b", "two")
	put("alice", "content", "s1_x", "underscore")
	put("alice", "content", "s10/a", "other session")
	put("alice", "project", "p1", "project")
	put("bob", "content", "s1/a", "bob's")
	put("alice", "content", "s1/a", "updated")

	got, err := s.GetAHPRecord(ctx, "alice", "content", "s1/a")
	require.NoError(t, err)
	require.Equal(t, "updated", string(got.Value))
	_, err = s.GetAHPRecord(ctx, "carol", "content", "s1/a")
	require.ErrorIs(t, err, store.ErrNotFound)

	// LIKE wildcards in the prefix are literal: "s1_" must not match "s10/".
	require.NoError(t, s.DeleteAHPRecords(ctx, "alice", "content", "s1_"))
	require.NoError(t, s.DeleteAHPRecords(ctx, "alice", "content", "s1/"))
	listed, err := s.ListAHPRecords(ctx, "alice", "content")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "s10/a", listed[0].Key)
	projects, err := s.ListAHPRecords(ctx, "alice", "project")
	require.NoError(t, err)
	require.Len(t, projects, 1)
	bobs, err := s.GetAHPRecord(ctx, "bob", "content", "s1/a")
	require.NoError(t, err)
	require.Equal(t, "bob's", string(bobs.Value))
	require.Error(t, s.PutAHPRecord(ctx, store.AHPRecord{UserKey: "alice", Kind: "content"}))

	// An exact delete leaves keys that merely share the prefix.
	put("alice", "session", "ahp-session:/one", "one")
	put("alice", "session", "ahp-session:/one-more", "more")
	require.NoError(t, s.DeleteAHPRecord(ctx, "alice", "session", "ahp-session:/one"))
	sessions, err := s.ListAHPRecords(ctx, "alice", "session")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "ahp-session:/one-more", sessions[0].Key)
	require.NoError(t, s.DeleteAHPRecord(ctx, "alice", "session", "missing"))
}

func TestAHPChannelsListByPrefix(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	limits := store.AHPJournalLimits{MaxEntries: 100, MaxBytes: 1 << 20}
	channelURIs := []string{"ahp-terminal:/a", "ahp-terminal:/b", "ahp-terminalx:/c", "ahp-session:/d"}
	states := make([]store.AHPChannelState, 0, len(channelURIs))
	for _, channel := range channelURIs {
		states = append(states, store.AHPChannelState{Channel: channel, State: []byte(`{}`), FromSeq: 1})
	}
	require.NoError(t, s.AppendAHPJournal(ctx, "alice", 0, ahpEntries(1, 1), states, limits))
	channels, err := s.ListAHPChannels(ctx, "alice", "ahp-terminal:")
	require.NoError(t, err)
	require.Equal(t, []string{"ahp-terminal:/a", "ahp-terminal:/b"}, channels)
	other, err := s.ListAHPChannels(ctx, "bob", "ahp-terminal:")
	require.NoError(t, err)
	require.Empty(t, other)
}
