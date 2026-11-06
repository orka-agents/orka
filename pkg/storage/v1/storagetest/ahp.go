package storagetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

func ahpEntries(from, count int) []storagev1.AHPJournalEntry {
	entries := make([]storagev1.AHPJournalEntry, count)
	for i := range entries {
		seq := int64(from + i)
		entries[i] = storagev1.AHPJournalEntry{Seq: seq, Channel: "ahp-root://", Envelope: fmt.Appendf(nil, `{"serverSeq":%d}`, seq)}
	}
	return entries
}

func runAHP(t *testing.T, newProvider StateFactory) {
	at := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)

	t.Run("tokens are unique and revoked only by their user", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		token := storagev1.AHPToken{ID: "tok-1", TokenHash: "hash-1", UserKey: "alice", Issuer: "https://issuer", Subject: "alice",
			Username: "alice@example.com", Namespace: "team", CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
		require.NoError(t, p.CreateAHPToken(ctx, token))
		require.ErrorIs(t, p.CreateAHPToken(ctx, token), storagev1.ErrConflict, "an ID is unique")
		sameHash := token
		sameHash.ID = "tok-2"
		require.ErrorIs(t, p.CreateAHPToken(ctx, sameHash), storagev1.ErrConflict, "a hash is unique")
		require.ErrorIs(t, p.CreateAHPToken(ctx, storagev1.AHPToken{ID: "tok-3", UserKey: "alice"}), storagev1.ErrValidation)

		got, err := p.GetAHPTokenByHash(ctx, "hash-1")
		require.NoError(t, err)
		require.Equal(t, token, got)
		_, err = p.GetAHPTokenByHash(ctx, "missing")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.ErrorIs(t, p.RevokeAHPToken(ctx, "bob", "tok-1", at), storagev1.ErrNotFound, "only the owner revokes")
		require.NoError(t, p.RevokeAHPToken(ctx, "alice", "tok-1", at))
		require.NoError(t, p.RevokeAHPToken(ctx, "alice", "tok-1", at.Add(time.Minute)))
		got, err = p.GetAHPTokenByHash(ctx, "hash-1")
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt)
		require.True(t, got.RevokedAt.Equal(at), "the first revocation time is kept")

		newer := storagev1.AHPToken{ID: "tok-0", TokenHash: "hash-0", UserKey: "alice", CreatedAt: at.Add(time.Hour),
			ExpiresAt: at.Add(2 * time.Hour)}
		require.NoError(t, p.CreateAHPToken(ctx, newer))
		list, err := p.ListAHPTokens(ctx, "alice")
		require.NoError(t, err)
		require.Equal(t, []string{"tok-0", "tok-1"}, []string{list[0].ID, list[1].ID}, "newest first")
		list, err = p.ListAHPTokens(ctx, "bob")
		require.NoError(t, err)
		require.Empty(t, list)
	})

	t.Run("clients stay bound to their first user", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, err := p.GetAHPClient(ctx, "client-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.NoError(t, p.PutAHPClient(ctx, storagev1.AHPClient{ClientID: "client-1", UserKey: "alice", ProtocolVersion: "0.9.0",
			Dialect: "default", UpdatedAt: at}))
		require.NoError(t, p.PutAHPClient(ctx, storagev1.AHPClient{ClientID: "client-1", UserKey: "alice", ProtocolVersion: "1.0.0",
			Dialect: "other", UpdatedAt: at.Add(time.Minute)}))
		require.ErrorIs(t, p.PutAHPClient(ctx, storagev1.AHPClient{ClientID: "client-1", UserKey: "bob", ProtocolVersion: "2.0.0",
			Dialect: "default", UpdatedAt: at.Add(2 * time.Minute)}), storagev1.ErrConflict)
		client, err := p.GetAHPClient(ctx, "client-1")
		require.NoError(t, err)
		require.Equal(t, storagev1.AHPClient{ClientID: "client-1", UserKey: "alice", ProtocolVersion: "1.0.0", Dialect: "other",
			CreatedAt: at, UpdatedAt: at.Add(time.Minute)}, client)
	})

	t.Run("the journal appends in sequence, prunes and reports gaps", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		limits := storagev1.AHPJournalLimits{MaxEntries: 5}
		require.NoError(t, p.AppendAHPJournal(ctx, "alice", 0, ahpEntries(1, 3),
			[]storagev1.AHPChannelState{{Channel: "ahp-root://", State: []byte(`{}`), FromSeq: 3}}, limits))
		require.ErrorIs(t, p.AppendAHPJournal(ctx, "alice", 0, ahpEntries(1, 1), nil, limits), storagev1.ErrConflict,
			"a stale previous sequence is refused")
		require.ErrorIs(t, p.AppendAHPJournal(ctx, "alice", 3, ahpEntries(5, 1), nil, limits), storagev1.ErrValidation,
			"entries must continue the sequence")
		require.NoError(t, p.AppendAHPJournal(ctx, "alice", 3, ahpEntries(4, 4), nil, limits))

		seq, err := p.GetAHPServerSeq(ctx, "alice")
		require.NoError(t, err)
		require.EqualValues(t, 7, seq)
		seq, err = p.GetAHPServerSeq(ctx, "bob")
		require.NoError(t, err)
		require.Zero(t, seq)

		entries, complete, err := p.ReadAHPJournal(ctx, "alice", 4)
		require.NoError(t, err)
		require.True(t, complete)
		require.Equal(t, ahpEntries(5, 3), entries)
		_, complete, err = p.ReadAHPJournal(ctx, "alice", 1)
		require.NoError(t, err)
		require.False(t, complete, "entry 2 was pruned, so a replay from 1 is incomplete")
		entries, complete, err = p.ReadAHPJournal(ctx, "alice", 7)
		require.NoError(t, err)
		require.True(t, complete)
		require.Empty(t, entries)
		_, complete, err = p.ReadAHPJournal(ctx, "alice", 9)
		require.NoError(t, err)
		require.False(t, complete, "a cursor from the future is never complete")
		_, complete, err = p.ReadAHPJournal(ctx, "alice", -1)
		require.NoError(t, err)
		require.False(t, complete, "a negative cursor is never complete")
		_, complete, err = p.ReadAHPJournal(ctx, "bob", 0)
		require.NoError(t, err)
		require.True(t, complete, "a user with no journal has nothing to replay")

		state, err := p.GetAHPChannelState(ctx, "alice", "ahp-root://")
		require.NoError(t, err)
		require.Equal(t, storagev1.AHPChannelState{Channel: "ahp-root://", State: []byte(`{}`), FromSeq: 3}, state)
		_, err = p.GetAHPChannelState(ctx, "bob", "ahp-root://")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.NoError(t, p.DeleteAHPChannelStates(ctx, "alice", []string{"ahp-root://", "ahp-session://none"}))
		_, err = p.GetAHPChannelState(ctx, "alice", "ahp-root://")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("the journal prunes by bytes", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		entries := ahpEntries(1, 4)
		size := int64(len(entries[0].Envelope))
		require.NoError(t, p.AppendAHPJournal(ctx, "alice", 0, entries, nil, storagev1.AHPJournalLimits{MaxBytes: 2 * size}))
		kept, complete, err := p.ReadAHPJournal(ctx, "alice", 2)
		require.NoError(t, err)
		require.True(t, complete)
		require.Len(t, kept, 2)
		_, complete, err = p.ReadAHPJournal(ctx, "alice", 1)
		require.NoError(t, err)
		require.False(t, complete)
	})

	t.Run("channels list by literal prefix for their user", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		channelURIs := []string{"ahp-terminal:/b", "ahp-terminal:/a", "ahp-terminalx:/c", "ahp-session:/d", "ahp_terminal:/e"}
		states := make([]storagev1.AHPChannelState, 0, len(channelURIs))
		for _, channel := range channelURIs {
			states = append(states, storagev1.AHPChannelState{Channel: channel, State: []byte(`{}`), FromSeq: 1})
		}
		require.NoError(t, p.AppendAHPJournal(ctx, "alice", 0, ahpEntries(1, 1), states, storagev1.AHPJournalLimits{}))
		channels, err := p.ListAHPChannels(ctx, "alice", "ahp-terminal:")
		require.NoError(t, err)
		require.Equal(t, []string{"ahp-terminal:/a", "ahp-terminal:/b"}, channels)
		channels, err = p.ListAHPChannels(ctx, "alice", "ahp_")
		require.NoError(t, err)
		require.Equal(t, []string{"ahp_terminal:/e"}, channels, "a wildcard character in the prefix is literal")
		other, err := p.ListAHPChannels(ctx, "bob", "ahp-terminal:")
		require.NoError(t, err)
		require.Empty(t, other)
	})

	t.Run("the session catalog is per user and keeps what it was created with", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for i, name := range []string{"ahp-session:/a", "ahp-session:/b"} {
			require.NoError(t, p.PutAHPSession(ctx, storagev1.AHPSession{UserKey: "alice", Session: name, Backend: "fake",
				Summary: []byte(`{}`), CreatedAt: at, ModifiedAt: at.Add(time.Duration(i) * time.Minute)}))
		}
		require.ErrorIs(t, p.PutAHPSession(ctx, storagev1.AHPSession{UserKey: "alice"}), storagev1.ErrValidation)
		sessions, err := p.ListAHPSessions(ctx, "alice")
		require.NoError(t, err)
		require.Equal(t, []string{"ahp-session:/b", "ahp-session:/a"}, []string{sessions[0].Session, sessions[1].Session},
			"most recently modified first")
		require.NoError(t, p.PutAHPSession(ctx, storagev1.AHPSession{UserKey: "alice", Session: "ahp-session:/a", Backend: "other",
			Summary: []byte(`{"title":"a"}`), CreatedAt: at.Add(time.Hour), ModifiedAt: at.Add(2 * time.Minute)}))
		session, err := p.GetAHPSession(ctx, "alice", "ahp-session:/a")
		require.NoError(t, err)
		require.Equal(t, storagev1.AHPSession{UserKey: "alice", Session: "ahp-session:/a", Backend: "fake",
			Summary: []byte(`{"title":"a"}`), CreatedAt: at, ModifiedAt: at.Add(2 * time.Minute)}, session)
		_, err = p.GetAHPSession(ctx, "bob", "ahp-session:/a")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.NoError(t, p.DeleteAHPSession(ctx, "alice", "ahp-session:/a"))
		require.NoError(t, p.DeleteAHPSession(ctx, "alice", "ahp-session:/missing"))
		sessions, err = p.ListAHPSessions(ctx, "alice")
		require.NoError(t, err)
		require.Len(t, sessions, 1)
	})

	t.Run("records are scoped to their user and kind and deleted by literal prefix", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		put := func(user, kind, key, value string) {
			t.Helper()
			require.NoError(t, p.PutAHPRecord(ctx, storagev1.AHPRecord{UserKey: user, Kind: kind, Key: key, Value: []byte(value), UpdatedAt: at}))
		}
		put("alice", "content", "s1/b", "two")
		put("alice", "content", "s1/a", "one")
		put("alice", "content", "s1_x", "underscore")
		put("alice", "content", "s10/a", "other session")
		put("alice", "project", "p1", "project")
		put("bob", "content", "s1/a", "bob's")
		put("alice", "content", "s1/a", "updated")

		got, err := p.GetAHPRecord(ctx, "alice", "content", "s1/a")
		require.NoError(t, err)
		require.Equal(t, storagev1.AHPRecord{UserKey: "alice", Kind: "content", Key: "s1/a", Value: []byte("updated"), UpdatedAt: at}, got)
		_, err = p.GetAHPRecord(ctx, "carol", "content", "s1/a")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		listed, err := p.ListAHPRecords(ctx, "alice", "content")
		require.NoError(t, err)
		require.Equal(t, []string{"s1/a", "s1/b", "s10/a", "s1_x"}, ahpRecordKeys(listed), "in key order")

		// Wildcards in the prefix are literal: "s1_" must not match "s10/".
		require.NoError(t, p.DeleteAHPRecords(ctx, "alice", "content", "s1_"))
		require.NoError(t, p.DeleteAHPRecords(ctx, "alice", "content", "s1/"))
		listed, err = p.ListAHPRecords(ctx, "alice", "content")
		require.NoError(t, err)
		require.Equal(t, []string{"s10/a"}, ahpRecordKeys(listed))
		projects, err := p.ListAHPRecords(ctx, "alice", "project")
		require.NoError(t, err)
		require.Len(t, projects, 1)
		bobs, err := p.GetAHPRecord(ctx, "bob", "content", "s1/a")
		require.NoError(t, err)
		require.Equal(t, "bob's", string(bobs.Value))
		require.ErrorIs(t, p.PutAHPRecord(ctx, storagev1.AHPRecord{UserKey: "alice", Kind: "content"}), storagev1.ErrValidation)

		// An exact delete leaves keys that merely share the prefix.
		put("alice", "session", "ahp-session:/one", "one")
		put("alice", "session", "ahp-session:/one-more", "more")
		require.NoError(t, p.DeleteAHPRecord(ctx, "alice", "session", "ahp-session:/one"))
		sessions, err := p.ListAHPRecords(ctx, "alice", "session")
		require.NoError(t, err)
		require.Equal(t, []string{"ahp-session:/one-more"}, ahpRecordKeys(sessions))
		require.NoError(t, p.DeleteAHPRecord(ctx, "alice", "session", "missing"))
	})
}

func ahpRecordKeys(records []storagev1.AHPRecord) []string {
	keys := make([]string, 0, len(records))
	for _, record := range records {
		keys = append(keys, record.Key)
	}
	return keys
}
