package storageclient

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	connectorDataKeyBytes         = 32
	connectorCommitAttempts       = 8
	connectorVerificationPageSize = storagev1.MaxSealedRecordPageSize
)

var newConnectorDataKey = func() ([]byte, error) {
	key := make([]byte, connectorDataKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate connector data key: %w", err)
	}
	return key, nil
}

type sealedConnectorCredential struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken,omitempty"`
	TokenType        string   `json:"tokenType,omitempty"`
	ExpiresAt        string   `json:"expiresAt,omitempty"`
	Scopes           []string `json:"scopes,omitempty"`
	AuthorityDigest  string   `json:"authorityDigest,omitempty"`
	RevocationDigest string   `json:"revocationDigest,omitempty"`
	GrantSequence    int64    `json:"grantSequence,omitempty"`
}

type sealedConnectorCompletionPayload struct {
	sealedConnectorCredential
	ConsentAuthorityDigest string `json:"consentAuthorityDigest,omitempty"`
	Committed              bool   `json:"committed,omitempty"`
}

func connectorDataKeyAdditionalData(connectionUID string) []byte {
	return fmt.Appendf(nil, "orka.connector-data-key\x00%s", connectionUID)
}

func connectorCredentialAdditionalData(ref store.ConnectorCredentialRef) []byte {
	return fmt.Appendf(nil, "orka.connector-credential\x00%s\x00%s\x00%s\x00%s\x00%s",
		ref.ConnectionUID, ref.Namespace, ref.Name, ref.SubjectDigest, ref.Provider)
}

func connectorConsentAdditionalData(consent storagev1.SealedConnectorConsent) []byte {
	return fmt.Appendf(nil, "orka.connector-consent\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d",
		consent.Nonce, consent.ConnectionUID, consent.Namespace, consent.Name, consent.SubjectDigest,
		consent.Provider, consent.Mode, consent.AuthorityDigest, consent.RevocationDigest, strings.Join(consent.Scopes, " "),
		consent.ExpiresAt.UTC().UnixNano(), consent.Sequence)
}

func connectorCompletionAdditionalData(completion storagev1.SealedConnectorCompletion) []byte {
	return fmt.Appendf(nil, "orka.connector-completion\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d",
		completion.Nonce, completion.ConnectionUID, completion.SubjectDigest,
		completion.Namespace, completion.Name, completion.Provider, completion.Mode,
		completion.ExpiresAt.UTC().UnixNano(), completion.ConsentSequence)
}

func toSealedConnectorCredential(credential store.ConnectorCredential) sealedConnectorCredential {
	return sealedConnectorCredential{
		AccessToken:      credential.AccessToken,
		RefreshToken:     credential.RefreshToken,
		TokenType:        credential.TokenType,
		ExpiresAt:        formatConnectorTime(credential.ExpiresAt),
		Scopes:           credential.Scopes,
		AuthorityDigest:  credential.AuthorityDigest,
		RevocationDigest: credential.RevocationDigest,
		GrantSequence:    credential.GrantSequence,
	}
}

func fromSealedConnectorCredential(sealed sealedConnectorCredential) (store.ConnectorCredential, error) {
	expiresAt, err := parseConnectorTime(sealed.ExpiresAt)
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("decode connector credential expiry: %w", err)
	}
	return store.ConnectorCredential{
		AccessToken:      sealed.AccessToken,
		RefreshToken:     sealed.RefreshToken,
		TokenType:        sealed.TokenType,
		ExpiresAt:        expiresAt,
		Scopes:           sealed.Scopes,
		AuthorityDigest:  sealed.AuthorityDigest,
		RevocationDigest: sealed.RevocationDigest,
		GrantSequence:    sealed.GrantSequence,
	}, nil
}

type connectorCompletionFields struct {
	ConsentAuthorityDigest string
	Committed              bool
}

func encodeConnectorCompletionPayload(credential store.ConnectorCredential, fields connectorCompletionFields) ([]byte, error) {
	body, err := json.Marshal(sealedConnectorCompletionPayload{
		sealedConnectorCredential: toSealedConnectorCredential(credential),
		ConsentAuthorityDigest:    fields.ConsentAuthorityDigest,
		Committed:                 fields.Committed,
	})
	if err != nil {
		return nil, fmt.Errorf("encode connector completion: %w", err)
	}
	return body, nil
}

func decodeConnectorCompletionPayload(body []byte) (store.ConnectorCredential, connectorCompletionFields, error) {
	var sealed sealedConnectorCompletionPayload
	if err := json.Unmarshal(body, &sealed); err != nil {
		return store.ConnectorCredential{}, connectorCompletionFields{}, errors.New("decode connector completion: invalid sealed payload")
	}
	credential, err := fromSealedConnectorCredential(sealed.sealedConnectorCredential)
	if err != nil {
		return store.ConnectorCredential{}, connectorCompletionFields{}, err
	}
	return credential, connectorCompletionFields{ConsentAuthorityDigest: sealed.ConsentAuthorityDigest, Committed: sealed.Committed}, nil
}

func formatConnectorTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseConnectorTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func newConnectorDataKeyAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize connector data key cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize connector data key AEAD: %w", err)
	}
	return aead, nil
}

func sealConnectorCredential(keys *keyCustody, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) (storagev1.SealedConnectorCredential, error) {
	body, err := json.Marshal(toSealedConnectorCredential(credential))
	if err != nil {
		return storagev1.SealedConnectorCredential{}, fmt.Errorf("encode connector credential: %w", err)
	}
	dataKey, err := newConnectorDataKey()
	if err != nil {
		return storagev1.SealedConnectorCredential{}, err
	}
	defer clear(dataKey)
	if len(dataKey) != connectorDataKeyBytes {
		return storagev1.SealedConnectorCredential{}, errors.New("connector data key must be 32 bytes")
	}
	dataAEAD, err := newConnectorDataKeyAEAD(dataKey)
	if err != nil {
		return storagev1.SealedConnectorCredential{}, err
	}
	sealed := storagev1.SealedConnectorCredential{
		ConnectionUID: ref.ConnectionUID, Namespace: ref.Namespace, Name: ref.Name,
		SubjectDigest: ref.SubjectDigest, Provider: ref.Provider, GrantSequence: credential.GrantSequence,
	}
	sealed.Nonce = make([]byte, dataAEAD.NonceSize())
	if _, err := rand.Read(sealed.Nonce); err != nil {
		return storagev1.SealedConnectorCredential{}, fmt.Errorf("generate nonce: %w", err)
	}
	sealed.Ciphertext = dataAEAD.Seal(nil, sealed.Nonce, body, connectorCredentialAdditionalData(ref))
	if sealed.DataKeyNonce, sealed.DataKeyCiphertext, err = keys.seal(connectorDataKeyAdditionalData(ref.ConnectionUID), dataKey); err != nil {
		return storagev1.SealedConnectorCredential{}, fmt.Errorf("seal connector data key: %w", err)
	}
	if !credential.ExpiresAt.IsZero() {
		sealed.ExpiresAt = credential.ExpiresAt.UTC()
	}
	return sealed, nil
}

func openConnectorCredential(keys *keyCustody, ref store.ConnectorCredentialRef, dataKeyNonce, dataKeyCiphertext, nonce, ciphertext []byte) (store.ConnectorCredential, error) {
	dataKey, err := keys.open(dataKeyNonce, dataKeyCiphertext, connectorDataKeyAdditionalData(ref.ConnectionUID))
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("unwrap connector data key for connection %s: %w", ref.ConnectionUID, err)
	}
	defer clear(dataKey)
	if len(dataKey) != connectorDataKeyBytes {
		return store.ConnectorCredential{}, fmt.Errorf("unwrap connector data key for connection %s: invalid data key", ref.ConnectionUID)
	}
	dataAEAD, err := newConnectorDataKeyAEAD(dataKey)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	if len(nonce) != dataAEAD.NonceSize() {
		return store.ConnectorCredential{}, fmt.Errorf("open connector credential for connection %s: %w", ref.ConnectionUID, errInvalidNonce)
	}
	body, err := dataAEAD.Open(nil, nonce, ciphertext, connectorCredentialAdditionalData(ref))
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("open connector credential for connection %s: binding mismatch or corrupt row: %w", ref.ConnectionUID, err)
	}
	var sealed sealedConnectorCredential
	if err := json.Unmarshal(body, &sealed); err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("decode connector credential for connection %s: invalid sealed body", ref.ConnectionUID)
	}
	return fromSealedConnectorCredential(sealed)
}

func openStoredConnectorCredential(keys *keyCustody, ref store.ConnectorCredentialRef, sealed *storagev1.SealedConnectorCredential) (store.ConnectorCredential, error) {
	if sealed == nil || sealed.ConnectionUID != ref.ConnectionUID {
		return store.ConnectorCredential{}, fmt.Errorf("connector custody provider returned another record for connection %s", ref.ConnectionUID)
	}
	credential, err := openConnectorCredential(keys, ref, sealed.DataKeyNonce, sealed.DataKeyCiphertext, sealed.Nonce, sealed.Ciphertext)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	if credential.GrantSequence != sealed.GrantSequence {
		return store.ConnectorCredential{}, fmt.Errorf("connector credential for connection %s: grant sequence column does not match the sealed grant", ref.ConnectionUID)
	}
	credential.UpdatedAt = sealed.UpdatedAt.UTC()
	credential.Version = sealed.Version
	return credential, nil
}

func openConnectorCompletion(keys *keyCustody, sealed storagev1.SealedConnectorCompletion) (store.ConnectorCompletion, error) {
	body, err := keys.open(sealed.PayloadNonce, sealed.Payload, connectorCompletionAdditionalData(sealed))
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	credential, fields, err := decodeConnectorCompletionPayload(body)
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	return store.ConnectorCompletion{
		Nonce: sealed.Nonce, ConnectionUID: sealed.ConnectionUID, Namespace: sealed.Namespace, Name: sealed.Name,
		SubjectDigest: sealed.SubjectDigest, Provider: sealed.Provider, Mode: sealed.Mode,
		Credential: credential, ExpiresAt: sealed.ExpiresAt.UTC(),
		ConsentAuthorityDigest: fields.ConsentAuthorityDigest, Committed: fields.Committed,
		ConsentSequence: sealed.ConsentSequence,
	}, nil
}

func judgeConnectorCompletions(keys *keyCustody, parked []storagev1.SealedConnectorCompletion) ([]storagev1.SealedConnectorCompletionRef, []storagev1.SealedConnectorCompletion) {
	refs := make([]storagev1.SealedConnectorCompletionRef, 0, len(parked))
	var uncommitted []storagev1.SealedConnectorCompletion
	for _, completion := range parked {
		refs = append(refs, completion.Ref())
		if opened, err := openConnectorCompletion(keys, completion); err == nil && opened.Committed {
			continue
		}
		uncommitted = append(uncommitted, completion)
	}
	return refs, uncommitted
}

func connectorRetirementFor(previous, replacement store.ConnectorCredential, now time.Time, consentGrant bool) *storagev1.ConnectorRetirement {
	if previous.AccessToken == replacement.AccessToken && previous.RefreshToken == replacement.RefreshToken &&
		previous.RevocationDigest == replacement.RevocationDigest {
		return nil
	}
	revocableUntil := previous.ExpiresAt
	if previous.RefreshToken != "" && (consentGrant || previous.RefreshToken != replacement.RefreshToken) {
		revocableUntil = time.Time{}
	}
	if !revocableUntil.IsZero() && !revocableUntil.After(now) {
		return nil
	}
	return &storagev1.ConnectorRetirement{RevocableUntil: revocableUntil.UTC()}
}

func (c *Client) readConnectorCustody(ctx context.Context, connectionUID string) (*storagev1.ConnectorCustody, error) {
	custody, err := c.ReadConnectorCustody(ctx, connectionUID)
	if err != nil {
		return nil, err
	}
	foreign := custody == nil || custody.ConnectionUID != connectionUID ||
		(custody.Credential != nil && custody.Credential.ConnectionUID != connectionUID)
	for i := 0; !foreign && i < len(custody.Retired); i++ {
		foreign = custody.Retired[i].ConnectionUID != connectionUID
	}
	for i := 0; !foreign && i < len(custody.Completions); i++ {
		foreign = custody.Completions[i].ConnectionUID != connectionUID
	}
	if foreign {
		return nil, fmt.Errorf("connector custody provider returned another Connection's records for %s", connectionUID)
	}
	return custody, nil
}

func errConnectorCommitContended(operation string) error {
	return fmt.Errorf("%s: %w: connector custody kept changing while it was written", operation, store.ErrConflict)
}

func requireConnectorAccessToken(credential store.ConnectorCredential) error {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return errors.New("connector credential access token is required")
	}
	return nil
}

func (c *Client) PutConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	keys, err := c.activeCustody()
	if err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := requireConnectorAccessToken(credential); err != nil {
		return err
	}
	for range connectorCommitAttempts {
		custody, err := c.readConnectorCustody(ctx, ref.ConnectionUID)
		if err != nil {
			return err
		}
		if custody.Tombstoned {
			return store.ErrConnectorCustodyTombstoned
		}
		credential.GrantSequence = custody.LastGrantSequence + 1
		sealed, err := sealConnectorCredential(keys, ref, credential)
		if err != nil {
			return err
		}
		_, err = c.PutSealedConnectorCredential(ctx, storagev1.PutSealedConnectorCredentialRequest{Credential: sealed, Now: time.Now().UTC()})
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return errConnectorCommitContended("store connector credential")
}

func (c *Client) GetConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef) (store.ConnectorCredential, error) {
	keys, err := c.activeCustody()
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	if err := ref.Validate(); err != nil {
		return store.ConnectorCredential{}, err
	}
	sealed, err := c.GetSealedConnectorCredential(ctx, ref.ConnectionUID)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	return openStoredConnectorCredential(keys, ref, sealed)
}

func (c *Client) GetConnectorCredentialGrant(ctx context.Context, ref store.ConnectorCredentialRef) (store.ConnectorGrant, error) {
	keys, err := c.activeCustody()
	if err != nil {
		return store.ConnectorGrant{}, err
	}
	if err := ref.Validate(); err != nil {
		return store.ConnectorGrant{}, err
	}
	custody, err := c.readConnectorCustody(ctx, ref.ConnectionUID)
	if err != nil {
		return store.ConnectorGrant{}, err
	}
	if custody.Tombstoned {
		return store.ConnectorGrant{}, store.ErrConnectorCustodyTombstoned
	}
	if custody.Credential == nil {
		return store.ConnectorGrant{}, store.ErrNotFound
	}
	credential, err := openStoredConnectorCredential(keys, ref, custody.Credential)
	if err != nil {
		return store.ConnectorGrant{}, err
	}
	return store.ConnectorGrant{Sequence: credential.GrantSequence, Scopes: credential.Scopes}, nil
}

func (c *Client) ListRetiredConnectorCredentials(ctx context.Context, ref store.ConnectorCredentialRef) ([]store.ConnectorCredential, error) {
	keys, err := c.activeCustody()
	if err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	custody, err := c.readConnectorCustody(ctx, ref.ConnectionUID)
	if err != nil {
		return nil, err
	}
	var result []store.ConnectorCredential
	for _, retired := range custody.Retired {
		credential, err := openConnectorCredential(keys, ref, retired.DataKeyNonce, retired.DataKeyCiphertext, retired.Nonce, retired.Ciphertext)
		if err != nil {
			return nil, err
		}
		credential.UpdatedAt = retired.RetiredAt.UTC()
		result = append(result, credential)
	}
	return result, nil
}

func (c *Client) ReplaceConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential, expectedVersion int64) error {
	keys, err := c.activeCustody()
	if err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := requireConnectorAccessToken(credential); err != nil {
		return err
	}
	custody, err := c.readConnectorCustody(ctx, ref.ConnectionUID)
	if err != nil {
		return err
	}
	switch {
	case custody.Tombstoned:
		return store.ErrConnectorCustodyTombstoned
	case custody.Credential == nil:
		return store.ErrNotFound
	case custody.Credential.Version != expectedVersion:
		return store.ErrConflict
	}
	previous, err := openStoredConnectorCredential(keys, ref, custody.Credential)
	if err != nil {
		return fmt.Errorf("open replaced connector credential: %w", err)
	}
	credential.GrantSequence = custody.Credential.GrantSequence
	sealed, err := sealConnectorCredential(keys, ref, credential)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = c.ReplaceSealedConnectorCredential(ctx, storagev1.ReplaceSealedConnectorCredentialRequest{
		Credential: sealed, ExpectedVersion: expectedVersion,
		Retire: connectorRetirementFor(previous, credential, now, false), Now: now,
	})
	return err
}

func (c *Client) RetireConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	keys, err := c.activeCustody()
	if err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := requireConnectorAccessToken(credential); err != nil {
		return err
	}
	now := time.Now().UTC()
	var revocableUntil time.Time
	if credential.RefreshToken == "" && !credential.ExpiresAt.IsZero() {
		if !credential.ExpiresAt.After(now) {
			return nil
		}
		revocableUntil = credential.ExpiresAt.UTC()
	}
	sealed, err := sealConnectorCredential(keys, ref, credential)
	if err != nil {
		return err
	}
	return c.RetireSealedConnectorCredential(ctx, storagev1.SealedRetiredConnectorCredential{
		ConnectionUID: ref.ConnectionUID, DataKeyNonce: sealed.DataKeyNonce, DataKeyCiphertext: sealed.DataKeyCiphertext,
		Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, RevocableUntil: revocableUntil, RetiredAt: now,
	})
}

func (c *Client) CreateConnectorConsent(ctx context.Context, consent store.ConnectorConsent) error {
	keys, err := c.activeCustody()
	if err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"nonce", consent.Nonce}, {"connection UID", consent.ConnectionUID}, {store.ConnectorFieldNamespace, consent.Namespace},
		{"name", consent.Name}, {"subject digest", consent.SubjectDigest}, {"provider", consent.Provider},
		{"mode", consent.Mode}, {"code verifier", consent.CodeVerifier},
	} {
		if strings.TrimSpace(field.value) == "" {
			return errors.New("connector consent " + field.name + " is required")
		}
	}
	if consent.ExpiresAt.IsZero() {
		return errors.New("connector consent expiry is required")
	}
	for range connectorCommitAttempts {
		custody, err := c.readConnectorCustody(ctx, consent.ConnectionUID)
		if err != nil {
			return err
		}
		sealed := storagev1.SealedConnectorConsent{
			Nonce: consent.Nonce, ConnectionUID: consent.ConnectionUID, Namespace: consent.Namespace, Name: consent.Name,
			SubjectDigest: consent.SubjectDigest, Provider: consent.Provider, Mode: consent.Mode,
			AuthorityDigest: consent.AuthorityDigest, RevocationDigest: consent.RevocationDigest, Scopes: consent.Scopes,
			Sequence: custody.IssuedConsentSequence + 1, ExpiresAt: consent.ExpiresAt.UTC(),
		}
		if sealed.VerifierNonce, sealed.VerifierCiphertext, err = keys.seal(connectorConsentAdditionalData(sealed), []byte(consent.CodeVerifier)); err != nil {
			return fmt.Errorf("seal connector consent verifier: %w", err)
		}
		err = c.CreateSealedConnectorConsent(ctx, storagev1.CreateSealedConnectorConsentRequest{Consent: sealed, Now: time.Now().UTC()})
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return errConnectorCommitContended("store connector consent")
}

func (c *Client) ConsumeConnectorConsent(ctx context.Context, nonce string) (store.ConnectorConsent, error) {
	keys, err := c.activeCustody()
	if err != nil {
		return store.ConnectorConsent{}, err
	}
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorConsent{}, store.ErrNotFound
	}
	now := time.Now().UTC()
	sealed, err := c.ConsumeSealedConnectorConsent(ctx, nonce, now)
	if err != nil {
		return store.ConnectorConsent{}, err
	}
	if sealed == nil || sealed.Nonce != nonce {
		return store.ConnectorConsent{}, errors.New("connector consent provider returned another consent")
	}
	if !sealed.ExpiresAt.After(time.Now()) {
		return store.ConnectorConsent{}, store.ErrNotFound
	}
	verifier, err := keys.open(sealed.VerifierNonce, sealed.VerifierCiphertext, connectorConsentAdditionalData(*sealed))
	if err != nil {
		return store.ConnectorConsent{}, fmt.Errorf("open connector consent verifier: %w", err)
	}
	return store.ConnectorConsent{
		Nonce: sealed.Nonce, ConnectionUID: sealed.ConnectionUID, Namespace: sealed.Namespace, Name: sealed.Name,
		SubjectDigest: sealed.SubjectDigest, Provider: sealed.Provider, Mode: sealed.Mode, CodeVerifier: string(verifier),
		AuthorityDigest: sealed.AuthorityDigest, RevocationDigest: sealed.RevocationDigest, Scopes: sealed.Scopes,
		ExpiresAt: sealed.ExpiresAt.UTC(), Sequence: sealed.Sequence,
	}, nil
}

func (c *Client) CreateConnectorCompletion(ctx context.Context, completion store.ConnectorCompletion) error {
	keys, err := c.activeCustody()
	if err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"nonce", completion.Nonce}, {"connection UID", completion.ConnectionUID}, {store.ConnectorFieldNamespace, completion.Namespace},
		{"name", completion.Name}, {"subject digest", completion.SubjectDigest}, {"provider", completion.Provider},
		{"mode", completion.Mode}, {"access token", completion.Credential.AccessToken},
	} {
		if strings.TrimSpace(field.value) == "" {
			return errors.New("connector completion " + field.name + " is required")
		}
	}
	if completion.ExpiresAt.IsZero() {
		return errors.New("connector completion expiry is required")
	}
	body, err := encodeConnectorCompletionPayload(completion.Credential, connectorCompletionFields{ConsentAuthorityDigest: completion.ConsentAuthorityDigest})
	if err != nil {
		return err
	}
	sealed := storagev1.SealedConnectorCompletion{
		Nonce: completion.Nonce, ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name,
		SubjectDigest: completion.SubjectDigest, Provider: completion.Provider, Mode: completion.Mode,
		ConsentSequence: completion.ConsentSequence, ExpiresAt: completion.ExpiresAt.UTC(),
	}
	if sealed.PayloadNonce, sealed.Payload, err = keys.seal(connectorCompletionAdditionalData(sealed), body); err != nil {
		return fmt.Errorf("seal connector completion: %w", err)
	}
	for range connectorCommitAttempts {
		custody, err := c.readConnectorCustody(ctx, completion.ConnectionUID)
		if err != nil {
			return err
		}
		if custody.Tombstoned {
			return store.ErrConnectorCustodyTombstoned
		}
		if completion.ConsentSequence <= custody.CommittedConsentSequence {
			return store.ErrConnectorConsentSuperseded
		}
		expected, uncommitted := judgeConnectorCompletions(keys, custody.Completions)
		replace := make([]string, 0, len(uncommitted))
		for _, other := range uncommitted {
			if other.ConsentSequence >= completion.ConsentSequence {
				return store.ErrConnectorConsentSuperseded
			}
			replace = append(replace, other.Nonce)
		}
		err = c.CreateSealedConnectorCompletion(ctx, storagev1.CreateSealedConnectorCompletionRequest{
			Completion: sealed, ExpectedCompletions: expected, Replace: replace, Now: time.Now().UTC(),
		})
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return errConnectorCommitContended("park connector completion")
}

func (c *Client) ConsumeConnectorCompletion(ctx context.Context, nonce string) (store.ConnectorCompletion, error) {
	keys, err := c.activeCustody()
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	sealed, err := c.ConsumeSealedConnectorCompletion(ctx, nonce)
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	if sealed == nil || sealed.Nonce != nonce {
		return store.ConnectorCompletion{}, errors.New("connector completion provider returned another completion")
	}
	if !sealed.ExpiresAt.After(time.Now()) {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	completion, err := openConnectorCompletion(keys, *sealed)
	if err != nil {
		return store.ConnectorCompletion{}, fmt.Errorf("open connector completion: %w", err)
	}
	return completion, nil
}

func (c *Client) PeekConnectorCompletion(ctx context.Context, nonce string) (store.ConnectorCompletion, error) {
	keys, err := c.activeCustody()
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	sealed, err := c.GetSealedConnectorCompletion(ctx, nonce)
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	if sealed == nil || sealed.Nonce != nonce || !sealed.ExpiresAt.After(time.Now()) {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	completion, err := openConnectorCompletion(keys, *sealed)
	if err != nil {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	return completion, nil
}

func (c *Client) ListConnectorCompletionsForConnection(ctx context.Context, connectionUID string) ([]store.ConnectorCompletion, error) {
	keys, err := c.activeCustody()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(connectionUID) == "" {
		return nil, errors.New("connector completion connection UID is required")
	}
	custody, err := c.readConnectorCustody(ctx, connectionUID)
	if err != nil {
		return nil, err
	}
	var result []store.ConnectorCompletion
	for _, sealed := range custody.Completions {
		completion, err := openConnectorCompletion(keys, sealed)
		if err != nil {
			continue
		}
		result = append(result, completion)
	}
	return result, nil
}

func (c *Client) CommitConnectorCompletion(ctx context.Context, nonce string, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) (store.ConnectorCredential, error) {
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorCredential{}, errors.New("connector completion nonce is required")
	}
	keys, err := c.activeCustody()
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	if err := ref.Validate(); err != nil {
		return store.ConnectorCredential{}, err
	}
	if err := requireConnectorAccessToken(credential); err != nil {
		return store.ConnectorCredential{}, err
	}
	for range connectorCommitAttempts {
		request, err := c.prepareConnectorCommit(ctx, keys, nonce, ref, credential)
		if err != nil {
			return store.ConnectorCredential{}, err
		}
		request.command.Now = time.Now().UTC()
		stored, err := c.CommitSealedConnectorCompletion(ctx, request.command)
		switch {
		case errors.Is(err, store.ErrConflict):
			continue
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConnectorConsentSuperseded):
			_ = c.DeleteConnectorCompletion(ctx, nonce)
			return store.ConnectorCredential{}, err
		case err != nil:
			return store.ConnectorCredential{}, err
		}
		if stored == nil || stored.ConnectionUID != ref.ConnectionUID || stored.GrantSequence != request.committed.GrantSequence {
			return store.ConnectorCredential{}, fmt.Errorf("connector custody provider returned another record for connection %s", ref.ConnectionUID)
		}
		committed := request.committed
		committed.Version = stored.Version
		committed.UpdatedAt = stored.UpdatedAt.UTC()
		return committed, nil
	}
	return store.ConnectorCredential{}, errConnectorCommitContended("commit connector completion")
}

type preparedConnectorCommit struct {
	command   storagev1.CommitSealedConnectorCompletionRequest
	committed store.ConnectorCredential
}

func (c *Client) prepareConnectorCommit(
	ctx context.Context, keys *keyCustody, nonce string, ref store.ConnectorCredentialRef, credential store.ConnectorCredential,
) (preparedConnectorCommit, error) {
	now := time.Now().UTC()
	custody, err := c.readConnectorCustody(ctx, ref.ConnectionUID)
	if err != nil {
		return preparedConnectorCommit{}, err
	}
	var parked *storagev1.SealedConnectorCompletion
	for i := range custody.Completions {
		if custody.Completions[i].Nonce == nonce {
			parked = &custody.Completions[i]
			break
		}
	}
	if parked == nil {
		return preparedConnectorCommit{}, store.ErrNotFound
	}
	if !parked.ExpiresAt.After(now) {
		_ = c.DeleteConnectorCompletion(ctx, nonce)
		return preparedConnectorCommit{}, store.ErrNotFound
	}
	body, err := keys.open(parked.PayloadNonce, parked.Payload, connectorCompletionAdditionalData(*parked))
	if err != nil {
		return preparedConnectorCommit{}, fmt.Errorf("open connector completion for commit: %w", err)
	}
	_, fields, err := decodeConnectorCompletionPayload(body)
	if err != nil {
		return preparedConnectorCommit{}, err
	}
	if fields.Committed {
		return preparedConnectorCommit{}, store.ErrConnectorCompletionCommitted
	}
	if parked.ConsentSequence <= custody.CommittedConsentSequence {
		_ = c.DeleteConnectorCompletion(ctx, nonce)
		return preparedConnectorCommit{}, store.ErrConnectorConsentSuperseded
	}
	command := storagev1.CommitSealedConnectorCompletionRequest{Nonce: nonce, Now: now}
	if current := custody.Credential; current != nil {
		previous, err := openStoredConnectorCredential(keys, ref, current)
		if err != nil {
			return preparedConnectorCommit{}, fmt.Errorf("open replaced connector credential: %w", err)
		}
		command.ExpectedCredentialVersion = current.Version
		command.Retire = connectorRetirementFor(previous, credential, now, true)
	}
	credential.GrantSequence = custody.LastGrantSequence + 1
	if command.Credential, err = sealConnectorCredential(keys, ref, credential); err != nil {
		return preparedConnectorCommit{}, err
	}
	fields.Committed = true
	committedBody, err := encodeConnectorCompletionPayload(credential, fields)
	if err != nil {
		return preparedConnectorCommit{}, err
	}
	if command.PayloadNonce, command.Payload, err = keys.seal(connectorCompletionAdditionalData(*parked), committedBody); err != nil {
		return preparedConnectorCommit{}, fmt.Errorf("seal connector completion: %w", err)
	}
	expected, uncommitted := judgeConnectorCompletions(keys, custody.Completions)
	command.ExpectedCompletions = expected
	for _, other := range uncommitted {
		if other.ConsentSequence < parked.ConsentSequence {
			command.Drop = append(command.Drop, other.Nonce)
		}
	}
	return preparedConnectorCommit{command: command, committed: credential}, nil
}

func (c *Client) verifyRetainedConnectorCustody(ctx context.Context, keys *keyCustody) error {
	if err := pageConnectorRecords(func(cursor string) (string, error) {
		page, err := c.ListSealedConnectorCredentials(ctx, cursor, connectorVerificationPageSize)
		if err != nil {
			return "", err
		}
		for _, sealed := range page.Credentials {
			if err := verifyConnectorDataKey(keys, sealed.ConnectionUID, sealed.DataKeyNonce, sealed.DataKeyCiphertext, "current custody"); err != nil {
				return "", err
			}
		}
		return page.NextCursor, nil
	}); err != nil {
		return err
	}
	if err := pageConnectorRecords(func(cursor string) (string, error) {
		page, err := c.ListSealedRetiredConnectorCredentials(ctx, cursor, connectorVerificationPageSize)
		if err != nil {
			return "", err
		}
		for _, sealed := range page.Retired {
			if err := verifyConnectorDataKey(keys, sealed.ConnectionUID, sealed.DataKeyNonce, sealed.DataKeyCiphertext, "retired custody"); err != nil {
				return "", err
			}
		}
		return page.NextCursor, nil
	}); err != nil {
		return err
	}
	now := time.Now()
	if err := pageConnectorRecords(func(cursor string) (string, error) {
		page, err := c.ListSealedConnectorConsents(ctx, cursor, connectorVerificationPageSize)
		if err != nil {
			return "", err
		}
		for _, sealed := range page.Consents {
			if !sealed.ExpiresAt.After(now) {
				continue
			}
			if _, err := keys.open(sealed.VerifierNonce, sealed.VerifierCiphertext, connectorConsentAdditionalData(sealed)); err != nil {
				return "", fmt.Errorf("candidate agent execution snapshot key cannot open pending connector consent for connection %s; restore the previous key: %w", sealed.ConnectionUID, err)
			}
		}
		return page.NextCursor, nil
	}); err != nil {
		return err
	}
	return pageConnectorRecords(func(cursor string) (string, error) {
		page, err := c.ListSealedConnectorCompletions(ctx, cursor, connectorVerificationPageSize)
		if err != nil {
			return "", err
		}
		for _, sealed := range page.Completions {
			if _, err := keys.open(sealed.PayloadNonce, sealed.Payload, connectorCompletionAdditionalData(sealed)); err != nil {
				return "", fmt.Errorf("candidate agent execution snapshot key cannot open parked connector completion for connection %s; restore the previous key: %w", sealed.ConnectionUID, err)
			}
		}
		return page.NextCursor, nil
	})
}

func verifyConnectorDataKey(keys *keyCustody, connectionUID string, nonce, ciphertext []byte, family string) error {
	dataKey, err := keys.open(nonce, ciphertext, connectorDataKeyAdditionalData(connectionUID))
	if err != nil {
		return fmt.Errorf("candidate agent execution snapshot key cannot open connector custody for connection %s (%s); "+
			"restore the previous key: linked accounts would be unusable and unrevocable: %w", connectionUID, family, err)
	}
	clear(dataKey)
	return nil
}

func pageConnectorRecords(visit func(cursor string) (string, error)) error {
	cursor := ""
	for {
		next, err := visit(cursor)
		if err != nil {
			return fmt.Errorf("verify connector custody key: %w", err)
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}
