/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// connectorSchemaStatements defines per-Connection sealed credentials and
// pending OAuth consents. Timestamps are stored as the driver's native time.
func connectorSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS connector_credentials (
			connection_uid  TEXT PRIMARY KEY,
			namespace       TEXT NOT NULL,
			name            TEXT NOT NULL,
			subject_digest  TEXT NOT NULL,
			provider        TEXT NOT NULL,
			dek_nonce       BLOB NOT NULL,
			dek_ciphertext  BLOB NOT NULL,
			nonce           BLOB NOT NULL,
			ciphertext      BLOB NOT NULL,
			expires_at      TIMESTAMP,
			created_at      TIMESTAMP NOT NULL,
			updated_at      TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_connector_credentials_subject
			ON connector_credentials(namespace, subject_digest, provider)`,
		`CREATE TABLE IF NOT EXISTS connector_consents (
			nonce               TEXT PRIMARY KEY,
			connection_uid      TEXT NOT NULL,
			namespace           TEXT NOT NULL,
			name                TEXT NOT NULL,
			subject_digest      TEXT NOT NULL,
			provider            TEXT NOT NULL,
			mode                TEXT NOT NULL,
			verifier_nonce      BLOB NOT NULL,
			verifier_ciphertext BLOB NOT NULL,
			authority_digest    TEXT NOT NULL DEFAULT '',
			expires_at          TIMESTAMP NOT NULL,
			created_at          TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_connector_consents_connection
			ON connector_consents(connection_uid)`,
		`CREATE TABLE IF NOT EXISTS connector_completions (
			nonce           TEXT PRIMARY KEY,
			connection_uid  TEXT NOT NULL,
			namespace       TEXT NOT NULL,
			name            TEXT NOT NULL,
			subject_digest  TEXT NOT NULL,
			provider        TEXT NOT NULL,
			mode            TEXT NOT NULL,
			payload_nonce   BLOB NOT NULL,
			payload         BLOB NOT NULL,
			expires_at      TIMESTAMP NOT NULL,
			created_at      TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_connector_completions_connection
			ON connector_completions(connection_uid)`,
		`CREATE TABLE IF NOT EXISTS connector_credential_tombstones (
			connection_uid  TEXT PRIMARY KEY,
			deleted_at      TIMESTAMP NOT NULL
		)`,
	}
}

var errConnectorCipherRequired = errors.New("connector credential encryption is not configured; connector custody fails closed")

const connectorDataKeyBytes = 32

// sealedConnectorCredential is the JSON body sealed under the Connection's
// data key. Field names are part of the stored format.
type sealedConnectorCredential struct {
	AccessToken     string   `json:"accessToken"`
	RefreshToken    string   `json:"refreshToken,omitempty"`
	TokenType       string   `json:"tokenType,omitempty"`
	ExpiresAt       string   `json:"expiresAt,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	AuthorityDigest string   `json:"authorityDigest,omitempty"`
}

func connectorDataKeyAdditionalData(connectionUID string) []byte {
	return fmt.Appendf(nil, "orka.connector-data-key\x00%s", connectionUID)
}

func connectorCredentialAdditionalData(ref store.ConnectorCredentialRef) []byte {
	return fmt.Appendf(nil, "orka.connector-credential\x00%s\x00%s\x00%s\x00%s\x00%s",
		ref.ConnectionUID, ref.Namespace, ref.Name, ref.SubjectDigest, ref.Provider)
}

// connectorConsentAdditionalData binds the sealed verifier to every plaintext
// column the callback fence reads, including the OAuth-authority digest, so
// an altered row cannot steer the code exchange to a different endpoint.
func connectorConsentAdditionalData(consent store.ConnectorConsent) []byte {
	return fmt.Appendf(nil, "orka.connector-consent\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		consent.Nonce, consent.ConnectionUID, consent.Namespace, consent.Name, consent.SubjectDigest,
		consent.Provider, consent.Mode, consent.AuthorityDigest, consent.ExpiresAt.UTC().Unix())
}

// connectorCompletionAdditionalData binds the sealed payload to every
// plaintext column the completion fence reads, so a row whose mode, name,
// provider, or expiry was altered no longer opens.
func connectorCompletionAdditionalData(completion store.ConnectorCompletion) []byte {
	return fmt.Appendf(nil, "orka.connector-completion\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		completion.Nonce, completion.ConnectionUID, completion.SubjectDigest,
		completion.Namespace, completion.Name, completion.Provider, completion.Mode, completion.ExpiresAt.UTC().Unix())
}

func encodeSealedConnectorCredential(credential store.ConnectorCredential) ([]byte, error) {
	body, err := json.Marshal(sealedConnectorCredential{
		AccessToken:     credential.AccessToken,
		RefreshToken:    credential.RefreshToken,
		TokenType:       credential.TokenType,
		ExpiresAt:       formatConnectorTime(credential.ExpiresAt),
		Scopes:          credential.Scopes,
		AuthorityDigest: credential.AuthorityDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("encode connector credential: %w", err)
	}
	return body, nil
}

func decodeSealedConnectorCredential(body []byte) (store.ConnectorCredential, error) {
	var sealed sealedConnectorCredential
	if err := json.Unmarshal(body, &sealed); err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("decode connector credential: %w", err)
	}
	expiresAt, err := parseConnectorTime(sealed.ExpiresAt)
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("decode connector credential expiry: %w", err)
	}
	return store.ConnectorCredential{
		AccessToken:     sealed.AccessToken,
		RefreshToken:    sealed.RefreshToken,
		TokenType:       sealed.TokenType,
		ExpiresAt:       expiresAt,
		Scopes:          sealed.Scopes,
		AuthorityDigest: sealed.AuthorityDigest,
	}, nil
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

func sealWithAEAD(aead cipher.AEAD, additionalData, body []byte) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, body, additionalData), nil
}

// PutConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) PutConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	if s.snapshotCipher == nil {
		return errConnectorCipherRequired
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return errors.New("connector credential access token is required")
	}
	body, err := encodeSealedConnectorCredential(credential)
	if err != nil {
		return err
	}
	dataKey := make([]byte, connectorDataKeyBytes)
	if _, err := rand.Read(dataKey); err != nil {
		return fmt.Errorf("generate connector data key: %w", err)
	}
	dataAEAD, err := newConnectorDataKeyAEAD(dataKey)
	if err != nil {
		return err
	}
	nonce, ciphertext, err := sealWithAEAD(dataAEAD, connectorCredentialAdditionalData(ref), body)
	if err != nil {
		return err
	}
	dekNonce, dekCiphertext, err := sealWithAEAD(s.snapshotCipher.aead, connectorDataKeyAdditionalData(ref.ConnectionUID), dataKey)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var expiresAt any
	if !credential.ExpiresAt.IsZero() {
		expiresAt = credential.ExpiresAt.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector credential transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var tombstoned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_credential_tombstones WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&tombstoned); err != nil {
		return fmt.Errorf("check connector custody tombstone: %w", err)
	}
	if tombstoned > 0 {
		return store.ErrConnectorCustodyTombstoned
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO connector_credentials
		(connection_uid, namespace, name, subject_digest, provider, dek_nonce, dek_ciphertext, nonce, ciphertext, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(connection_uid) DO UPDATE SET
			namespace = excluded.namespace,
			name = excluded.name,
			subject_digest = excluded.subject_digest,
			provider = excluded.provider,
			dek_nonce = excluded.dek_nonce,
			dek_ciphertext = excluded.dek_ciphertext,
			nonce = excluded.nonce,
			ciphertext = excluded.ciphertext,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at`,
		ref.ConnectionUID, ref.Namespace, ref.Name, ref.SubjectDigest, ref.Provider,
		dekNonce, dekCiphertext, nonce, ciphertext, expiresAt, now, now)
	if err != nil {
		return fmt.Errorf("persist connector credential: %w", err)
	}
	return tx.Commit()
}

// GetConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) GetConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef) (store.ConnectorCredential, error) {
	if s.snapshotCipher == nil {
		return store.ConnectorCredential{}, errConnectorCipherRequired
	}
	if err := ref.Validate(); err != nil {
		return store.ConnectorCredential{}, err
	}
	var (
		dekNonce, dekCiphertext, nonce, ciphertext []byte
		updatedAt                                  time.Time
	)
	err := s.db.QueryRowContext(ctx, `SELECT dek_nonce, dek_ciphertext, nonce, ciphertext, updated_at
		FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).
		Scan(&dekNonce, &dekCiphertext, &nonce, &ciphertext, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ConnectorCredential{}, store.ErrNotFound
	}
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("read connector credential: %w", err)
	}
	dataKey, err := s.snapshotCipher.aead.Open(nil, dekNonce, dekCiphertext, connectorDataKeyAdditionalData(ref.ConnectionUID))
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("unwrap connector data key for connection %s: %w", ref.ConnectionUID, err)
	}
	dataAEAD, err := newConnectorDataKeyAEAD(dataKey)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	body, err := dataAEAD.Open(nil, nonce, ciphertext, connectorCredentialAdditionalData(ref))
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("open connector credential for connection %s: binding mismatch or corrupt row: %w", ref.ConnectionUID, err)
	}
	credential, err := decodeSealedConnectorCredential(body)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	credential.UpdatedAt = updatedAt.UTC()
	return credential, nil
}

// DeleteConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) DeleteConnectorCredential(ctx context.Context, connectionUID string) error {
	if strings.TrimSpace(connectionUID) == "" {
		return errors.New("connector credential connection UID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector credential transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_credentials WHERE connection_uid = ?`, connectionUID); err != nil {
		return fmt.Errorf("delete connector credential: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_credential_tombstones (connection_uid, deleted_at) VALUES (?, ?)
		ON CONFLICT(connection_uid) DO NOTHING`, connectionUID, time.Now().UTC()); err != nil {
		return fmt.Errorf("tombstone connector custody: %w", err)
	}
	return tx.Commit()
}

// CreateConnectorConsent implements store.ConnectorConsentStore.
func (s *Store) CreateConnectorConsent(ctx context.Context, consent store.ConnectorConsent) error {
	if s.snapshotCipher == nil {
		return errConnectorCipherRequired
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
	verifierNonce, verifierCiphertext, err := sealWithAEAD(s.snapshotCipher.aead,
		connectorConsentAdditionalData(consent), []byte(consent.CodeVerifier))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector consent transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE expires_at < ?`, now); err != nil {
		return fmt.Errorf("purge expired connector consents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_consents
		(nonce, connection_uid, namespace, name, subject_digest, provider, mode, verifier_nonce, verifier_ciphertext, authority_digest, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		consent.Nonce, consent.ConnectionUID, consent.Namespace, consent.Name, consent.SubjectDigest, consent.Provider,
		consent.Mode, verifierNonce, verifierCiphertext, consent.AuthorityDigest, consent.ExpiresAt.UTC(), now); err != nil {
		return fmt.Errorf("persist connector consent: %w", err)
	}
	return tx.Commit()
}

// ConsumeConnectorConsent implements store.ConnectorConsentStore.
func (s *Store) ConsumeConnectorConsent(ctx context.Context, nonce string) (store.ConnectorConsent, error) {
	if s.snapshotCipher == nil {
		return store.ConnectorConsent{}, errConnectorCipherRequired
	}
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorConsent{}, store.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ConnectorConsent{}, fmt.Errorf("begin connector consent transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE expires_at < ?`, time.Now().UTC()); err != nil {
		return store.ConnectorConsent{}, fmt.Errorf("purge expired connector consents: %w", err)
	}
	var (
		consent                           store.ConnectorConsent
		verifierNonce, verifierCiphertext []byte
	)
	err = tx.QueryRowContext(ctx, `SELECT nonce, connection_uid, namespace, name, subject_digest, provider, mode,
		verifier_nonce, verifier_ciphertext, authority_digest, expires_at FROM connector_consents WHERE nonce = ?`, nonce).
		Scan(&consent.Nonce, &consent.ConnectionUID, &consent.Namespace, &consent.Name, &consent.SubjectDigest,
			&consent.Provider, &consent.Mode, &verifierNonce, &verifierCiphertext, &consent.AuthorityDigest, &consent.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ConnectorConsent{}, store.ErrNotFound
	}
	if err != nil {
		return store.ConnectorConsent{}, fmt.Errorf("read connector consent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE nonce = ?`, nonce); err != nil {
		return store.ConnectorConsent{}, fmt.Errorf("consume connector consent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.ConnectorConsent{}, fmt.Errorf("commit connector consent: %w", err)
	}
	consent.ExpiresAt = consent.ExpiresAt.UTC()
	if !consent.ExpiresAt.After(time.Now()) {
		return store.ConnectorConsent{}, store.ErrNotFound
	}
	verifier, err := s.snapshotCipher.aead.Open(nil, verifierNonce, verifierCiphertext,
		connectorConsentAdditionalData(consent))
	if err != nil {
		return store.ConnectorConsent{}, fmt.Errorf("open connector consent verifier: %w", err)
	}
	consent.CodeVerifier = string(verifier)
	return consent, nil
}

// CreateConnectorCompletion implements store.ConnectorConsentStore.
func (s *Store) CreateConnectorCompletion(ctx context.Context, completion store.ConnectorCompletion) error {
	if s.snapshotCipher == nil {
		return errConnectorCipherRequired
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
	body, err := encodeSealedConnectorCredential(completion.Credential)
	if err != nil {
		return err
	}
	payloadNonce, payload, err := sealWithAEAD(s.snapshotCipher.aead,
		connectorCompletionAdditionalData(completion), body)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector completion transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Expired completions are not purged here: they may hold live provider
	// tokens that only the Connection reconciler can revoke before deletion.
	// A callback that was mid-exchange while the Connection was disconnected
	// must not park tokens for the tombstoned UID.
	var tombstoned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_credential_tombstones WHERE connection_uid = ?`, completion.ConnectionUID).Scan(&tombstoned); err != nil {
		return fmt.Errorf("check connector custody tombstone: %w", err)
	}
	if tombstoned > 0 {
		return store.ErrConnectorCustodyTombstoned
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_completions
		(nonce, connection_uid, namespace, name, subject_digest, provider, mode, payload_nonce, payload, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		completion.Nonce, completion.ConnectionUID, completion.Namespace, completion.Name, completion.SubjectDigest,
		completion.Provider, completion.Mode, payloadNonce, payload, completion.ExpiresAt.UTC(), now); err != nil {
		return fmt.Errorf("persist connector completion: %w", err)
	}
	return tx.Commit()
}

// ConsumeConnectorCompletion implements store.ConnectorConsentStore.
func (s *Store) ConsumeConnectorCompletion(ctx context.Context, nonce string) (store.ConnectorCompletion, error) {
	if s.snapshotCipher == nil {
		return store.ConnectorCompletion{}, errConnectorCipherRequired
	}
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ConnectorCompletion{}, fmt.Errorf("begin connector completion transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		completion            store.ConnectorCompletion
		payloadNonce, payload []byte
	)
	err = tx.QueryRowContext(ctx, `SELECT nonce, connection_uid, namespace, name, subject_digest, provider, mode,
		payload_nonce, payload, expires_at FROM connector_completions WHERE nonce = ?`, nonce).
		Scan(&completion.Nonce, &completion.ConnectionUID, &completion.Namespace, &completion.Name, &completion.SubjectDigest,
			&completion.Provider, &completion.Mode, &payloadNonce, &payload, &completion.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	if err != nil {
		return store.ConnectorCompletion{}, fmt.Errorf("read connector completion: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_completions WHERE nonce = ?`, nonce); err != nil {
		return store.ConnectorCompletion{}, fmt.Errorf("consume connector completion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.ConnectorCompletion{}, fmt.Errorf("commit connector completion: %w", err)
	}
	completion.ExpiresAt = completion.ExpiresAt.UTC()
	if !completion.ExpiresAt.After(time.Now()) {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	body, err := s.snapshotCipher.aead.Open(nil, payloadNonce, payload,
		connectorCompletionAdditionalData(completion))
	if err != nil {
		return store.ConnectorCompletion{}, fmt.Errorf("open connector completion: %w", err)
	}
	credential, err := decodeSealedConnectorCredential(body)
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	completion.Credential = credential
	return completion, nil
}

// PeekConnectorCompletion implements store.ConnectorConsentStore.
func (s *Store) PeekConnectorCompletion(ctx context.Context, nonce string) (store.ConnectorCompletion, error) {
	if s.snapshotCipher == nil {
		return store.ConnectorCompletion{}, errConnectorCipherRequired
	}
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	rows, err := s.queryConnectorCompletions(ctx, `WHERE nonce = ?`, nonce, false)
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	if len(rows) == 0 {
		return store.ConnectorCompletion{}, store.ErrNotFound
	}
	return rows[0], nil
}

// DeleteConnectorCompletion implements store.ConnectorConsentStore.
func (s *Store) DeleteConnectorCompletion(ctx context.Context, nonce string) error {
	if strings.TrimSpace(nonce) == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM connector_completions WHERE nonce = ?`, nonce); err != nil {
		return fmt.Errorf("delete connector completion: %w", err)
	}
	return nil
}

// ListConnectorCompletionsForConnection implements store.ConnectorConsentStore.
func (s *Store) ListConnectorCompletionsForConnection(ctx context.Context, connectionUID string) ([]store.ConnectorCompletion, error) {
	if s.snapshotCipher == nil {
		return nil, errConnectorCipherRequired
	}
	if strings.TrimSpace(connectionUID) == "" {
		return nil, errors.New("connector completion connection UID is required")
	}
	// Expired rows are included: their tokens may still be live upstream and
	// disconnect must revoke them before the rows are dropped.
	return s.queryConnectorCompletions(ctx, `WHERE connection_uid = ?`, connectionUID, true)
}

// queryConnectorCompletions reads and opens completions matching the clause.
// Redemption paths exclude expired rows; revocation paths include them. Rows
// that fail to open are skipped rather than returned.
func (s *Store) queryConnectorCompletions(ctx context.Context, where string, arg any, includeExpired bool) ([]store.ConnectorCompletion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT nonce, connection_uid, namespace, name, subject_digest, provider, mode,
		payload_nonce, payload, expires_at FROM connector_completions `+where, arg)
	if err != nil {
		return nil, fmt.Errorf("read connector completions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	now := time.Now()
	var result []store.ConnectorCompletion
	for rows.Next() {
		var (
			completion            store.ConnectorCompletion
			payloadNonce, payload []byte
		)
		if err := rows.Scan(&completion.Nonce, &completion.ConnectionUID, &completion.Namespace, &completion.Name, &completion.SubjectDigest,
			&completion.Provider, &completion.Mode, &payloadNonce, &payload, &completion.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan connector completion: %w", err)
		}
		completion.ExpiresAt = completion.ExpiresAt.UTC()
		if !includeExpired && !completion.ExpiresAt.After(now) {
			continue
		}
		body, err := s.snapshotCipher.aead.Open(nil, payloadNonce, payload,
			connectorCompletionAdditionalData(completion))
		if err != nil {
			continue
		}
		credential, err := decodeSealedConnectorCredential(body)
		if err != nil {
			continue
		}
		completion.Credential = credential
		result = append(result, completion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connector completions: %w", err)
	}
	return result, nil
}

// DeleteConnectorConsentsForConnection implements store.ConnectorConsentStore.
func (s *Store) DeleteConnectorConsentsForConnection(ctx context.Context, connectionUID string) error {
	if strings.TrimSpace(connectionUID) == "" {
		return errors.New("connector consent connection UID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector consent transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE connection_uid = ?`, connectionUID); err != nil {
		return fmt.Errorf("delete connector consents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_completions WHERE connection_uid = ?`, connectionUID); err != nil {
		return fmt.Errorf("delete connector completions: %w", err)
	}
	return tx.Commit()
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
