package storagetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

func testAuthSession(id, hash string, now time.Time) storagev1.AuthSession {
	return storagev1.AuthSession{
		ID: id, TokenHash: hash, Issuer: "https://github.com", Subject: "1001", Username: "alice",
		Namespace: "team", CreatedAt: now, ExpiresAt: now.Add(12 * time.Hour),
	}
}

func runAuthSessions(t *testing.T, newProvider StateFactory) {
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)

	t.Run("sessions are stored by hash and revoked once", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		session := testAuthSession("sess-1", "hash-1", now)
		require.NoError(t, p.CreateAuthSession(ctx, session))
		require.ErrorIs(t, p.CreateAuthSession(ctx, session), storagev1.ErrConflict, "an ID is unique")
		sameHash := testAuthSession("sess-other", "hash-1", now)
		require.ErrorIs(t, p.CreateAuthSession(ctx, sameHash), storagev1.ErrConflict, "a hash is unique")

		got, err := p.GetAuthSessionByHash(ctx, "hash-1")
		require.NoError(t, err)
		require.Equal(t, "sess-1", got.ID)
		require.Equal(t, "https://github.com", got.Issuer)
		require.Equal(t, "1001", got.Subject)
		require.Equal(t, "alice", got.Username)
		require.Equal(t, "team", got.Namespace)
		require.True(t, got.CreatedAt.Equal(now))
		require.True(t, got.ExpiresAt.Equal(now.Add(12*time.Hour)))
		require.Nil(t, got.RevokedAt)
		_, err = p.GetAuthSessionByHash(ctx, "missing")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, p.RevokeAuthSession(ctx, "sess-1", now))
		require.NoError(t, p.RevokeAuthSession(ctx, "sess-1", now.Add(time.Minute)))
		got, err = p.GetAuthSessionByHash(ctx, "hash-1")
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt)
		require.True(t, got.RevokedAt.Equal(now), "the first revocation time is kept")
		require.ErrorIs(t, p.RevokeAuthSession(ctx, "missing", now), storagev1.ErrNotFound)
	})

	t.Run("a session renews once and only while live", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.NoError(t, p.CreateAuthSession(ctx, testAuthSession("sess-1", "hash-1", now)))

		later := now.Add(11 * time.Hour)
		require.NoError(t, p.RenewAuthSession(ctx, "sess-1", testAuthSession("sess-2", "hash-2", later), later))
		old, err := p.GetAuthSessionByHash(ctx, "hash-1")
		require.NoError(t, err)
		require.NotNil(t, old.RevokedAt, "renewing revokes the old session")
		next, err := p.GetAuthSessionByHash(ctx, "hash-2")
		require.NoError(t, err)
		require.Nil(t, next.RevokedAt)

		// A revoked session renews no more, and its successor is not stored.
		err = p.RenewAuthSession(ctx, "sess-1", testAuthSession("sess-3", "hash-3", later), later)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		_, err = p.GetAuthSessionByHash(ctx, "hash-3")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		// Nor does an expired one.
		expired := later.Add(13 * time.Hour)
		err = p.RenewAuthSession(ctx, "sess-2", testAuthSession("sess-4", "hash-4", expired), expired)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		_, err = p.GetAuthSessionByHash(ctx, "hash-4")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		// Nor an unknown one.
		require.ErrorIs(t, p.RenewAuthSession(ctx, "missing", testAuthSession("sess-5", "hash-5", later), later),
			storagev1.ErrConflict)
	})

	t.Run("a renewal whose successor is refused changes nothing", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.NoError(t, p.CreateAuthSession(ctx, testAuthSession("sess-1", "hash-1", now)))
		require.NoError(t, p.CreateAuthSession(ctx, testAuthSession("sess-2", "hash-2", now)))
		require.ErrorIs(t, p.RenewAuthSession(ctx, "sess-1", testAuthSession("sess-3", "hash-2", now), now), storagev1.ErrConflict)
		old, err := p.GetAuthSessionByHash(ctx, "hash-1")
		require.NoError(t, err)
		require.Nil(t, old.RevokedAt, "a failed renewal leaves the old session live")
	})

	t.Run("an ended session's ID and hash stay taken while it is stored", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.NoError(t, p.CreateAuthSession(ctx, testAuthSession("ended", "hash-ended", now)))
		require.NoError(t, p.CreateAuthSession(ctx, testAuthSession("live", "hash-live", now)))
		// Revoking prunes nothing, so the ended session is still stored.
		require.NoError(t, p.RevokeAuthSession(ctx, "ended", now))
		later := now.Add(time.Minute)

		require.ErrorIs(t, p.CreateAuthSession(ctx, testAuthSession("other", "hash-ended", later)), storagev1.ErrConflict,
			"reusing an ended session's hash would make its token valid again")
		require.ErrorIs(t, p.CreateAuthSession(ctx, testAuthSession("ended", "hash-other", later)), storagev1.ErrConflict)
		require.ErrorIs(t, p.RenewAuthSession(ctx, "live", testAuthSession("next", "hash-ended", later), later),
			storagev1.ErrConflict)

		ended, err := p.GetAuthSessionByHash(ctx, "hash-ended")
		require.NoError(t, err, "a refused write prunes nothing")
		require.NotNil(t, ended.RevokedAt, "the ended session stays revoked")
		live, err := p.GetAuthSessionByHash(ctx, "hash-live")
		require.NoError(t, err)
		require.Nil(t, live.RevokedAt, "a refused renewal leaves its session live")
	})

	t.Run("sessions stay bounded", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for i := range storagev1.MaxAuthSessionsPerSubject + 2 {
			at := now.Add(time.Duration(i) * time.Second)
			require.NoError(t, p.CreateAuthSession(ctx, testAuthSession(fmt.Sprintf("sess-%d", i), fmt.Sprintf("hash-%d", i), at)))
		}
		for i := range storagev1.MaxAuthSessionsPerSubject + 2 {
			_, err := p.GetAuthSessionByHash(ctx, fmt.Sprintf("hash-%d", i))
			if i < 2 {
				require.ErrorIs(t, err, storagev1.ErrNotFound, "the oldest sessions beyond the bound end")
			} else {
				require.NoError(t, err)
			}
		}
		other := testAuthSession("bob-1", "bob-hash-1", now)
		other.Subject = "1002"
		require.NoError(t, p.CreateAuthSession(ctx, other))
		_, err := p.GetAuthSessionByHash(ctx, "hash-11")
		require.NoError(t, err, "another person's sign-in ends none of alice's sessions")

		// The next write drops every session that ended: revoked or expired.
		require.NoError(t, p.RevokeAuthSession(ctx, "sess-2", now))
		later := now.Add(13 * time.Hour)
		require.NoError(t, p.CreateAuthSession(ctx, testAuthSession("sess-new", "hash-new", later)))
		for _, hash := range []string{"hash-2", "hash-3", "hash-11", "bob-hash-1"} {
			_, err := p.GetAuthSessionByHash(ctx, hash)
			require.ErrorIs(t, err, storagev1.ErrNotFound, hash)
		}
		_, err = p.GetAuthSessionByHash(ctx, "hash-new")
		require.NoError(t, err)
	})

	t.Run("sessions are validated", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for name, mutate := range map[string]func(*storagev1.AuthSession){
			"no id":      func(s *storagev1.AuthSession) { s.ID = "" },
			"no hash":    func(s *storagev1.AuthSession) { s.TokenHash = " " },
			"no issuer":  func(s *storagev1.AuthSession) { s.Issuer = "" },
			"no subject": func(s *storagev1.AuthSession) { s.Subject = "" },
		} {
			session := testAuthSession("sess-1", "hash-1", now)
			mutate(&session)
			require.ErrorIs(t, p.CreateAuthSession(ctx, session), storagev1.ErrValidation, name)
			require.ErrorIs(t, p.RenewAuthSession(ctx, "sess-0", session, now), storagev1.ErrValidation, name)
		}
	})
}
