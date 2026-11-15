/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// TestConnectorCustodySealedByEarlierReleaseOpensUnchanged loads connector
// custody the embedded store sealed before connector sealing moved into the
// storage client (testdata/connector-custody.sql) and proves the client
// authenticates, opens and judges every record unchanged, then commits over
// it.
func TestConnectorCustodySealedByEarlierReleaseOpensUnchanged(t *testing.T) {
	ctx := t.Context()
	s := setupDiskStore(t)
	fixture, err := os.ReadFile("testdata/connector-custody.sql")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, string(fixture))
	require.NoError(t, err)
	// Activation authenticates every retained connector record.
	client := activateTestKey(t, s)
	ref := store.ConnectorCredentialRef{ConnectionUID: "fixture-connection-uid", Namespace: "fixture", Name: "github-fixture", SubjectDigest: "fixture-subject", Provider: "github"}
	far := time.Date(2099, 1, 2, 3, 4, 5, 600000007, time.UTC)

	held, err := client.GetConnectorCredential(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, store.ConnectorCredential{
		AccessToken: "fixture-access-3", RefreshToken: "fixture-refresh-3", TokenType: "bearer", ExpiresAt: far, Scopes: []string{"repo"},
		AuthorityDigest: "fixture-issuer", RevocationDigest: "fixture-revocation", GrantSequence: 2, UpdatedAt: held.UpdatedAt, Version: 3,
	}, held)
	retired, err := client.ListRetiredConnectorCredentials(ctx, ref)
	require.NoError(t, err)
	retiredTokens := make([]string, 0, len(retired))
	for _, credential := range retired {
		retiredTokens = append(retiredTokens, credential.AccessToken+"/"+credential.RefreshToken)
	}
	require.Equal(t, []string{"fixture-access-1/fixture-refresh-1", "fixture-access-2/fixture-refresh-2", "fixture-access-lost/fixture-refresh-lost"}, retiredTokens)

	completions, err := client.ListConnectorCompletionsForConnection(ctx, ref.ConnectionUID)
	require.NoError(t, err)
	require.Len(t, completions, 2)
	committedRecord, parked := completions[0], completions[1]
	require.Equal(t, "fixture-completion-1", committedRecord.Nonce)
	require.True(t, committedRecord.Committed)
	require.EqualValues(t, 2, committedRecord.Credential.GrantSequence)
	require.Equal(t, "fixture-access-2", committedRecord.Credential.AccessToken)
	require.Equal(t, "fixture-authority", committedRecord.ConsentAuthorityDigest)
	require.Equal(t, "fixture-completion-2", parked.Nonce)
	require.False(t, parked.Committed)
	require.EqualValues(t, 2, parked.ConsentSequence)
	require.Equal(t, "fixture-access-4", parked.Credential.AccessToken)
	require.True(t, far.Equal(parked.ExpiresAt))

	consent, err := client.ConsumeConnectorConsent(ctx, "fixture-consent-2")
	require.NoError(t, err)
	require.Equal(t, "fixture-verifier-2", consent.CodeVerifier)
	require.EqualValues(t, 2, consent.Sequence)
	require.Equal(t, []string{"read:user", "repo"}, consent.Scopes)

	_, err = client.CommitConnectorCompletion(ctx, committedRecord.Nonce, ref, committedRecord.Credential)
	require.ErrorIs(t, err, store.ErrConnectorCompletionCommitted)
	committed, err := client.CommitConnectorCompletion(ctx, parked.Nonce, ref, parked.Credential)
	require.NoError(t, err)
	require.EqualValues(t, 3, committed.GrantSequence)
	require.EqualValues(t, 4, committed.Version)
	retired, err = client.ListRetiredConnectorCredentials(ctx, ref)
	require.NoError(t, err)
	require.Len(t, retired, 4)
	require.Equal(t, "fixture-access-3", retired[3].AccessToken, "the earlier custody is retired as stored")
	kept, err := client.PeekConnectorCompletion(ctx, committedRecord.Nonce)
	require.NoError(t, err)
	require.True(t, kept.Committed, "an earlier committed record is kept for status recovery")
	now, err := client.PeekConnectorCompletion(ctx, parked.Nonce)
	require.NoError(t, err)
	require.True(t, now.Committed)
	require.EqualValues(t, 3, now.Credential.GrantSequence)
}
