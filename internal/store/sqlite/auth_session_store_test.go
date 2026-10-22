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

func testAuthSession(id, hash string, now time.Time) store.AuthSession {
	return store.AuthSession{
		ID: id, TokenHash: hash, Issuer: "https://github.com", Subject: "1001", Username: "alice",
		Namespace: "team", CreatedAt: now, ExpiresAt: now.Add(12 * time.Hour),
	}
}

func TestAuthSessionsAreStoredByHash(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	session := testAuthSession("sess-1", "hash-1", now)
	require.NoError(t, s.CreateAuthSession(ctx, session))
	require.Error(t, s.CreateAuthSession(ctx, session), "session ids and hashes are unique")

	got, err := s.GetAuthSessionByHash(ctx, "hash-1")
	require.NoError(t, err)
	require.Equal(t, "sess-1", got.ID)
	require.Equal(t, "https://github.com", got.Issuer)
	require.Equal(t, "1001", got.Subject)
	require.Equal(t, "alice", got.Username)
	require.Equal(t, "team", got.Namespace)
	require.True(t, got.ExpiresAt.Equal(now.Add(12*time.Hour)))
	require.Nil(t, got.RevokedAt)
	_, err = s.GetAuthSessionByHash(ctx, "missing")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Revoking twice keeps the first time.
	require.NoError(t, s.RevokeAuthSession(ctx, "sess-1", now))
	require.NoError(t, s.RevokeAuthSession(ctx, "sess-1", now.Add(time.Minute)))
	got, err = s.GetAuthSessionByHash(ctx, "hash-1")
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt)
	require.True(t, got.RevokedAt.Equal(now))
	require.ErrorIs(t, s.RevokeAuthSession(ctx, "missing", now), store.ErrNotFound)
}

func TestAuthSessionRenewsOnceAndOnlyWhileLive(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	require.NoError(t, s.CreateAuthSession(ctx, testAuthSession("sess-1", "hash-1", now)))

	later := now.Add(11 * time.Hour)
	require.NoError(t, s.RenewAuthSession(ctx, "sess-1", testAuthSession("sess-2", "hash-2", later), later))
	old, err := s.GetAuthSessionByHash(ctx, "hash-1")
	require.NoError(t, err)
	require.NotNil(t, old.RevokedAt, "renewing revokes the old session")
	next, err := s.GetAuthSessionByHash(ctx, "hash-2")
	require.NoError(t, err)
	require.Nil(t, next.RevokedAt)

	// A revoked session renews no more, and its successor is not stored.
	err = s.RenewAuthSession(ctx, "sess-1", testAuthSession("sess-3", "hash-3", later), later)
	require.ErrorIs(t, err, store.ErrConflict)
	_, err = s.GetAuthSessionByHash(ctx, "hash-3")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Nor does an expired one.
	expired := later.Add(13 * time.Hour)
	err = s.RenewAuthSession(ctx, "sess-2", testAuthSession("sess-4", "hash-4", expired), expired)
	require.ErrorIs(t, err, store.ErrConflict)
	_, err = s.GetAuthSessionByHash(ctx, "hash-4")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Nor an unknown one.
	require.ErrorIs(t, s.RenewAuthSession(ctx, "missing", testAuthSession("sess-5", "hash-5", later), later), store.ErrConflict)
}

// The table stays bounded: ended sessions are dropped by the next write, and
// a person keeps only their most recently issued sessions.
func TestAuthSessionsAreBounded(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	for i := range maxAuthSessionsPerSubject + 2 {
		at := now.Add(time.Duration(i) * time.Second)
		require.NoError(t, s.CreateAuthSession(ctx, testAuthSession(fmt.Sprintf("sess-%d", i), fmt.Sprintf("hash-%d", i), at)))
	}
	for i := range maxAuthSessionsPerSubject + 2 {
		_, err := s.GetAuthSessionByHash(ctx, fmt.Sprintf("hash-%d", i))
		if i < 2 {
			require.ErrorIs(t, err, store.ErrNotFound, "the oldest sessions beyond the bound end")
		} else {
			require.NoError(t, err)
		}
	}
	other := testAuthSession("bob-1", "bob-hash-1", now)
	other.Subject = "1002"
	require.NoError(t, s.CreateAuthSession(ctx, other))
	_, err := s.GetAuthSessionByHash(ctx, "hash-11")
	require.NoError(t, err, "another person's sign-in ends none of alice's sessions")

	require.NoError(t, s.RevokeAuthSession(ctx, "sess-2", now))
	later := now.Add(13 * time.Hour)
	require.NoError(t, s.CreateAuthSession(ctx, testAuthSession("sess-new", "hash-new", later)))
	var rows int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions`).Scan(&rows))
	require.Equal(t, 1, rows, "revoked and expired sessions are dropped")
}

// A successor whose hash is taken fails the renewal as a whole: the old
// session stays live.
func TestAuthSessionRenewalIsAtomic(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	require.NoError(t, s.CreateAuthSession(ctx, testAuthSession("sess-1", "hash-1", now)))
	require.NoError(t, s.CreateAuthSession(ctx, testAuthSession("sess-2", "hash-2", now)))
	require.Error(t, s.RenewAuthSession(ctx, "sess-1", testAuthSession("sess-3", "hash-2", now), now))
	old, err := s.GetAuthSessionByHash(ctx, "hash-1")
	require.NoError(t, err)
	require.Nil(t, old.RevokedAt, "a failed renewal leaves the old session live")
}

func TestAuthSessionsAreValidated(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*store.AuthSession){
		"no id":      func(s *store.AuthSession) { s.ID = "" },
		"no hash":    func(s *store.AuthSession) { s.TokenHash = " " },
		"no issuer":  func(s *store.AuthSession) { s.Issuer = "" },
		"no subject": func(s *store.AuthSession) { s.Subject = "" },
	} {
		t.Run(name, func(t *testing.T) {
			session := testAuthSession("sess-1", "hash-1", now)
			mutate(&session)
			require.ErrorIs(t, s.CreateAuthSession(ctx, session), store.ErrValidation)
			require.ErrorIs(t, s.RenewAuthSession(ctx, "sess-0", session, now), store.ErrValidation)
		})
	}
}
