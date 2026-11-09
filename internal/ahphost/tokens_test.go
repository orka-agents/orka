/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store/sqlite"
)

func newTokenService(t *testing.T) *TokenService {
	t.Helper()
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	return &TokenService{Store: s}
}

func TestMintStoresOnlyAHash(t *testing.T) {
	tokens := newTokenService(t)
	ctx := context.Background()
	minted, err := tokens.Mint(ctx, MintRequest{
		Issuer: "https://issuer.example", Subject: "alice", Namespace: "team", TTL: 24 * time.Hour,
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(minted.Token, TokenPrefix))
	require.WithinDuration(t, time.Now().Add(24*time.Hour), minted.ExpiresAt, time.Minute)

	record, err := tokens.Store.GetAHPTokenByHash(ctx, hashToken(minted.Token))
	require.NoError(t, err)
	require.NotEqual(t, minted.Token, record.TokenHash)

	principal, err := tokens.Verify(ctx, minted.Token)
	require.NoError(t, err)
	require.Equal(t, UserKey("https://issuer.example", "alice"), principal.UserKey)
	require.Equal(t, "team", principal.Namespace)
}

func TestMintRejectsBadRequests(t *testing.T) {
	tokens := newTokenService(t)
	ctx := context.Background()
	for name, req := range map[string]MintRequest{
		"no subject":        {Issuer: "https://issuer.example"},
		"ttl too long":      {Issuer: "i", Subject: "s", TTL: DefaultMaxTokenTTL + time.Hour},
		"ttl too short":     {Issuer: "i", Subject: "s", TTL: time.Second},
		"description limit": {Issuer: "i", Subject: "s", Description: strings.Repeat("x", maxDescriptionLength+1)},
		"control character": {Issuer: "i", Subject: "s", Description: "laptop\x1b[2J"},
	} {
		_, err := tokens.Mint(ctx, req)
		require.ErrorIs(t, err, ErrInvalidMintRequest, name)
	}
}

// A maximum below the default lifetime still lets a token be minted without
// one, for that maximum; asking for more is still refused.
func TestMintDefaultsToAMaximumBelowTheDefault(t *testing.T) {
	tokens := newTokenService(t)
	tokens.MaxTTL = 24 * time.Hour
	ctx := context.Background()
	minted, err := tokens.Mint(ctx, MintRequest{Issuer: "https://issuer.example", Subject: "alice", Namespace: "team"})
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), minted.ExpiresAt, time.Minute)
	_, err = tokens.Mint(ctx, MintRequest{Issuer: "https://issuer.example", Subject: "alice", Namespace: "team", TTL: 48 * time.Hour})
	require.ErrorIs(t, err, ErrInvalidMintRequest)
}

func TestMintTrimsTheDescription(t *testing.T) {
	tokens := newTokenService(t)
	ctx := context.Background()
	minted, err := tokens.Mint(ctx, MintRequest{Issuer: "i", Subject: "alice", Description: "  work laptop \n"})
	require.NoError(t, err)
	require.Equal(t, "work laptop", minted.Description)
	list, err := tokens.List(ctx, "i", "alice")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "work laptop", list[0].Description)
	require.WithinDuration(t, time.Now().Add(DefaultTokenTTL), list[0].ExpiresAt, time.Minute)
}

func TestVerifyRefusesOwnersAdmitRefuses(t *testing.T) {
	tokens := newTokenService(t)
	ctx := context.Background()
	minted, err := tokens.Mint(ctx, MintRequest{Issuer: "i", Subject: "alice", TTL: time.Hour})
	require.NoError(t, err)
	principal, err := tokens.Verify(ctx, minted.Token)
	require.NoError(t, err)

	admitted := map[string]bool{"alice": true}
	tokens.Admit = func(p Principal) error {
		if !admitted[p.Subject] {
			return errors.New("no longer admitted")
		}
		return nil
	}
	_, err = tokens.Verify(ctx, minted.Token)
	require.NoError(t, err)
	require.NoError(t, tokens.Recheck(ctx, principal))

	delete(admitted, "alice")
	_, err = tokens.Verify(ctx, minted.Token)
	require.ErrorIs(t, err, ErrInvalidToken, "an owner the sign-in policy no longer admits cannot connect")
	require.ErrorIs(t, tokens.Recheck(ctx, principal), ErrInvalidToken, "and open connections close on the next recheck")
}

func TestVerifyRefusesExpiredRevokedAndForeignTokens(t *testing.T) {
	tokens := newTokenService(t)
	ctx := context.Background()
	minted, err := tokens.Mint(ctx, MintRequest{Issuer: "i", Subject: "alice", TTL: time.Hour})
	require.NoError(t, err)
	_, err = tokens.Verify(ctx, minted.Token+"x")
	require.ErrorIs(t, err, ErrInvalidToken)
	_, err = tokens.Verify(ctx, "Bearer "+minted.Token)
	require.ErrorIs(t, err, ErrInvalidToken)

	tokens.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, err = tokens.Verify(ctx, minted.Token)
	require.ErrorIs(t, err, ErrInvalidToken, "expired")
	tokens.Now = nil

	require.Error(t, tokens.Revoke(ctx, "i", "bob", minted.ID), "only the owner revokes")
	var closed string
	tokens.OnRevoke = func(id string) { closed = id }
	require.NoError(t, tokens.Revoke(ctx, "i", "alice", minted.ID))
	require.Equal(t, minted.ID, closed)
	_, err = tokens.Verify(ctx, minted.Token)
	require.ErrorIs(t, err, ErrInvalidToken, "revoked")

	list, err := tokens.List(ctx, "i", "alice")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].RevokedAt)
}

func TestVersionHelpers(t *testing.T) {
	require.Equal(t, -1, compareVersions("0.9.0", "1.0.0"))
	require.Equal(t, 0, compareVersions("0.10.0", "0.10.0"))
	require.Equal(t, 1, compareVersions("0.10.0", "0.9.0"))
	require.Equal(t, 1, compareVersions("bad", "1.0.0"), "a malformed version is never treated as known")
	require.Equal(t, 1, compareVersions("01.0.0", "1.0.0"))

	require.True(t, actionKnownTo(ahptypes.ActionTypeChatActivityChanged, "0.9.0"))
	require.False(t, actionKnownTo(ahptypes.ActionTypeChatCanvasesChanged, "0.9.0"))
	require.True(t, actionKnownTo(ahptypes.ActionTypeChatCanvasesChanged, "1.0.0"))
	require.False(t, actionKnownTo("chat/notAnAction", "1.0.0"))
	require.True(t, isClientDispatchable(ahptypes.ActionTypeChatTurnStarted))
	require.False(t, isClientDispatchable(ahptypes.ActionTypeChatDelta))
	require.True(t, notificationKnownTo(methodSessionAdded, "0.9.0"))
	require.False(t, notificationKnownTo("root/unknown", "1.0.0"))
}
