package storageclient_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The recorder captures every connector provider call with its arguments.

func (r *recordingStore) ReadConnectorCustody(ctx context.Context, connectionUID string) (*storagev1.ConnectorCustody, error) {
	r.record("ReadConnectorCustody", connectionUID)
	return r.Store.ReadConnectorCustody(ctx, connectionUID)
}

func (r *recordingStore) GetSealedConnectorCredential(ctx context.Context, connectionUID string) (*storagev1.SealedConnectorCredential, error) {
	r.record("GetSealedConnectorCredential", connectionUID)
	return r.Store.GetSealedConnectorCredential(ctx, connectionUID)
}

func (r *recordingStore) PutSealedConnectorCredential(ctx context.Context, request storagev1.PutSealedConnectorCredentialRequest) (*storagev1.SealedConnectorCredential, error) {
	r.record("PutSealedConnectorCredential", request)
	return r.Store.PutSealedConnectorCredential(ctx, request)
}

func (r *recordingStore) ReplaceSealedConnectorCredential(ctx context.Context, request storagev1.ReplaceSealedConnectorCredentialRequest) (*storagev1.SealedConnectorCredential, error) {
	r.record("ReplaceSealedConnectorCredential", request)
	return r.Store.ReplaceSealedConnectorCredential(ctx, request)
}

func (r *recordingStore) RetireSealedConnectorCredential(ctx context.Context, retired storagev1.SealedRetiredConnectorCredential) error {
	r.record("RetireSealedConnectorCredential", retired)
	return r.Store.RetireSealedConnectorCredential(ctx, retired)
}

func (r *recordingStore) TombstoneConnectorCustody(ctx context.Context, connectionUID string) error {
	r.record("TombstoneConnectorCustody", connectionUID)
	return r.Store.TombstoneConnectorCustody(ctx, connectionUID)
}

func (r *recordingStore) ShredConnectorCredential(ctx context.Context, connectionUID string, expectedVersion int64) error {
	r.record("ShredConnectorCredential", connectionUID)
	return r.Store.ShredConnectorCredential(ctx, connectionUID, expectedVersion)
}

func (r *recordingStore) DeleteConnectorCredential(ctx context.Context, connectionUID string) error {
	r.record("DeleteConnectorCredential", connectionUID)
	return r.Store.DeleteConnectorCredential(ctx, connectionUID)
}

func (r *recordingStore) CreateSealedConnectorConsent(ctx context.Context, request storagev1.CreateSealedConnectorConsentRequest) error {
	r.record("CreateSealedConnectorConsent", request)
	return r.Store.CreateSealedConnectorConsent(ctx, request)
}

func (r *recordingStore) ConsumeSealedConnectorConsent(ctx context.Context, nonce string, now time.Time) (*storagev1.SealedConnectorConsent, error) {
	r.record("ConsumeSealedConnectorConsent", nonce)
	return r.Store.ConsumeSealedConnectorConsent(ctx, nonce, now)
}

func (r *recordingStore) CreateSealedConnectorCompletion(ctx context.Context, request storagev1.CreateSealedConnectorCompletionRequest) error {
	r.record("CreateSealedConnectorCompletion", request)
	return r.Store.CreateSealedConnectorCompletion(ctx, request)
}

func (r *recordingStore) GetSealedConnectorCompletion(ctx context.Context, nonce string) (*storagev1.SealedConnectorCompletion, error) {
	r.record("GetSealedConnectorCompletion", nonce)
	return r.Store.GetSealedConnectorCompletion(ctx, nonce)
}

func (r *recordingStore) ConsumeSealedConnectorCompletion(ctx context.Context, nonce string) (*storagev1.SealedConnectorCompletion, error) {
	r.record("ConsumeSealedConnectorCompletion", nonce)
	return r.Store.ConsumeSealedConnectorCompletion(ctx, nonce)
}

func (r *recordingStore) CommitSealedConnectorCompletion(ctx context.Context, request storagev1.CommitSealedConnectorCompletionRequest) (*storagev1.SealedConnectorCredential, error) {
	r.record("CommitSealedConnectorCompletion", request)
	return r.Store.CommitSealedConnectorCompletion(ctx, request)
}

func (r *recordingStore) DeleteConnectorCompletion(ctx context.Context, nonce string) error {
	r.record("DeleteConnectorCompletion", nonce)
	return r.Store.DeleteConnectorCompletion(ctx, nonce)
}

func (r *recordingStore) DeleteConnectorConsentsForConnection(ctx context.Context, connectionUID string) error {
	r.record("DeleteConnectorConsentsForConnection", connectionUID)
	return r.Store.DeleteConnectorConsentsForConnection(ctx, connectionUID)
}

func (r *recordingStore) ListSealedConnectorCredentials(ctx context.Context, cursor string, limit int) (*storagev1.SealedConnectorCredentialPage, error) {
	r.record("ListSealedConnectorCredentials", cursor)
	return r.Store.ListSealedConnectorCredentials(ctx, cursor, limit)
}

func (r *recordingStore) ListSealedRetiredConnectorCredentials(ctx context.Context, cursor string, limit int) (*storagev1.SealedRetiredConnectorCredentialPage, error) {
	r.record("ListSealedRetiredConnectorCredentials", cursor)
	return r.Store.ListSealedRetiredConnectorCredentials(ctx, cursor, limit)
}

func (r *recordingStore) ListSealedConnectorConsents(ctx context.Context, cursor string, limit int) (*storagev1.SealedConnectorConsentPage, error) {
	r.record("ListSealedConnectorConsents", cursor)
	return r.Store.ListSealedConnectorConsents(ctx, cursor, limit)
}

func (r *recordingStore) ListSealedConnectorCompletions(ctx context.Context, cursor string, limit int) (*storagev1.SealedConnectorCompletionPage, error) {
	r.record("ListSealedConnectorCompletions", cursor)
	return r.Store.ListSealedConnectorCompletions(ctx, cursor, limit)
}

func connectorRef(uid string) store.ConnectorCredentialRef {
	return store.ConnectorCredentialRef{ConnectionUID: uid, Namespace: "tenant", Name: "github-" + uid, SubjectDigest: "subject-digest", Provider: "github"}
}

func connectorConsent(nonce, uid string) store.ConnectorConsent {
	return store.ConnectorConsent{
		Nonce: nonce, ConnectionUID: uid, Namespace: "tenant", Name: "github-" + uid, SubjectDigest: "subject-digest", Provider: "github",
		Mode: "readOnly", CodeVerifier: "pkce-verifier-" + nonce, AuthorityDigest: "authority-digest", RevocationDigest: "revocation-digest",
		Scopes: []string{"read:user", "repo"}, ExpiresAt: time.Now().Add(10 * time.Minute),
	}
}

func connectorCompletion(nonce, uid string, sequence int64, token string) store.ConnectorCompletion {
	return store.ConnectorCompletion{
		Nonce: nonce, ConnectionUID: uid, Namespace: "tenant", Name: "github-" + uid, SubjectDigest: "subject-digest", Provider: "github",
		Mode: "readOnly", ConsentAuthorityDigest: "consent-authority", ConsentSequence: sequence, ExpiresAt: time.Now().Add(10 * time.Minute),
		Credential: store.ConnectorCredential{
			AccessToken: "access-" + token, RefreshToken: "refresh-" + token, TokenType: "bearer", Scopes: []string{"repo"},
			AuthorityDigest: "authority-digest", RevocationDigest: "revocation-digest", ExpiresAt: time.Now().Add(time.Hour),
		},
	}
}

// TestProviderReceivesOnlySealedConnectorCustody drives every connector
// custody path through the client, then proves the provider never received a
// token, a PKCE verifier, a per-record data key or the snapshot key, in any
// encoding.
func TestProviderReceivesOnlySealedConnectorCustody(t *testing.T) {
	var dataKeys [][]byte
	t.Cleanup(storageclient.ObserveConnectorDataKeys(func(key []byte) { dataKeys = append(dataKeys, key) }))
	recorder := newRecordingStore(t)
	client := activatedClient(t, recorder, sealedTestKey)
	ctx := t.Context()
	ref := connectorRef("uid-recorded")
	tokens := make([]string, 0, 16)
	tokens = append(tokens, "pkce-verifier-consent-recorded")

	consent := connectorConsent("consent-recorded", ref.ConnectionUID)
	require.NoError(t, client.CreateConnectorConsent(ctx, consent))
	consumed, err := client.ConsumeConnectorConsent(ctx, consent.Nonce)
	require.NoError(t, err)
	require.Equal(t, consent.CodeVerifier, consumed.CodeVerifier)
	require.EqualValues(t, 1, consumed.Sequence)

	first := store.ConnectorCredential{AccessToken: "access-first-grant", RefreshToken: "refresh-first-grant", ExpiresAt: time.Now().Add(time.Hour)}
	tokens = append(tokens, first.AccessToken, first.RefreshToken)
	require.NoError(t, client.PutConnectorCredential(ctx, ref, first))

	completion := connectorCompletion("completion-recorded", ref.ConnectionUID, consumed.Sequence, "parked-grant")
	tokens = append(tokens, completion.Credential.AccessToken, completion.Credential.RefreshToken)
	require.NoError(t, client.CreateConnectorCompletion(ctx, completion))
	peeked, err := client.PeekConnectorCompletion(ctx, completion.Nonce)
	require.NoError(t, err)
	require.Equal(t, completion.Credential.AccessToken, peeked.Credential.AccessToken)
	committed, err := client.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential)
	require.NoError(t, err)
	require.EqualValues(t, 2, committed.GrantSequence)
	listed, err := client.ListConnectorCompletionsForConnection(ctx, ref.ConnectionUID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.True(t, listed[0].Committed)

	refreshed := store.ConnectorCredential{AccessToken: "access-refreshed-grant", RefreshToken: "refresh-refreshed-grant", ExpiresAt: time.Now().Add(time.Hour)}
	tokens = append(tokens, refreshed.AccessToken, refreshed.RefreshToken)
	require.NoError(t, client.ReplaceConnectorCredential(ctx, ref, refreshed, committed.Version))
	lost := store.ConnectorCredential{AccessToken: "access-lost-refresh", RefreshToken: "refresh-lost-refresh"}
	tokens = append(tokens, lost.AccessToken, lost.RefreshToken)
	require.NoError(t, client.RetireConnectorCredential(ctx, ref, lost))
	retired, err := client.ListRetiredConnectorCredentials(ctx, ref)
	require.NoError(t, err)
	require.Len(t, retired, 3, "the first grant, the refreshed-away grant and the lost refresh")
	held, err := client.GetConnectorCredential(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, refreshed.AccessToken, held.AccessToken)
	require.EqualValues(t, 2, held.GrantSequence, "a refresh carries the grant forward")

	consumedCompletion, err := client.ConsumeConnectorCompletion(ctx, completion.Nonce)
	require.NoError(t, err)
	require.True(t, consumedCompletion.Committed)
	// Key activation pages through every connector family.
	activatedClient(t, recorder, sealedTestKey)
	require.NoError(t, client.TombstoneConnectorCustody(ctx, ref.ConnectionUID))
	require.NoError(t, client.DeleteConnectorCredential(ctx, ref.ConnectionUID))

	recorder.requireCalled(t, "CreateSealedConnectorConsent", "ConsumeSealedConnectorConsent", "PutSealedConnectorCredential",
		"CreateSealedConnectorCompletion", "GetSealedConnectorCompletion", "CommitSealedConnectorCompletion",
		"ReplaceSealedConnectorCredential", "RetireSealedConnectorCredential", "ReadConnectorCustody", "GetSealedConnectorCredential",
		"ConsumeSealedConnectorCompletion", "ListSealedConnectorCredentials", "ListSealedRetiredConnectorCredentials",
		"ListSealedConnectorConsents", "ListSealedConnectorCompletions")
	require.Len(t, dataKeys, 4, "one data key per sealed credential record")
	secrets := map[string]string{
		"key":         string(sealedTestKey),
		"hex key":     hex.EncodeToString(sealedTestKey),
		"base64 key":  base64.StdEncoding.EncodeToString(sealedTestKey),
		"token field": `"accessToken"`,
	}
	for i, token := range tokens {
		secrets[fmt.Sprintf("token %d", i)] = token
		secrets[fmt.Sprintf("base64 token %d", i)] = base64.StdEncoding.EncodeToString([]byte(token))
	}
	for i, key := range dataKeys {
		secrets[fmt.Sprintf("data key %d", i)] = string(key)
		secrets[fmt.Sprintf("hex data key %d", i)] = hex.EncodeToString(key)
		secrets[fmt.Sprintf("base64 data key %d", i)] = base64.StdEncoding.EncodeToString(key)
	}
	recorder.requireNoSecret(t, secrets)
	// The provider received the records' plain facts, so the check above is
	// not passing on empty records.
	recorder.requireReceived(t, "CommitSealedConnectorCompletion", ref.SubjectDigest)
	recorder.requireReceived(t, "CreateSealedConnectorConsent", consent.AuthorityDigest)
	recorder.requireReceived(t, "RetireSealedConnectorCredential", ref.ConnectionUID)
}

// Legacy connector formats, implemented independently of the client as the
// embedded store sealed them before the client held the key.

func legacyConnectorCredentialAAD(ref store.ConnectorCredentialRef) []byte {
	return []byte("orka.connector-credential\x00" + ref.ConnectionUID + "\x00" + ref.Namespace + "\x00" + ref.Name + "\x00" + ref.SubjectDigest + "\x00" + ref.Provider)
}

func legacyConnectorDataKeyAAD(uid string) []byte {
	return []byte("orka.connector-data-key\x00" + uid)
}

func legacyConnectorConsentAAD(c storagev1.SealedConnectorConsent) []byte {
	return fmt.Appendf(nil, "orka.connector-consent\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d",
		c.Nonce, c.ConnectionUID, c.Namespace, c.Name, c.SubjectDigest, c.Provider, c.Mode, c.AuthorityDigest, c.RevocationDigest,
		strings.Join(c.Scopes, " "), c.ExpiresAt.UnixNano(), c.Sequence)
}

func legacyConnectorCompletionAAD(c storagev1.SealedConnectorCompletion) []byte {
	return fmt.Appendf(nil, "orka.connector-completion\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d",
		c.Nonce, c.ConnectionUID, c.SubjectDigest, c.Namespace, c.Name, c.Provider, c.Mode, c.ExpiresAt.UnixNano(), c.ConsentSequence)
}

// TestSealedConnectorFormatsMatchStoredRecords proves the client opens
// connector records sealed in the stored formats and writes records those
// formats open, so existing datasets open unchanged.
func TestSealedConnectorFormatsMatchStoredRecords(t *testing.T) {
	backend := openSQLite(t)
	ctx := t.Context()
	now := time.Now().UTC()
	ref := connectorRef("uid-legacy")

	// A credential sealed under its own data key, in the stored format.
	dataKey := bytes.Repeat([]byte{0x6b}, 32)
	expires := time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)
	body := []byte(`{"accessToken":"legacy-access","refreshToken":"legacy-refresh","tokenType":"bearer","expiresAt":"` +
		expires.Format(time.RFC3339Nano) + `","scopes":["repo"],"authorityDigest":"authority","revocationDigest":"revocation","grantSequence":1}`)
	nonce, ciphertext := legacySeal(t, dataKey, legacyConnectorCredentialAAD(ref), body)
	keyNonce, keyCiphertext := legacySeal(t, sealedTestKey, legacyConnectorDataKeyAAD(ref.ConnectionUID), dataKey)
	_, err := backend.PutSealedConnectorCredential(ctx, storagev1.PutSealedConnectorCredentialRequest{Now: now, Credential: storagev1.SealedConnectorCredential{
		ConnectionUID: ref.ConnectionUID, Namespace: ref.Namespace, Name: ref.Name, SubjectDigest: ref.SubjectDigest, Provider: ref.Provider,
		DataKeyNonce: keyNonce, DataKeyCiphertext: keyCiphertext, Nonce: nonce, Ciphertext: ciphertext, ExpiresAt: expires, GrantSequence: 1,
	}})
	require.NoError(t, err)

	consent := storagev1.SealedConnectorConsent{
		Nonce: "legacy-consent", ConnectionUID: ref.ConnectionUID, Namespace: ref.Namespace, Name: ref.Name, SubjectDigest: ref.SubjectDigest,
		Provider: ref.Provider, Mode: "readOnly", AuthorityDigest: "authority", RevocationDigest: "revocation", Scopes: []string{"read:user", "repo"},
		Sequence: 1, ExpiresAt: now.Add(10 * time.Minute),
	}
	consent.VerifierNonce, consent.VerifierCiphertext = legacySeal(t, sealedTestKey, legacyConnectorConsentAAD(consent), []byte("legacy-verifier"))
	require.NoError(t, backend.CreateSealedConnectorConsent(ctx, storagev1.CreateSealedConnectorConsentRequest{Consent: consent, Now: now}))

	completion := storagev1.SealedConnectorCompletion{
		Nonce: "legacy-completion", ConnectionUID: ref.ConnectionUID, Namespace: ref.Namespace, Name: ref.Name, SubjectDigest: ref.SubjectDigest,
		Provider: ref.Provider, Mode: "readOnly", ConsentSequence: 2, ExpiresAt: now.Add(10 * time.Minute),
	}
	completion.PayloadNonce, completion.Payload = legacySeal(t, sealedTestKey, legacyConnectorCompletionAAD(completion),
		[]byte(`{"accessToken":"legacy-parked","refreshToken":"legacy-parked-refresh","authorityDigest":"authority","consentAuthorityDigest":"consent-authority"}`))
	require.NoError(t, backend.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{Completion: completion, Now: now}))

	client := activatedClient(t, backend, sealedTestKey)
	held, err := client.GetConnectorCredential(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, "legacy-access", held.AccessToken)
	require.Equal(t, "legacy-refresh", held.RefreshToken)
	require.True(t, expires.Equal(held.ExpiresAt))
	require.Equal(t, []string{"repo"}, held.Scopes)
	require.EqualValues(t, 1, held.GrantSequence)
	parked, err := client.PeekConnectorCompletion(ctx, completion.Nonce)
	require.NoError(t, err)
	require.Equal(t, "legacy-parked", parked.Credential.AccessToken)
	require.Equal(t, "consent-authority", parked.ConsentAuthorityDigest)
	require.False(t, parked.Committed)
	opened, err := client.ConsumeConnectorConsent(ctx, consent.Nonce)
	require.NoError(t, err)
	require.Equal(t, "legacy-verifier", opened.CodeVerifier)

	// Records the client seals open in the stored formats.
	_, err = client.CommitConnectorCompletion(ctx, completion.Nonce, ref, parked.Credential)
	require.NoError(t, err)
	sealed, err := backend.GetSealedConnectorCredential(ctx, ref.ConnectionUID)
	require.NoError(t, err)
	clientKey := legacyOpen(t, sealedTestKey, sealed.DataKeyNonce, sealed.DataKeyCiphertext, legacyConnectorDataKeyAAD(ref.ConnectionUID))
	var credentialBody map[string]any
	require.NoError(t, json.Unmarshal(legacyOpen(t, clientKey, sealed.Nonce, sealed.Ciphertext, legacyConnectorCredentialAAD(ref)), &credentialBody))
	require.Equal(t, "legacy-parked", credentialBody["accessToken"])
	require.EqualValues(t, 2, credentialBody["grantSequence"])
	committedCompletion, err := backend.GetSealedConnectorCompletion(ctx, completion.Nonce)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(legacyOpen(t, sealedTestKey, committedCompletion.PayloadNonce, committedCompletion.Payload,
		legacyConnectorCompletionAAD(*committedCompletion)), &payload))
	require.Equal(t, true, payload["committed"])
	require.Equal(t, "consent-authority", payload["consentAuthorityDigest"])
	require.EqualValues(t, 2, payload["grantSequence"])
	custody, err := backend.ReadConnectorCustody(ctx, ref.ConnectionUID)
	require.NoError(t, err)
	require.Len(t, custody.Retired, 1, "the legacy grant is retired as stored")
	require.Equal(t, keyCiphertext, custody.Retired[0].DataKeyCiphertext)
	require.Equal(t, ciphertext, custody.Retired[0].Ciphertext)

	clientConsent := connectorConsent("client-consent", ref.ConnectionUID)
	require.NoError(t, client.CreateConnectorConsent(ctx, clientConsent))
	page, err := backend.ListSealedConnectorConsents(ctx, "", storagev1.MaxSealedRecordPageSize)
	require.NoError(t, err)
	require.Len(t, page.Consents, 1)
	stored := page.Consents[0]
	require.EqualValues(t, 3, stored.Sequence, "a committed consent raises the issued sequence")
	require.Equal(t, clientConsent.CodeVerifier, string(legacyOpen(t, sealedTestKey, stored.VerifierNonce, stored.VerifierCiphertext, legacyConnectorConsentAAD(stored))))
}

// connectorTamperingStore changes what the provider returns for connector
// records.
type connectorTamperingStore struct {
	*sqlite.Store
	credential func(*storagev1.SealedConnectorCredential)
	completion func(*storagev1.SealedConnectorCompletion)
	consent    func(*storagev1.SealedConnectorConsent)
}

func (s *connectorTamperingStore) GetSealedConnectorCredential(ctx context.Context, uid string) (*storagev1.SealedConnectorCredential, error) {
	sealed, err := s.Store.GetSealedConnectorCredential(ctx, uid)
	if err == nil && s.credential != nil {
		s.credential(sealed)
	}
	return sealed, err
}

func (s *connectorTamperingStore) ReadConnectorCustody(ctx context.Context, uid string) (*storagev1.ConnectorCustody, error) {
	custody, err := s.Store.ReadConnectorCustody(ctx, uid)
	if err == nil && custody.Credential != nil && s.credential != nil {
		s.credential(custody.Credential)
	}
	if err == nil && s.completion != nil {
		for i := range custody.Completions {
			s.completion(&custody.Completions[i])
		}
	}
	return custody, err
}

func (s *connectorTamperingStore) GetSealedConnectorCompletion(ctx context.Context, nonce string) (*storagev1.SealedConnectorCompletion, error) {
	sealed, err := s.Store.GetSealedConnectorCompletion(ctx, nonce)
	if err == nil && s.completion != nil {
		s.completion(sealed)
	}
	return sealed, err
}

func (s *connectorTamperingStore) ConsumeSealedConnectorConsent(ctx context.Context, nonce string, now time.Time) (*storagev1.SealedConnectorConsent, error) {
	sealed, err := s.Store.ConsumeSealedConnectorConsent(ctx, nonce, now)
	if err == nil && s.consent != nil {
		s.consent(sealed)
	}
	return sealed, err
}

// TestClientRejectsModifiedConnectorRecords covers records a provider altered
// or returned for another Connection: the client refuses them without
// panicking and without echoing token material.
func TestClientRejectsModifiedConnectorRecords(t *testing.T) {
	ctx := t.Context()
	backend := &connectorTamperingStore{Store: openSQLite(t)}
	client := activatedClient(t, backend, sealedTestKey)
	ref, other := connectorRef("uid-a"), connectorRef("uid-b")
	require.NoError(t, client.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "access-held-a", RefreshToken: "refresh-held-a"}))
	require.NoError(t, client.PutConnectorCredential(ctx, other, store.ConnectorCredential{AccessToken: "access-held-b"}))
	otherSealed, err := backend.Store.GetSealedConnectorCredential(ctx, other.ConnectionUID)
	require.NoError(t, err)

	for name, tamper := range map[string]func(*storagev1.SealedConnectorCredential){
		"ciphertext":       func(s *storagev1.SealedConnectorCredential) { s.Ciphertext[0] ^= 0xff },
		"data key":         func(s *storagev1.SealedConnectorCredential) { s.DataKeyCiphertext[0] ^= 0xff },
		"short nonce":      func(s *storagev1.SealedConnectorCredential) { s.Nonce = s.Nonce[:4] },
		"short key nonce":  func(s *storagev1.SealedConnectorCredential) { s.DataKeyNonce = s.DataKeyNonce[:4] },
		"grant sequence":   func(s *storagev1.SealedConnectorCredential) { s.GrantSequence = 7 },
		"other connection": func(s *storagev1.SealedConnectorCredential) { s.ConnectionUID = other.ConnectionUID },
		"other record":     func(s *storagev1.SealedConnectorCredential) { *s = *otherSealed; s.ConnectionUID = ref.ConnectionUID },
		"other record body": func(s *storagev1.SealedConnectorCredential) {
			s.Nonce, s.Ciphertext = otherSealed.Nonce, otherSealed.Ciphertext
		},
	} {
		stored, err := backend.Store.GetSealedConnectorCredential(ctx, ref.ConnectionUID)
		require.NoError(t, err)
		backend.credential = tamper
		_, err = client.GetConnectorCredential(ctx, ref)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "access-held", name)
		err = client.ReplaceConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "access-replacement"}, stored.Version)
		require.Error(t, err, name)
		require.NotErrorIs(t, err, store.ErrConflict, "%s: the client refuses the record before it decides", name)
	}
	backend.credential = nil
	held, err := client.GetConnectorCredential(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, "access-held-a", held.AccessToken)

	completion := connectorCompletion("completion-tampered", ref.ConnectionUID, 1, "parked-tampered")
	require.NoError(t, client.CreateConnectorCompletion(ctx, completion))
	for name, tamper := range map[string]func(*storagev1.SealedConnectorCompletion){
		"mode":             func(s *storagev1.SealedConnectorCompletion) { s.Mode = "readWrite" },
		"expiry":           func(s *storagev1.SealedConnectorCompletion) { s.ExpiresAt = s.ExpiresAt.Add(time.Hour) },
		"consent sequence": func(s *storagev1.SealedConnectorCompletion) { s.ConsentSequence = 9 },
		"payload":          func(s *storagev1.SealedConnectorCompletion) { s.Payload[0] ^= 0xff },
		"short nonce":      func(s *storagev1.SealedConnectorCompletion) { s.PayloadNonce = s.PayloadNonce[:4] },
	} {
		backend.completion = tamper
		_, err := client.PeekConnectorCompletion(ctx, completion.Nonce)
		require.ErrorIs(t, err, store.ErrNotFound, name)
		_, err = client.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "parked-tampered", name)
	}
	backend.completion = nil
	committed, err := client.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential)
	require.NoError(t, err)
	require.EqualValues(t, 2, committed.GrantSequence, "no refused attempt took a grant")

	for name, tamper := range map[string]func(*storagev1.SealedConnectorConsent){
		"authority": func(s *storagev1.SealedConnectorConsent) { s.AuthorityDigest = "another-authority" },
		"scopes":    func(s *storagev1.SealedConnectorConsent) { s.Scopes = append(s.Scopes, "admin") },
		"sequence":  func(s *storagev1.SealedConnectorConsent) { s.Sequence++ },
		"expiry":    func(s *storagev1.SealedConnectorConsent) { s.ExpiresAt = s.ExpiresAt.Add(time.Minute) },
		"verifier":  func(s *storagev1.SealedConnectorConsent) { s.VerifierCiphertext[0] ^= 0xff },
	} {
		consent := connectorConsent("consent-"+strings.ReplaceAll(name, " ", "-"), ref.ConnectionUID)
		require.NoError(t, client.CreateConnectorConsent(ctx, consent))
		backend.consent = tamper
		_, err := client.ConsumeConnectorConsent(ctx, consent.Nonce)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), consent.CodeVerifier, name)
	}
}

// TestConnectorKeyActivationAuthenticatesEveryFamily covers key rotation over
// each kind of retained connector record on its own: a key that cannot open
// it is refused and not installed, while an expired consent does not block.
func TestConnectorKeyActivationAuthenticatesEveryFamily(t *testing.T) {
	wrong := bytes.Repeat([]byte{0x24}, storageclient.SnapshotKeyBytes)
	for name, seed := range map[string]func(*testing.T, *storageclient.Client, *sqlite.Store){
		"current custody": func(t *testing.T, c *storageclient.Client, _ *sqlite.Store) {
			require.NoError(t, c.PutConnectorCredential(t.Context(), connectorRef("uid"), store.ConnectorCredential{AccessToken: "access"}))
		},
		"retired custody": func(t *testing.T, c *storageclient.Client, _ *sqlite.Store) {
			require.NoError(t, c.RetireConnectorCredential(t.Context(), connectorRef("uid"), store.ConnectorCredential{AccessToken: "access", RefreshToken: "refresh"}))
		},
		"pending consent": func(t *testing.T, c *storageclient.Client, _ *sqlite.Store) {
			require.NoError(t, c.CreateConnectorConsent(t.Context(), connectorConsent("nonce", "uid")))
		},
		"parked completion": func(t *testing.T, c *storageclient.Client, _ *sqlite.Store) {
			require.NoError(t, c.CreateConnectorCompletion(t.Context(), connectorCompletion("nonce", "uid", 1, "parked")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			backend := openSQLite(t)
			seed(t, activatedClient(t, backend, sealedTestKey), backend)
			candidate, err := storageclient.NewEmbedded(backend)
			require.NoError(t, err)
			err = candidate.ActivateSnapshotKey(t.Context(), wrong)
			require.ErrorContains(t, err, "connector")
			require.ErrorContains(t, err, "restore the previous key")
			_, err = candidate.GetConnectorCredential(t.Context(), connectorRef("uid"))
			require.ErrorIs(t, err, storageclient.ErrSnapshotKeyNotActive, "a rejected key is not installed")
			require.NoError(t, candidate.ActivateSnapshotKey(t.Context(), sealedTestKey))
		})
	}
	t.Run("expired consent", func(t *testing.T) {
		backend := openSQLite(t)
		expired := connectorConsent("nonce", "uid")
		expired.ExpiresAt = time.Now().Add(-time.Minute)
		require.NoError(t, activatedClient(t, backend, sealedTestKey).CreateConnectorConsent(t.Context(), expired))
		candidate, err := storageclient.NewEmbedded(backend)
		require.NoError(t, err)
		require.NoError(t, candidate.ActivateSnapshotKey(t.Context(), wrong))
	})
}

// interleavingStore runs a step once, right after the client reads a
// Connection's custody, as another API replica would between this replica's
// read and its conditional commit.
type interleavingStore struct {
	*sqlite.Store
	afterRead func()
}

func (s *interleavingStore) ReadConnectorCustody(ctx context.Context, uid string) (*storagev1.ConnectorCustody, error) {
	custody, err := s.Store.ReadConnectorCustody(ctx, uid)
	if step := s.afterRead; step != nil {
		s.afterRead = nil
		step()
	}
	return custody, err
}

// TestConnectorWritesRereadCustodyThatChanged covers concurrent writers: a
// conditional command built from custody another replica changed is refused
// and decided again from a fresh read, so a completion is committed once,
// consents take distinct sequences and an older consent never parks over a
// newer one.
func TestConnectorWritesRereadCustodyThatChanged(t *testing.T) {
	ctx := t.Context()
	backend := &interleavingStore{Store: openSQLite(t)}
	client := activatedClient(t, backend, sealedTestKey)
	other := activatedClient(t, backend.Store, sealedTestKey)
	ref := connectorRef("uid-raced")

	completion := connectorCompletion("completion-raced", ref.ConnectionUID, 1, "raced")
	require.NoError(t, client.CreateConnectorCompletion(ctx, completion))
	backend.afterRead = func() {
		_, err := other.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential)
		require.NoError(t, err)
	}
	_, err := client.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential)
	require.ErrorIs(t, err, store.ErrConnectorCompletionCommitted)
	held, err := client.GetConnectorCredential(ctx, ref)
	require.NoError(t, err)
	require.EqualValues(t, 1, held.GrantSequence, "the completion took one grant")
	retired, err := client.ListRetiredConnectorCredentials(ctx, ref)
	require.NoError(t, err)
	require.Empty(t, retired)

	first, second := connectorConsent("consent-first", ref.ConnectionUID), connectorConsent("consent-second", ref.ConnectionUID)
	backend.afterRead = func() { require.NoError(t, other.CreateConnectorConsent(ctx, first)) }
	require.NoError(t, client.CreateConnectorConsent(ctx, second))
	consumed, err := client.ConsumeConnectorConsent(ctx, second.Nonce)
	require.NoError(t, err)
	require.EqualValues(t, 3, consumed.Sequence, "the committed consent and the racing one came first")
	_, err = client.ConsumeConnectorConsent(ctx, first.Nonce)
	require.ErrorIs(t, err, store.ErrNotFound, "the later consent replaced the pending one")

	older := connectorCompletion("completion-older", ref.ConnectionUID, 2, "older")
	newer := connectorCompletion("completion-newer", ref.ConnectionUID, 3, "newer")
	backend.afterRead = func() { require.NoError(t, other.CreateConnectorCompletion(ctx, newer)) }
	require.ErrorIs(t, client.CreateConnectorCompletion(ctx, older), store.ErrConnectorConsentSuperseded)
	parked, err := client.PeekConnectorCompletion(ctx, newer.Nonce)
	require.NoError(t, err)
	require.Equal(t, newer.Credential.AccessToken, parked.Credential.AccessToken)
	_, err = client.PeekConnectorCompletion(ctx, older.Nonce)
	require.ErrorIs(t, err, store.ErrNotFound)

	// A commit whose custody keeps changing gives up after bounded rereads
	// and changes nothing.
	rounds := 0
	var churn func()
	churn = func() {
		rounds++
		require.NoError(t, other.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: fmt.Sprintf("access-churn-%d", rounds)}))
		backend.afterRead = churn
	}
	backend.afterRead = churn
	_, err = client.CommitConnectorCompletion(ctx, newer.Nonce, ref, newer.Credential)
	backend.afterRead = nil
	require.ErrorIs(t, err, store.ErrConflict)
	require.Equal(t, storageclient.ConnectorCommitAttempts, rounds)
	parked, err = client.PeekConnectorCompletion(ctx, newer.Nonce)
	require.NoError(t, err)
	require.False(t, parked.Committed, "a refused commit changes nothing")
}
