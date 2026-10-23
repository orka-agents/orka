package storagetest

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Connector custody records carry sealed bytes the provider cannot read.
// These cases check that a provider stores them exactly and decides every
// conditional command from the plain fields and the expectations Orka sends.

// connectorNow is the time the cases pass as each request's Now.
var connectorNow = sealedBaseTime

func sealedConnectorCredential(uid string, grant int64, label string) storagev1.SealedConnectorCredential {
	return storagev1.SealedConnectorCredential{
		ConnectionUID: uid, Namespace: "tenant", Name: "connection-" + uid, SubjectDigest: "subject-digest", Provider: "github",
		DataKeyNonce: sealedBytes("data key nonce " + label), DataKeyCiphertext: sealedBytes("data key " + label),
		Nonce: sealedBytes("nonce " + label), Ciphertext: sealedBytes("ciphertext " + label),
		ExpiresAt: sealedBaseTime.Add(time.Hour), GrantSequence: grant,
	}
}

func sealedConnectorConsent(uid, nonce string, sequence int64) storagev1.SealedConnectorConsent {
	return storagev1.SealedConnectorConsent{
		Nonce: nonce, ConnectionUID: uid, Namespace: "tenant", Name: "connection-" + uid, SubjectDigest: "subject-digest",
		Provider: "github", Mode: "readOnly", AuthorityDigest: "authority", RevocationDigest: "revocation",
		Scopes: []string{"repo", "read:user"}, Sequence: sequence, ExpiresAt: sealedBaseTime.Add(10 * time.Minute),
		VerifierNonce: sealedBytes("verifier nonce " + nonce), VerifierCiphertext: sealedBytes("verifier " + nonce),
	}
}

func sealedConnectorCompletion(uid, nonce string, sequence int64) storagev1.SealedConnectorCompletion {
	return storagev1.SealedConnectorCompletion{
		Nonce: nonce, ConnectionUID: uid, Namespace: "tenant", Name: "connection-" + uid, SubjectDigest: "subject-digest",
		Provider: "github", Mode: "readOnly", ConsentSequence: sequence, ExpiresAt: sealedBaseTime.Add(10 * time.Minute),
		PayloadNonce: sealedBytes("payload nonce " + nonce), Payload: sealedBytes("payload " + nonce),
	}
}

func requireSameSealedCredential(t *testing.T, want storagev1.SealedConnectorCredential, got *storagev1.SealedConnectorCredential) {
	t.Helper()
	require.NotNil(t, got)
	require.Equal(t, want.ConnectionUID, got.ConnectionUID)
	require.Equal(t, []string{want.Namespace, want.Name, want.SubjectDigest, want.Provider}, []string{got.Namespace, got.Name, got.SubjectDigest, got.Provider})
	for name, pair := range map[string][2][]byte{
		"data key nonce": {want.DataKeyNonce, got.DataKeyNonce}, "data key": {want.DataKeyCiphertext, got.DataKeyCiphertext},
		"nonce": {want.Nonce, got.Nonce}, "ciphertext": {want.Ciphertext, got.Ciphertext},
	} {
		require.True(t, bytes.Equal(pair[0], pair[1]), "%s changed", name)
	}
	require.Equal(t, want.GrantSequence, got.GrantSequence)
	require.True(t, want.ExpiresAt.Equal(got.ExpiresAt), "expiry %s, want %s", got.ExpiresAt, want.ExpiresAt)
	if !got.ExpiresAt.IsZero() {
		require.Equal(t, time.UTC, got.ExpiresAt.Location())
	}
	require.Equal(t, time.UTC, got.UpdatedAt.Location())
}

func requireSameSealedCompletion(t *testing.T, want storagev1.SealedConnectorCompletion, got *storagev1.SealedConnectorCompletion) {
	t.Helper()
	require.NotNil(t, got)
	require.Equal(t, want.Ref(), got.Ref(), "sealed payload changed")
	wantFields, gotFields := want, *got
	wantFields.PayloadNonce, wantFields.Payload, gotFields.PayloadNonce, gotFields.Payload = nil, nil, nil, nil
	require.True(t, want.ExpiresAt.Equal(got.ExpiresAt))
	require.Equal(t, time.UTC, got.ExpiresAt.Location())
	wantFields.ExpiresAt, gotFields.ExpiresAt = time.Time{}, time.Time{}
	require.Equal(t, wantFields, gotFields)
}

func readCustody(t *testing.T, provider storagev1.StateProvider, uid string) *storagev1.ConnectorCustody {
	t.Helper()
	custody, err := provider.ReadConnectorCustody(t.Context(), uid)
	require.NoError(t, err)
	require.Equal(t, uid, custody.ConnectionUID)
	return custody
}

func putGrant(t *testing.T, provider storagev1.StateProvider, credential storagev1.SealedConnectorCredential) *storagev1.SealedConnectorCredential {
	t.Helper()
	stored, err := provider.PutSealedConnectorCredential(t.Context(), storagev1.PutSealedConnectorCredentialRequest{Credential: credential, Now: connectorNow})
	require.NoError(t, err)
	return stored
}

func runSealedConnectorCustody(t *testing.T, newProvider StateFactory) {
	t.Run("put and read opaque custody", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		empty := readCustody(t, provider, "uid-a")
		require.False(t, empty.Tombstoned)
		require.Nil(t, empty.Credential)
		require.Empty(t, empty.Retired)
		require.Empty(t, empty.Completions)
		require.Zero(t, empty.LastGrantSequence)
		_, err := provider.GetSealedConnectorCredential(ctx, "uid-a")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		first := sealedConnectorCredential("uid-a", 1, "first")
		stored := putGrant(t, provider, first)
		requireSameSealedCredential(t, first, stored)
		require.Positive(t, stored.Version)
		require.True(t, connectorNow.Equal(stored.UpdatedAt))
		got, err := provider.GetSealedConnectorCredential(ctx, "uid-a")
		require.NoError(t, err)
		requireSameSealedCredential(t, first, got)
		require.Equal(t, stored.Version, got.Version)
		require.True(t, connectorNow.Equal(got.UpdatedAt))

		for _, grant := range []int64{1, 3} {
			_, err := provider.PutSealedConnectorCredential(ctx, storagev1.PutSealedConnectorCredentialRequest{
				Credential: sealedConnectorCredential("uid-a", grant, "out of order"), Now: connectorNow,
			})
			require.ErrorIs(t, err, storagev1.ErrConflict, "grant %d is not the next grant", grant)
		}
		got, err = provider.GetSealedConnectorCredential(ctx, "uid-a")
		require.NoError(t, err)
		requireSameSealedCredential(t, first, got)

		second := sealedConnectorCredential("uid-a", 2, "second")
		second.ExpiresAt = time.Time{}
		stored = putGrant(t, provider, second)
		require.Greater(t, stored.Version, got.Version)
		got, err = provider.GetSealedConnectorCredential(ctx, "uid-a")
		require.NoError(t, err)
		requireSameSealedCredential(t, second, got)
		require.True(t, got.ExpiresAt.IsZero(), "no expiry stays no expiry")
		custody := readCustody(t, provider, "uid-a")
		require.EqualValues(t, 2, custody.LastGrantSequence)
		requireSameSealedCredential(t, second, custody.Credential)
		require.Nil(t, readCustody(t, provider, "uid-b").Credential, "custody is per Connection")
	})

	t.Run("validation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		for name, mutate := range map[string]func(*storagev1.SealedConnectorCredential){
			"connection UID": func(c *storagev1.SealedConnectorCredential) { c.ConnectionUID = " " },
			"subject":        func(c *storagev1.SealedConnectorCredential) { c.SubjectDigest = "" },
			"data key":       func(c *storagev1.SealedConnectorCredential) { c.DataKeyCiphertext = nil },
			"ciphertext":     func(c *storagev1.SealedConnectorCredential) { c.Ciphertext = nil },
			"grant":          func(c *storagev1.SealedConnectorCredential) { c.GrantSequence = 0 },
		} {
			credential := sealedConnectorCredential("uid-a", 1, name)
			mutate(&credential)
			_, err := provider.PutSealedConnectorCredential(ctx, storagev1.PutSealedConnectorCredentialRequest{Credential: credential, Now: connectorNow})
			require.ErrorIs(t, err, storagev1.ErrValidation, name)
		}
		_, err := provider.PutSealedConnectorCredential(ctx, storagev1.PutSealedConnectorCredentialRequest{Credential: sealedConnectorCredential("uid-a", 1, "x")})
		require.ErrorIs(t, err, storagev1.ErrValidation, "the request time is required")
		_, err = provider.ReadConnectorCustody(ctx, "")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		require.Nil(t, readCustody(t, provider, "uid-a").Credential, "a refused write stores nothing")
	})

	t.Run("replacement is fenced by version and retires as asked", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		first := putGrant(t, provider, sealedConnectorCredential("uid-a", 1, "first"))
		refreshed := sealedConnectorCredential("uid-a", 1, "refreshed")
		refreshed.Name = "renamed"
		later := connectorNow.Add(time.Minute)
		replace := func(credential storagev1.SealedConnectorCredential, version int64, retire *storagev1.ConnectorRetirement) (*storagev1.SealedConnectorCredential, error) {
			return provider.ReplaceSealedConnectorCredential(ctx, storagev1.ReplaceSealedConnectorCredentialRequest{
				Credential: credential, ExpectedVersion: version, Retire: retire, Now: later,
			})
		}
		_, err := replace(refreshed, first.Version+1, &storagev1.ConnectorRetirement{})
		require.ErrorIs(t, err, storagev1.ErrConflict, "a stale version")
		_, err = replace(sealedConnectorCredential("uid-a", 2, "new grant"), first.Version, nil)
		require.ErrorIs(t, err, storagev1.ErrConflict, "a replacement keeps the stored grant")
		_, err = replace(sealedConnectorCredential("uid-missing", 1, "missing"), 1, nil)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		custody := readCustody(t, provider, "uid-a")
		requireSameSealedCredential(t, sealedConnectorCredential("uid-a", 1, "first"), custody.Credential)
		require.Empty(t, custody.Retired, "a refused replacement retires nothing")

		until := later.Add(time.Hour)
		stored, err := replace(refreshed, first.Version, &storagev1.ConnectorRetirement{RevocableUntil: until})
		require.NoError(t, err)
		require.Greater(t, stored.Version, first.Version)
		got, err := provider.GetSealedConnectorCredential(ctx, "uid-a")
		require.NoError(t, err)
		require.Equal(t, "connection-uid-a", got.Name, "a replacement keeps the identity fields")
		require.True(t, bytes.Equal(refreshed.Ciphertext, got.Ciphertext))
		require.True(t, later.Equal(got.UpdatedAt))
		custody = readCustody(t, provider, "uid-a")
		require.Len(t, custody.Retired, 1)
		retired := custody.Retired[0]
		require.True(t, bytes.Equal(first.Ciphertext, retired.Ciphertext), "the replaced record is retired as stored")
		require.True(t, bytes.Equal(first.DataKeyCiphertext, retired.DataKeyCiphertext))
		require.True(t, until.Equal(retired.RevocableUntil))
		require.False(t, retired.ConsentGrant)
		require.True(t, later.Equal(retired.RetiredAt))
	})

	t.Run("versions and grants never repeat after a shred", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		first := putGrant(t, provider, sealedConnectorCredential("uid-a", 1, "first"))
		require.ErrorIs(t, provider.ShredConnectorCredential(ctx, "uid-a", first.Version+1), storagev1.ErrConflict)
		require.NoError(t, provider.ShredConnectorCredential(ctx, "uid-a", first.Version))
		require.ErrorIs(t, provider.ShredConnectorCredential(ctx, "uid-a", first.Version), storagev1.ErrNotFound)
		_, err := provider.GetSealedConnectorCredential(ctx, "uid-a")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		custody := readCustody(t, provider, "uid-a")
		require.False(t, custody.Tombstoned, "a shred is not a disconnect")
		require.EqualValues(t, 1, custody.LastGrantSequence, "the grant counter outlives the record")
		second := putGrant(t, provider, sealedConnectorCredential("uid-a", 2, "second"))
		require.Greater(t, second.Version, first.Version)
		require.ErrorIs(t, provider.ShredConnectorCredential(ctx, "uid-a", first.Version), storagev1.ErrConflict,
			"a fence taken against the shredded record does not match the new one")
	})

	t.Run("retired material is pruned and bounded", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		retire := func(label string, until, at time.Time) error {
			return provider.RetireSealedConnectorCredential(ctx, storagev1.SealedRetiredConnectorCredential{
				ConnectionUID: "uid-a", DataKeyNonce: sealedBytes("dk nonce " + label), DataKeyCiphertext: sealedBytes("dk " + label),
				Nonce: sealedBytes("nonce " + label), Ciphertext: sealedBytes(label), RevocableUntil: until, RetiredAt: at,
			})
		}
		require.NoError(t, retire("access only", connectorNow.Add(time.Minute), connectorNow))
		require.Len(t, readCustody(t, provider, "uid-a").Retired, 1)
		for i := range storagev1.MaxRetiredConnectorRefreshGrants + 3 {
			require.NoError(t, retire(fmt.Sprintf("refresh %02d", i), time.Time{}, connectorNow.Add(2*time.Minute)))
		}
		retired := readCustody(t, provider, "uid-a").Retired
		require.Len(t, retired, storagev1.MaxRetiredConnectorRefreshGrants, "expired material is pruned and refresh grants are bounded")
		require.Equal(t, sealedBytes("refresh 03"), retired[0].Ciphertext, "the oldest refresh grants are dropped first")
		require.Equal(t, sealedBytes(fmt.Sprintf("refresh %02d", storagev1.MaxRetiredConnectorRefreshGrants+2)), retired[len(retired)-1].Ciphertext)
		for _, record := range retired {
			require.True(t, record.RevocableUntil.IsZero())
		}
		err := provider.RetireSealedConnectorCredential(ctx, storagev1.SealedRetiredConnectorCredential{
			ConnectionUID: "uid-a", DataKeyNonce: sealedBytes("n"), DataKeyCiphertext: sealedBytes("k"), Nonce: sealedBytes("n"),
			Ciphertext: sealedBytes("c"), ConsentGrant: true, RetiredAt: connectorNow,
		})
		require.ErrorIs(t, err, storagev1.ErrValidation, "only a commit retires a consent grant")
	})

	t.Run("a tombstone fences writes and deletion removes custody", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		first := putGrant(t, provider, sealedConnectorCredential("uid-a", 1, "first"))
		require.NoError(t, provider.RetireSealedConnectorCredential(ctx, storagev1.SealedRetiredConnectorCredential{
			ConnectionUID: "uid-a", DataKeyNonce: sealedBytes("n"), DataKeyCiphertext: sealedBytes("k"), Nonce: sealedBytes("n"),
			Ciphertext: sealedBytes("lost"), RetiredAt: connectorNow,
		}))
		require.NoError(t, provider.TombstoneConnectorCustody(ctx, "uid-a"))
		require.NoError(t, provider.TombstoneConnectorCustody(ctx, "uid-a"), "tombstoning is idempotent")
		custody := readCustody(t, provider, "uid-a")
		require.True(t, custody.Tombstoned)
		require.NotNil(t, custody.Credential, "custody stays readable for revocation")
		require.Len(t, custody.Retired, 1)

		_, err := provider.PutSealedConnectorCredential(ctx, storagev1.PutSealedConnectorCredentialRequest{
			Credential: sealedConnectorCredential("uid-a", 2, "late"), Now: connectorNow,
		})
		require.ErrorIs(t, err, storagev1.ErrConnectorCustodyTombstoned)
		_, err = provider.ReplaceSealedConnectorCredential(ctx, storagev1.ReplaceSealedConnectorCredentialRequest{
			Credential: sealedConnectorCredential("uid-a", 1, "late"), ExpectedVersion: first.Version + 1, Now: connectorNow,
		})
		require.ErrorIs(t, err, storagev1.ErrConnectorCustodyTombstoned, "the tombstone is judged before the version")
		require.ErrorIs(t, provider.RetireSealedConnectorCredential(ctx, storagev1.SealedRetiredConnectorCredential{
			ConnectionUID: "uid-a", DataKeyNonce: sealedBytes("n"), DataKeyCiphertext: sealedBytes("k"), Nonce: sealedBytes("n"),
			Ciphertext: sealedBytes("late"), RetiredAt: connectorNow,
		}), storagev1.ErrConnectorCustodyTombstoned)
		require.ErrorIs(t, provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{
			Completion: sealedConnectorCompletion("uid-a", "late", 1), Now: connectorNow,
		}), storagev1.ErrConnectorCustodyTombstoned)

		require.NoError(t, provider.DeleteConnectorCredential(ctx, "uid-a"))
		require.NoError(t, provider.DeleteConnectorCredential(ctx, "uid-a"), "deletion is idempotent")
		custody = readCustody(t, provider, "uid-a")
		require.True(t, custody.Tombstoned)
		require.Nil(t, custody.Credential)
		require.Empty(t, custody.Retired)
		putGrant(t, provider, sealedConnectorCredential("uid-b", 1, "other"))
	})
}

func runSealedConnectorConsents(t *testing.T, newProvider StateFactory) {
	t.Run("consents are numbered and replace the pending one", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		create := func(consent storagev1.SealedConnectorConsent, now time.Time) error {
			return provider.CreateSealedConnectorConsent(ctx, storagev1.CreateSealedConnectorConsentRequest{Consent: consent, Now: now})
		}
		require.NoError(t, create(sealedConnectorConsent("uid-a", "consent-1", 1), connectorNow))
		require.ErrorIs(t, create(sealedConnectorConsent("uid-a", "consent-x", 1), connectorNow), storagev1.ErrConflict)
		require.ErrorIs(t, create(sealedConnectorConsent("uid-a", "consent-x", 3), connectorNow), storagev1.ErrConflict)
		second := sealedConnectorConsent("uid-a", "consent-2", 2)
		require.NoError(t, create(second, connectorNow))
		require.NoError(t, create(sealedConnectorConsent("uid-b", "consent-b", 1), connectorNow))
		require.EqualValues(t, 2, readCustody(t, provider, "uid-a").IssuedConsentSequence)

		_, err := provider.ConsumeSealedConnectorConsent(ctx, "consent-1", connectorNow)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a new consent replaces the pending one")
		got, err := provider.ConsumeSealedConnectorConsent(ctx, "consent-2", connectorNow)
		require.NoError(t, err)
		require.True(t, second.ExpiresAt.Equal(got.ExpiresAt))
		require.Equal(t, time.UTC, got.ExpiresAt.Location())
		wantFields, gotFields := second, *got
		wantFields.ExpiresAt, gotFields.ExpiresAt = time.Time{}, time.Time{}
		require.Equal(t, wantFields, gotFields, "the consent is returned exactly")
		_, err = provider.ConsumeSealedConnectorConsent(ctx, "consent-2", connectorNow)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a consent is consumed once")
		_, err = provider.ConsumeSealedConnectorConsent(ctx, "consent-b", second.ExpiresAt.Add(time.Second))
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a consent that expired before the request is dropped")

		spaced := sealedConnectorConsent("uid-c", "consent-c", 1)
		spaced.Scopes = []string{"repo read:user"}
		require.ErrorIs(t, create(spaced, connectorNow), storagev1.ErrValidation)
	})

	t.Run("completions park against the completions Orka read", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		park := func(completion storagev1.SealedConnectorCompletion, expected []storagev1.SealedConnectorCompletionRef, replace ...string) error {
			return provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{
				Completion: completion, ExpectedCompletions: expected, Replace: replace, Now: connectorNow,
			})
		}
		first := sealedConnectorCompletion("uid-a", "completion-1", 1)
		require.NoError(t, park(first, nil))
		second := sealedConnectorCompletion("uid-a", "completion-2", 2)
		require.ErrorIs(t, park(second, nil), storagev1.ErrConflict, "the Connection's completions changed")
		stale := first.Ref()
		stale.Digest = controlDigest("another payload")
		require.ErrorIs(t, park(second, []storagev1.SealedConnectorCompletionRef{stale}, first.Nonce), storagev1.ErrConflict)
		require.ErrorIs(t, park(second, nil, first.Nonce), storagev1.ErrValidation, "a replaced completion must be expected")
		require.NoError(t, park(second, []storagev1.SealedConnectorCompletionRef{first.Ref()}, first.Nonce))
		_, err := provider.GetSealedConnectorCompletion(ctx, first.Nonce)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		got, err := provider.GetSealedConnectorCompletion(ctx, second.Nonce)
		require.NoError(t, err)
		requireSameSealedCompletion(t, second, got)
		custody := readCustody(t, provider, "uid-a")
		require.Len(t, custody.Completions, 1)
		requireSameSealedCompletion(t, second, &custody.Completions[0])
		require.NoError(t, park(sealedConnectorCompletion("uid-b", "completion-b", 1), nil), "completions are per Connection")

		consumed, err := provider.ConsumeSealedConnectorCompletion(ctx, second.Nonce)
		require.NoError(t, err)
		requireSameSealedCompletion(t, second, consumed)
		_, err = provider.ConsumeSealedConnectorCompletion(ctx, second.Nonce)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.NoError(t, provider.DeleteConnectorCompletion(ctx, second.Nonce), "deletion is idempotent")

		require.NoError(t, provider.CreateSealedConnectorConsent(ctx, storagev1.CreateSealedConnectorConsentRequest{
			Consent: sealedConnectorConsent("uid-b", "consent-b", 1), Now: connectorNow,
		}))
		require.NoError(t, provider.DeleteConnectorConsentsForConnection(ctx, "uid-b"))
		_, err = provider.GetSealedConnectorCompletion(ctx, "completion-b")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = provider.ConsumeSealedConnectorConsent(ctx, "consent-b", connectorNow)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("commit applies only what Orka read", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		first := putGrant(t, provider, sealedConnectorCredential("uid-a", 1, "first"))
		older := sealedConnectorCompletion("uid-a", "completion-older", 4)
		parked := sealedConnectorCompletion("uid-a", "completion-parked", 5)
		require.NoError(t, provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{Completion: older, Now: connectorNow}))
		require.NoError(t, provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{
			Completion: parked, ExpectedCompletions: []storagev1.SealedConnectorCompletionRef{older.Ref()}, Now: connectorNow,
		}))
		request := func() storagev1.CommitSealedConnectorCompletionRequest {
			return storagev1.CommitSealedConnectorCompletionRequest{
				Nonce: parked.Nonce, PayloadNonce: sealedBytes("committed payload nonce"), Payload: sealedBytes("committed payload"),
				Credential: sealedConnectorCredential("uid-a", 2, "second"), ExpectedCredentialVersion: first.Version,
				Retire: &storagev1.ConnectorRetirement{}, ExpectedCompletions: []storagev1.SealedConnectorCompletionRef{older.Ref(), parked.Ref()},
				Drop: []string{older.Nonce}, Now: connectorNow,
			}
		}
		for name, refusal := range map[string]struct {
			mutate func(*storagev1.CommitSealedConnectorCompletionRequest)
			err    error
		}{
			"stale version": {func(r *storagev1.CommitSealedConnectorCompletionRequest) { r.ExpectedCredentialVersion++ }, storagev1.ErrConflict},
			"no custody expected": {func(r *storagev1.CommitSealedConnectorCompletionRequest) {
				r.ExpectedCredentialVersion, r.Retire = 0, nil
			}, storagev1.ErrConflict},
			"grant not next": {func(r *storagev1.CommitSealedConnectorCompletionRequest) { r.Credential.GrantSequence = 3 }, storagev1.ErrConflict},
			"completion unseen": {func(r *storagev1.CommitSealedConnectorCompletionRequest) {
				r.ExpectedCompletions, r.Drop = r.ExpectedCompletions[1:], nil
			}, storagev1.ErrConflict},
			"completion changed": {func(r *storagev1.CommitSealedConnectorCompletionRequest) {
				r.ExpectedCompletions[1].Digest = controlDigest("changed")
			}, storagev1.ErrConflict},
			"expired at commit":    {func(r *storagev1.CommitSealedConnectorCompletionRequest) { r.Now = parked.ExpiresAt }, storagev1.ErrNotFound},
			"drops the committed":  {func(r *storagev1.CommitSealedConnectorCompletionRequest) { r.Drop = []string{parked.Nonce} }, storagev1.ErrValidation},
			"commits the unknown":  {func(r *storagev1.CommitSealedConnectorCompletionRequest) { r.Nonce = "completion-unknown" }, storagev1.ErrValidation},
			"missing payload":      {func(r *storagev1.CommitSealedConnectorCompletionRequest) { r.Payload = nil }, storagev1.ErrValidation},
			"missing request time": {func(r *storagev1.CommitSealedConnectorCompletionRequest) { r.Now = time.Time{} }, storagev1.ErrValidation},
		} {
			commit := request()
			refusal.mutate(&commit)
			_, err := provider.CommitSealedConnectorCompletion(ctx, commit)
			require.ErrorIs(t, err, refusal.err, name)
			custody := readCustody(t, provider, "uid-a")
			require.Equal(t, first.Version, custody.Credential.Version, "%s changed custody", name)
			require.Empty(t, custody.Retired, "%s retired custody", name)
			require.Len(t, custody.Completions, 2, "%s changed the completions", name)
			require.Zero(t, custody.CommittedConsentSequence, name)
		}

		stored, err := provider.CommitSealedConnectorCompletion(ctx, request())
		require.NoError(t, err)
		requireSameSealedCredential(t, sealedConnectorCredential("uid-a", 2, "second"), stored)
		require.Greater(t, stored.Version, first.Version)
		custody := readCustody(t, provider, "uid-a")
		requireSameSealedCredential(t, sealedConnectorCredential("uid-a", 2, "second"), custody.Credential)
		require.EqualValues(t, 2, custody.LastGrantSequence)
		require.EqualValues(t, 5, custody.CommittedConsentSequence)
		require.GreaterOrEqual(t, custody.IssuedConsentSequence, int64(5), "a committed consent counts as issued")
		require.Len(t, custody.Retired, 1)
		require.True(t, bytes.Equal(first.Ciphertext, custody.Retired[0].Ciphertext))
		require.True(t, custody.Retired[0].ConsentGrant, "a commit retires a consent grant")
		require.True(t, custody.Retired[0].RevocableUntil.IsZero())
		require.Len(t, custody.Completions, 1, "the dropped completion is gone")
		require.Equal(t, sealedBytes("committed payload"), custody.Completions[0].Payload)

		for _, sequence := range []int64{4, 5} {
			require.ErrorIs(t, provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{
				Completion: sealedConnectorCompletion("uid-a", fmt.Sprintf("late-%d", sequence), sequence), Now: connectorNow,
				ExpectedCompletions: []storagev1.SealedConnectorCompletionRef{custody.Completions[0].Ref()},
			}), storagev1.ErrConnectorConsentSuperseded, "consent %d is not after the committed one", sequence)
		}
		again := request()
		again.Credential.GrantSequence, again.ExpectedCredentialVersion = 3, stored.Version
		again.ExpectedCompletions, again.Drop = []storagev1.SealedConnectorCompletionRef{custody.Completions[0].Ref()}, nil
		_, err = provider.CommitSealedConnectorCompletion(ctx, again)
		require.ErrorIs(t, err, storagev1.ErrConnectorConsentSuperseded, "a committed consent is never committed again")
	})

	t.Run("commit bounds retained consent grants and honors the tombstone", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		current := putGrant(t, provider, sealedConnectorCredential("uid-a", 1, "grant 1"))
		commit := func(sequence int64) (*storagev1.SealedConnectorCredential, error) {
			completion := sealedConnectorCompletion("uid-a", fmt.Sprintf("completion-%02d", sequence), sequence)
			require.NoError(t, provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{
				Completion: completion, Now: connectorNow,
			}))
			return provider.CommitSealedConnectorCompletion(ctx, storagev1.CommitSealedConnectorCompletionRequest{
				Nonce: completion.Nonce, PayloadNonce: sealedBytes("n"), Payload: sealedBytes(fmt.Sprintf("committed %d", sequence)),
				Credential:                sealedConnectorCredential("uid-a", sequence+1, fmt.Sprintf("grant %d", sequence+1)),
				ExpectedCredentialVersion: current.Version, Retire: &storagev1.ConnectorRetirement{},
				ExpectedCompletions: []storagev1.SealedConnectorCompletionRef{completion.Ref()}, Now: connectorNow,
			})
		}
		for sequence := int64(1); sequence <= storagev1.MaxRetiredConnectorConsentGrants; sequence++ {
			var err error
			current, err = commit(sequence)
			require.NoError(t, err)
			require.NoError(t, provider.DeleteConnectorCompletion(ctx, fmt.Sprintf("completion-%02d", sequence)))
		}
		_, err := commit(storagev1.MaxRetiredConnectorConsentGrants + 1)
		require.ErrorIs(t, err, storagev1.ErrConnectorRetiredLimit)
		custody := readCustody(t, provider, "uid-a")
		require.Equal(t, current.Version, custody.Credential.Version, "a refused commit changes nothing")
		require.Len(t, custody.Retired, storagev1.MaxRetiredConnectorConsentGrants)

		other := putGrant(t, provider, sealedConnectorCredential("uid-b", 1, "other"))
		completion := sealedConnectorCompletion("uid-b", "completion-b", 1)
		require.NoError(t, provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{Completion: completion, Now: connectorNow}))
		require.NoError(t, provider.TombstoneConnectorCustody(ctx, "uid-b"))
		_, err = provider.CommitSealedConnectorCompletion(ctx, storagev1.CommitSealedConnectorCompletionRequest{
			Nonce: completion.Nonce, PayloadNonce: sealedBytes("n"), Payload: sealedBytes("committed"),
			Credential: sealedConnectorCredential("uid-b", 2, "late"), ExpectedCredentialVersion: other.Version,
			ExpectedCompletions: []storagev1.SealedConnectorCompletionRef{completion.Ref()}, Now: connectorNow,
		})
		require.ErrorIs(t, err, storagev1.ErrConnectorCustodyTombstoned)
		require.Equal(t, other.Version, readCustody(t, provider, "uid-b").Credential.Version)
	})
}

func runSealedConnectorListings(t *testing.T, newProvider StateFactory) {
	provider := newProvider(t)
	ctx := t.Context()
	for _, uid := range []string{"uid-c", "uid-a", "uid-b"} {
		putGrant(t, provider, sealedConnectorCredential(uid, 1, uid))
		require.NoError(t, provider.CreateSealedConnectorConsent(ctx, storagev1.CreateSealedConnectorConsentRequest{
			Consent: sealedConnectorConsent(uid, "consent-"+uid, 1), Now: connectorNow,
		}))
		require.NoError(t, provider.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{
			Completion: sealedConnectorCompletion(uid, "completion-"+uid, 1), Now: connectorNow,
		}))
		for i := range 2 {
			require.NoError(t, provider.RetireSealedConnectorCredential(ctx, storagev1.SealedRetiredConnectorCredential{
				ConnectionUID: uid, DataKeyNonce: sealedBytes("n"), DataKeyCiphertext: sealedBytes("k"), Nonce: sealedBytes("n"),
				Ciphertext: sealedBytes(fmt.Sprintf("%s retired %d", uid, i)), RetiredAt: connectorNow,
			}))
		}
	}
	pageAll := func(list func(cursor string) ([]string, string, error)) []string {
		var keys []string
		cursor := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 10, "listing did not finish")
			page, next, err := list(cursor)
			require.NoError(t, err)
			require.LessOrEqual(t, len(page), 2)
			keys = append(keys, page...)
			if next == "" {
				return keys
			}
			cursor = next
		}
	}
	require.Equal(t, []string{"uid-a", "uid-b", "uid-c"}, pageAll(func(cursor string) ([]string, string, error) {
		page, err := provider.ListSealedConnectorCredentials(ctx, cursor, 2)
		if err != nil {
			return nil, "", err
		}
		var keys []string
		for _, credential := range page.Credentials {
			keys = append(keys, credential.ConnectionUID)
		}
		return keys, page.NextCursor, nil
	}))
	require.Equal(t, []string{"uid-a retired 0", "uid-a retired 1", "uid-b retired 0", "uid-b retired 1", "uid-c retired 0", "uid-c retired 1"},
		pageAll(func(cursor string) ([]string, string, error) {
			page, err := provider.ListSealedRetiredConnectorCredentials(ctx, cursor, 2)
			if err != nil {
				return nil, "", err
			}
			var keys []string
			for _, retired := range page.Retired {
				keys = append(keys, string(bytes.TrimPrefix(retired.Ciphertext, sealedBytes(""))))
			}
			return keys, page.NextCursor, nil
		}))
	require.Equal(t, []string{"consent-uid-a", "consent-uid-b", "consent-uid-c"}, pageAll(func(cursor string) ([]string, string, error) {
		page, err := provider.ListSealedConnectorConsents(ctx, cursor, 2)
		if err != nil {
			return nil, "", err
		}
		var keys []string
		for _, consent := range page.Consents {
			keys = append(keys, consent.Nonce)
		}
		return keys, page.NextCursor, nil
	}))
	require.Equal(t, []string{"completion-uid-a", "completion-uid-b", "completion-uid-c"}, pageAll(func(cursor string) ([]string, string, error) {
		page, err := provider.ListSealedConnectorCompletions(ctx, cursor, 2)
		if err != nil {
			return nil, "", err
		}
		var keys []string
		for _, completion := range page.Completions {
			keys = append(keys, completion.Nonce)
		}
		return keys, page.NextCursor, nil
	}))

	_, err := provider.ListSealedConnectorCredentials(ctx, "", 0)
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = provider.ListSealedRetiredConnectorCredentials(ctx, "", storagev1.MaxSealedRecordPageSize+1)
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = provider.ListSealedConnectorConsents(ctx, "not a provider cursor", 2)
	require.ErrorIs(t, err, storagev1.ErrValidation)
	_, err = provider.ListSealedConnectorCompletions(ctx, "not a provider cursor", 2)
	require.ErrorIs(t, err, storagev1.ErrValidation)
}
