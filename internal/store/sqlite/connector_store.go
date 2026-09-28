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
			version         INTEGER NOT NULL DEFAULT 1,
			grant_sequence  INTEGER NOT NULL DEFAULT 0,
			created_at      TIMESTAMP NOT NULL,
			updated_at      TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_connector_credentials_subject
			ON connector_credentials(namespace, subject_digest, provider)`,
		`CREATE TABLE IF NOT EXISTS connector_credential_grants (
			connection_uid  TEXT PRIMARY KEY,
			grant_sequence  INTEGER NOT NULL
		)`,
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
			revocation_digest   TEXT NOT NULL DEFAULT '',
			scopes              TEXT NOT NULL DEFAULT '',
			expires_at          TIMESTAMP NOT NULL,
			created_at          TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_connector_consents_connection
			ON connector_consents(connection_uid)`,
		`CREATE TABLE IF NOT EXISTS connector_retired_credentials (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			connection_uid TEXT NOT NULL,
			dek_nonce      BLOB NOT NULL,
			dek_ciphertext BLOB NOT NULL,
			nonce          BLOB NOT NULL,
			ciphertext     BLOB NOT NULL,
			revocable_until TIMESTAMP,
			retired_at     TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_connector_retired_credentials_connection
			ON connector_retired_credentials(connection_uid)`,
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
		`CREATE TABLE IF NOT EXISTS connector_credential_versions (
			connection_uid  TEXT PRIMARY KEY,
			last_version    INTEGER NOT NULL
		)`,
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
	// RevocationDigest travels sealed so a retired row and a parked
	// completion keep the authority they can be revoked against.
	RevocationDigest string `json:"revocationDigest,omitempty"`
	// GrantSequence is also a plain column on custody rows (which wins on
	// read); sealed here so a committed completion carries its grant.
	GrantSequence int64 `json:"grantSequence,omitempty"`
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
	return fmt.Appendf(nil, "orka.connector-consent\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		consent.Nonce, consent.ConnectionUID, consent.Namespace, consent.Name, consent.SubjectDigest,
		consent.Provider, consent.Mode, consent.AuthorityDigest, consent.RevocationDigest, strings.Join(consent.Scopes, " "), consent.ExpiresAt.UTC().Unix())
}

// connectorCompletionAdditionalData binds the sealed payload to every
// plaintext column the completion fence reads, so a row whose mode, name,
// provider, or expiry was altered no longer opens.
func connectorCompletionAdditionalData(completion store.ConnectorCompletion) []byte {
	return fmt.Appendf(nil, "orka.connector-completion\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		completion.Nonce, completion.ConnectionUID, completion.SubjectDigest,
		completion.Namespace, completion.Name, completion.Provider, completion.Mode, completion.ExpiresAt.UTC().Unix())
}

// sealedConnectorCompletionPayload is a parked completion's sealed body: the
// credential plus the committed marker, which is authenticated by the seal
// so a plaintext column cannot flip a commit back into a fresh write.
type sealedConnectorCompletionPayload struct {
	sealedConnectorCredential
	ConsentAuthorityDigest string `json:"consentAuthorityDigest,omitempty"`
	Committed              bool   `json:"committed,omitempty"`
}

// sealedConnectorCompletionFields are the non-credential facts sealed with a
// parked completion.
type sealedConnectorCompletionFields struct {
	ConsentAuthorityDigest string
	Committed              bool
}

func encodeSealedConnectorCompletionPayload(credential store.ConnectorCredential, fields sealedConnectorCompletionFields) ([]byte, error) {
	body, err := json.Marshal(sealedConnectorCompletionPayload{
		sealedConnectorCredential: sealedConnectorCredential{
			AccessToken:     credential.AccessToken,
			RefreshToken:    credential.RefreshToken,
			TokenType:       credential.TokenType,
			ExpiresAt:       formatConnectorTime(credential.ExpiresAt),
			Scopes:          credential.Scopes,
			AuthorityDigest: credential.AuthorityDigest, RevocationDigest: credential.RevocationDigest, GrantSequence: credential.GrantSequence,
		},
		ConsentAuthorityDigest: fields.ConsentAuthorityDigest,
		Committed:              fields.Committed,
	})
	if err != nil {
		return nil, fmt.Errorf("encode connector completion: %w", err)
	}
	return body, nil
}

func decodeSealedConnectorCompletionPayload(body []byte) (store.ConnectorCredential, sealedConnectorCompletionFields, error) {
	var sealed sealedConnectorCompletionPayload
	if err := json.Unmarshal(body, &sealed); err != nil {
		return store.ConnectorCredential{}, sealedConnectorCompletionFields{}, fmt.Errorf("decode connector completion: %w", err)
	}
	inner, err := json.Marshal(sealed.sealedConnectorCredential)
	if err != nil {
		return store.ConnectorCredential{}, sealedConnectorCompletionFields{}, fmt.Errorf("decode connector completion: %w", err)
	}
	credential, err := decodeSealedConnectorCredential(inner)
	if err != nil {
		return store.ConnectorCredential{}, sealedConnectorCompletionFields{}, err
	}
	return credential, sealedConnectorCompletionFields{ConsentAuthorityDigest: sealed.ConsentAuthorityDigest, Committed: sealed.Committed}, nil
}

func encodeSealedConnectorCredential(credential store.ConnectorCredential) ([]byte, error) {
	body, err := json.Marshal(sealedConnectorCredential{
		AccessToken:      credential.AccessToken,
		RefreshToken:     credential.RefreshToken,
		TokenType:        credential.TokenType,
		ExpiresAt:        formatConnectorTime(credential.ExpiresAt),
		Scopes:           credential.Scopes,
		AuthorityDigest:  credential.AuthorityDigest,
		RevocationDigest: credential.RevocationDigest,
		GrantSequence:    credential.GrantSequence,
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

// sealedConnectorRow is one credential sealed under a fresh data key.
type sealedConnectorRow struct {
	dekNonce, dekCiphertext, nonce, ciphertext []byte
	expiresAt                                  any
}

// sealConnectorCredentialRow seals credential under a fresh per-row data key
// bound to ref, and wraps that key with the snapshot key.
func (s *Store) sealConnectorCredentialRow(ref store.ConnectorCredentialRef, credential store.ConnectorCredential) (sealedConnectorRow, error) {
	body, err := encodeSealedConnectorCredential(credential)
	if err != nil {
		return sealedConnectorRow{}, err
	}
	dataKey := make([]byte, connectorDataKeyBytes)
	if _, err := rand.Read(dataKey); err != nil {
		return sealedConnectorRow{}, fmt.Errorf("generate connector data key: %w", err)
	}
	dataAEAD, err := newConnectorDataKeyAEAD(dataKey)
	if err != nil {
		return sealedConnectorRow{}, err
	}
	row := sealedConnectorRow{}
	if row.nonce, row.ciphertext, err = sealWithAEAD(dataAEAD, connectorCredentialAdditionalData(ref), body); err != nil {
		return sealedConnectorRow{}, err
	}
	if row.dekNonce, row.dekCiphertext, err = sealWithAEAD(s.snapshotCipher.aead, connectorDataKeyAdditionalData(ref.ConnectionUID), dataKey); err != nil {
		return sealedConnectorRow{}, err
	}
	if !credential.ExpiresAt.IsZero() {
		row.expiresAt = credential.ExpiresAt.UTC()
	}
	return row, nil
}

// PutConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) PutConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	s.finishPendingWALTruncate(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector credential transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.putConnectorCredentialTx(ctx, tx, ref, credential); err != nil {
		return err
	}
	return tx.Commit()
}

// putConnectorCredentialTx seals and upserts custody inside tx.
// putConnectorCredentialTx seals and upserts a new grant's custody inside
// tx and returns the material as stored, with the grant sequence it took.
// Grants are numbered from a per-Connection counter that outlives the row,
// so a re-consent after a shred never repeats a number.
func (s *Store) putConnectorCredentialTx(ctx context.Context, tx *sql.Tx, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) (store.ConnectorCredential, error) {
	if s.snapshotCipher == nil {
		return store.ConnectorCredential{}, errConnectorCipherRequired
	}
	if err := ref.Validate(); err != nil {
		return store.ConnectorCredential{}, err
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return store.ConnectorCredential{}, errors.New("connector credential access token is required")
	}
	var tombstoned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_credential_tombstones WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&tombstoned); err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("check connector custody tombstone: %w", err)
	}
	if tombstoned > 0 {
		return store.ConnectorCredential{}, store.ErrConnectorCustodyTombstoned
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO connector_credential_grants (connection_uid, grant_sequence) VALUES (?, 1)
		ON CONFLICT(connection_uid) DO UPDATE SET grant_sequence = grant_sequence + 1
		RETURNING grant_sequence`, ref.ConnectionUID).Scan(&credential.GrantSequence); err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("assign connector grant sequence: %w", err)
	}
	row, err := s.sealConnectorCredentialRow(ref, credential)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	now := time.Now().UTC()
	// Versions are drawn from a per-Connection sequence that survives a
	// shred, so a row re-created after one never reuses a version number
	// that a stale fenced write or verdict could still be holding.
	next, err := nextConnectorCredentialVersion(ctx, tx, ref.ConnectionUID)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO connector_credentials
		(connection_uid, namespace, name, subject_digest, provider, dek_nonce, dek_ciphertext, nonce, ciphertext, expires_at, version, grant_sequence, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			version = excluded.version,
			grant_sequence = excluded.grant_sequence,
			updated_at = excluded.updated_at`,
		ref.ConnectionUID, ref.Namespace, ref.Name, ref.SubjectDigest, ref.Provider,
		row.dekNonce, row.dekCiphertext, row.nonce, row.ciphertext, row.expiresAt, next, credential.GrantSequence, now, now)
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("persist connector credential: %w", err)
	}
	credential.Version = next
	credential.UpdatedAt = now
	return credential, nil
}

// nextConnectorCredentialVersion advances and returns the Connection's
// version sequence inside tx.
func nextConnectorCredentialVersion(ctx context.Context, tx *sql.Tx, connectionUID string) (int64, error) {
	var next int64
	err := tx.QueryRowContext(ctx, `INSERT INTO connector_credential_versions (connection_uid, last_version) VALUES (?, 1)
		ON CONFLICT(connection_uid) DO UPDATE SET last_version = connector_credential_versions.last_version + 1
		RETURNING last_version`, connectionUID).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("advance connector credential version: %w", err)
	}
	return next, nil
}

// CommitConnectorCompletion implements store.ConnectorConsentStore.
func (s *Store) CommitConnectorCompletion(ctx context.Context, nonce string, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) (store.ConnectorCredential, error) {
	if strings.TrimSpace(nonce) == "" {
		return store.ConnectorCredential{}, errors.New("connector completion nonce is required")
	}
	if s.snapshotCipher == nil {
		return store.ConnectorCredential{}, errConnectorCipherRequired
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("begin connector completion commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The parked row is read and judged inside this transaction before
	// custody changes: a completion another API replica already committed
	// (the in-process completion lock does not span replicas) is never
	// committed twice, which would take another grant and replay material
	// a newer consent may have replaced.
	var (
		completion            store.ConnectorCompletion
		payloadNonce, payload []byte
	)
	err = tx.QueryRowContext(ctx, `SELECT nonce, connection_uid, namespace, name, subject_digest, provider, mode,
		payload_nonce, payload, expires_at FROM connector_completions WHERE nonce = ?`, nonce).
		Scan(&completion.Nonce, &completion.ConnectionUID, &completion.Namespace, &completion.Name, &completion.SubjectDigest,
			&completion.Provider, &completion.Mode, &payloadNonce, &payload, &completion.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ConnectorCredential{}, store.ErrNotFound
	}
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("read connector completion for commit: %w", err)
	}
	completion.ExpiresAt = completion.ExpiresAt.UTC()
	if !completion.ExpiresAt.After(time.Now().UTC()) {
		// The token's lifetime is judged here, not only at the peek: a
		// commit that stalled past the deadline redeems nothing.
		if _, err := tx.ExecContext(ctx, `DELETE FROM connector_completions WHERE nonce = ?`, nonce); err != nil {
			return store.ConnectorCredential{}, fmt.Errorf("drop expired connector completion: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return store.ConnectorCredential{}, err
		}
		return store.ConnectorCredential{}, store.ErrNotFound
	}
	current, err := s.snapshotCipher.aead.Open(nil, payloadNonce, payload, connectorCompletionAdditionalData(completion))
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("open connector completion for commit: %w", err)
	}
	_, fields, err := decodeSealedConnectorCompletionPayload(current)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	if fields.Committed {
		return store.ConnectorCredential{}, store.ErrConnectorCompletionCommitted
	}
	// A re-consent replaces a whole grant: its refresh token outlives its
	// access token, so the row is kept until disconnect revokes it.
	if err := s.retireConnectorCredentialTx(ctx, tx, ref, credential, time.Now().UTC(), retireGrant); err != nil {
		return store.ConnectorCredential{}, err
	}
	committed, err := s.putConnectorCredentialTx(ctx, tx, ref, credential)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	credential = committed
	// Re-seal the parked row with the committed marker inside the sealed
	// body, bound to the same fence columns, so only a holder of the
	// snapshot key can flip it.
	fields.Committed = true
	body, err := encodeSealedConnectorCompletionPayload(credential, fields)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	newNonce, newPayload, err := sealWithAEAD(s.snapshotCipher.aead, connectorCompletionAdditionalData(completion), body)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connector_completions SET payload_nonce = ?, payload = ? WHERE nonce = ?`, newNonce, newPayload, nonce); err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("mark connector completion committed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.ConnectorCredential{}, err
	}
	return credential, nil
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
		version, grantSequence                     int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT dek_nonce, dek_ciphertext, nonce, ciphertext, updated_at, version, grant_sequence
		FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).
		Scan(&dekNonce, &dekCiphertext, &nonce, &ciphertext, &updatedAt, &version, &grantSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ConnectorCredential{}, store.ErrNotFound
	}
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("read connector credential: %w", err)
	}
	credential, err := s.openConnectorCredentialRow(ref, dekNonce, dekCiphertext, nonce, ciphertext)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	credential.UpdatedAt = updatedAt.UTC()
	credential.Version = version
	// The plaintext column is an index for queries; the grant identity that
	// counts is the sealed one, and the two must agree.
	if credential.GrantSequence != grantSequence {
		return store.ConnectorCredential{}, fmt.Errorf("connector credential for connection %s: grant sequence column does not match the sealed grant", ref.ConnectionUID)
	}
	return credential, nil
}

// openConnectorCredentialRow unwraps a row's data key and opens its sealed
// body, both bound to ref.
func (s *Store) openConnectorCredentialRow(ref store.ConnectorCredentialRef, dekNonce, dekCiphertext, nonce, ciphertext []byte) (store.ConnectorCredential, error) {
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
	return decodeSealedConnectorCredential(body)
}

// ListRetiredConnectorCredentials implements store.ConnectorCredentialStore.
func (s *Store) ListRetiredConnectorCredentials(ctx context.Context, ref store.ConnectorCredentialRef) ([]store.ConnectorCredential, error) {
	if s.snapshotCipher == nil {
		return nil, errConnectorCipherRequired
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT dek_nonce, dek_ciphertext, nonce, ciphertext, retired_at
		FROM connector_retired_credentials WHERE connection_uid = ? ORDER BY id`, ref.ConnectionUID)
	if err != nil {
		return nil, fmt.Errorf("read retired connector credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []store.ConnectorCredential
	for rows.Next() {
		var (
			dekNonce, dekCiphertext, nonce, ciphertext []byte
			retiredAt                                  time.Time
		)
		if err := rows.Scan(&dekNonce, &dekCiphertext, &nonce, &ciphertext, &retiredAt); err != nil {
			return nil, fmt.Errorf("scan retired connector credential: %w", err)
		}
		credential, err := s.openConnectorCredentialRow(ref, dekNonce, dekCiphertext, nonce, ciphertext)
		if err != nil {
			return nil, err
		}
		credential.UpdatedAt = retiredAt.UTC()
		result = append(result, credential)
	}
	return result, rows.Err()
}

// TombstoneConnectorCustody implements store.ConnectorCredentialStore.
func (s *Store) TombstoneConnectorCustody(ctx context.Context, connectionUID string) error {
	if strings.TrimSpace(connectionUID) == "" {
		return errors.New("connector credential connection UID is required")
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector tombstone transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := reapConnectorTombstonesTx(ctx, tx, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_credential_tombstones (connection_uid, deleted_at) VALUES (?, ?)
		ON CONFLICT(connection_uid) DO NOTHING`, connectionUID, now); err != nil {
		return fmt.Errorf("tombstone connector custody: %w", err)
	}
	return tx.Commit()
}

// connectorTombstoneRetention is how long a tombstone outlives its
// Connection. It only has to fence a consent or completion that was in
// flight at disconnect, and those expire after minutes; a UID is never
// reused, so an expired tombstone fences nothing.
const connectorTombstoneRetention = 24 * time.Hour

// reapConnectorTombstonesTx drops tombstones past their retention together
// with the grant counters of those Connections, so create/delete churn does
// not grow the store without bound.
func reapConnectorTombstonesTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	cutoff := now.Add(-connectorTombstoneRetention)
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_credential_grants WHERE connection_uid IN
		(SELECT connection_uid FROM connector_credential_tombstones WHERE deleted_at < ?)`, cutoff); err != nil {
		return fmt.Errorf("reap connector grant counters: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_credential_versions WHERE connection_uid IN
		(SELECT connection_uid FROM connector_credential_tombstones WHERE deleted_at < ?)`, cutoff); err != nil {
		return fmt.Errorf("reap connector version counters: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_credential_tombstones WHERE deleted_at < ?`, cutoff); err != nil {
		return fmt.Errorf("reap connector tombstones: %w", err)
	}
	return nil
}

// ReplaceConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) ReplaceConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential, expectedVersion int64) error {
	s.finishPendingWALTruncate(ctx)
	if s.snapshotCipher == nil {
		return errConnectorCipherRequired
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return errors.New("connector credential access token is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector credential replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	// A disconnect fences custody before it reads the material to revoke;
	// a refresh that lands afterwards must not add a rotated credential
	// outside that revocation set.
	var tombstoned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_credential_tombstones WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&tombstoned); err != nil {
		return fmt.Errorf("check connector custody tombstone: %w", err)
	}
	if tombstoned > 0 {
		return store.ErrConnectorCustodyTombstoned
	}
	// The version fence is checked next so a stale writer retires nothing.
	var currentVersion, currentGrant int64
	err = tx.QueryRowContext(ctx, `SELECT version, grant_sequence FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&currentVersion, &currentGrant)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.ErrNotFound
	case err != nil:
		return fmt.Errorf("inspect connector credential row: %w", err)
	case currentVersion != expectedVersion:
		return store.ErrConflict
	}
	// A replacement carries the row's grant forward whatever the caller
	// supplied: only a consent starts a new grant, and the sealed body must
	// agree with the column.
	credential.GrantSequence = currentGrant
	row, err := s.sealConnectorCredentialRow(ref, credential)
	if err != nil {
		return err
	}
	// A refresh rotates the access token only: the previous refresh token
	// is either the same one (no rotation) or already invalid (rotation),
	// so the replaced row matters until its access token expires.
	if err := s.retireConnectorCredentialTx(ctx, tx, ref, credential, now, retireAccessToken); err != nil {
		return err
	}
	next, err := nextConnectorCredentialVersion(ctx, tx, ref.ConnectionUID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE connector_credentials SET
			dek_nonce = ?, dek_ciphertext = ?, nonce = ?, ciphertext = ?, expires_at = ?,
			version = ?, updated_at = ?
		WHERE connection_uid = ? AND version = ?`,
		row.dekNonce, row.dekCiphertext, row.nonce, row.ciphertext, row.expiresAt, next, now, ref.ConnectionUID, expectedVersion)
	if err != nil {
		return fmt.Errorf("replace connector credential: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("inspect connector credential write: %w", err)
	} else if affected == 0 {
		return store.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit connector credential replacement: %w", err)
	}
	return nil
}

// connectorRetirement says how long a replaced credential row must stay
// revocable.
type connectorRetirement int

const (
	// retireAccessToken keeps the row until its access token expires when
	// the replacement carries the same refresh token; a rotated refresh
	// token makes the row a grant kept until disconnect.
	retireAccessToken connectorRetirement = iota
	// retireGrant keeps a row that holds a refresh token until disconnect,
	// because a refresh token stays usable after its access token expires;
	// a row without one is kept until its access token expires.
	retireGrant
)

// retireConnectorCredentialTx keeps the credential row a write is about to
// replace sealed in the retired table so disconnect can still revoke it:
// only Orka held a copy, and neither a refresh nor a re-consent makes the
// provider forget the previous material. Material that has nothing left to
// revoke is dropped, from the row being replaced and from earlier
// retirements alike, so the table stays bounded by the provider's token
// lifetime rather than by refresh count.
func (s *Store) retireConnectorCredentialTx(ctx context.Context, tx *sql.Tx, ref store.ConnectorCredentialRef, replacement store.ConnectorCredential, now time.Time, retirement connectorRetirement) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_retired_credentials
		WHERE connection_uid = ? AND revocable_until IS NOT NULL AND revocable_until <= ?`, ref.ConnectionUID, now); err != nil {
		return fmt.Errorf("prune expired retired connector credentials: %w", err)
	}
	var (
		dekNonce, dekCiphertext, nonce, ciphertext []byte
		expiresAt                                  sql.NullTime
	)
	err := tx.QueryRowContext(ctx, `SELECT dek_nonce, dek_ciphertext, nonce, ciphertext, expires_at
		FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).
		Scan(&dekNonce, &dekCiphertext, &nonce, &ciphertext, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read replaced connector credential: %w", err)
	}
	previous, err := s.openConnectorCredentialRow(ref, dekNonce, dekCiphertext, nonce, ciphertext)
	if err != nil {
		return fmt.Errorf("open replaced connector credential: %w", err)
	}
	// A replacement that carries the very same tokens (a provider that
	// re-issues long-lived material on every re-consent) retires nothing:
	// the current row will be revoked, and duplicate rows would only
	// multiply the serial provider calls disconnect has to make.
	// Identical material under the same revocation identity is one grant;
	// the same strings issued by another client or revocation endpoint are
	// kept, because disconnect must offer them to that authority too.
	if previous.AccessToken == replacement.AccessToken && previous.RefreshToken == replacement.RefreshToken &&
		previous.RevocationDigest == replacement.RevocationDigest {
		return nil
	}
	revocableUntil := expiresAt
	// A row whose refresh token the replacement does not carry is a grant
	// the provider may still honor (rotation does not promise immediate
	// invalidation): it is kept until disconnect. Only a replacement that
	// keeps the same refresh token leaves nothing but the old access token
	// to revoke.
	if previous.RefreshToken != "" && (retirement == retireGrant || previous.RefreshToken != replacement.RefreshToken) {
		revocableUntil = sql.NullTime{}
	}
	if revocableUntil.Valid && !revocableUntil.Time.After(now) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_retired_credentials
		(connection_uid, dek_nonce, dek_ciphertext, nonce, ciphertext, revocable_until, retired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ref.ConnectionUID, dekNonce, dekCiphertext, nonce, ciphertext, revocableUntil, now); err != nil {
		return fmt.Errorf("retire replaced connector credential: %w", err)
	}
	return nil
}

// ShredConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) ShredConnectorCredential(ctx context.Context, connectionUID string, expectedVersion int64) error {
	s.finishPendingWALTruncate(ctx)
	if strings.TrimSpace(connectionUID) == "" {
		return errors.New("connector credential connection UID is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM connector_credentials WHERE connection_uid = ? AND version = ?`, connectionUID, expectedVersion)
	if err != nil {
		return fmt.Errorf("shred connector credential: %w", err)
	}
	// A shred is a crypto-shred only once the log frames that carried the
	// row are gone too. The truncation runs whether or not this call was the
	// one that removed the row, so a retry after a busy log finishes it.
	fenced := s.connectorRowFenced(ctx, result, connectionUID)
	if err := s.truncateWAL(ctx); err != nil && fenced == nil {
		return err
	}
	return fenced
}

// connectorRowFenced turns a zero-row fenced write into ErrConflict when the
// row still exists at another version, or ErrNotFound when it is gone.
func (s *Store) connectorRowFenced(ctx context.Context, result sql.Result, connectionUID string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect connector credential write: %w", err)
	}
	if affected > 0 {
		return nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_credentials WHERE connection_uid = ?`, connectionUID).Scan(&count); err != nil {
		return fmt.Errorf("inspect connector credential row: %w", err)
	}
	if count == 0 {
		return store.ErrNotFound
	}
	return store.ErrConflict
}

// DeleteConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) DeleteConnectorCredential(ctx context.Context, connectionUID string) error {
	s.finishPendingWALTruncate(ctx)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_retired_credentials WHERE connection_uid = ?`, connectionUID); err != nil {
		return fmt.Errorf("delete retired connector credentials: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_credential_tombstones (connection_uid, deleted_at) VALUES (?, ?)
		ON CONFLICT(connection_uid) DO NOTHING`, connectionUID, time.Now().UTC()); err != nil {
		return fmt.Errorf("tombstone connector custody: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.truncateWAL(ctx)
}

// truncateWAL checkpoints the write-ahead log and truncates it, so frames
// that still carry a deleted custody row (its wrapped data key and
// ciphertext) do not outlive the deletion in the -wal file. secure_delete
// zeroes the row in the main file; this removes the log copy.
func (s *Store) truncateWAL(ctx context.Context) error {
	var err error
	for range 5 {
		var busy, logFrames, checkpointed int
		err = s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
		if err != nil {
			s.pendingWALTruncate.Store(true)
			return fmt.Errorf("truncate connector custody log: %w", err)
		}
		if busy == 0 {
			s.pendingWALTruncate.Store(false)
			return nil
		}
		select {
		case <-ctx.Done():
			s.pendingWALTruncate.Store(true)
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	s.pendingWALTruncate.Store(true)
	return errors.New("truncate connector custody log: readers kept the log busy; retry")
}

// finishPendingWALTruncate completes a truncation an earlier deletion could
// not, so a shredded row's log frames never outlive the next custody
// operation even when the caller of that deletion did not retry.
func (s *Store) finishPendingWALTruncate(ctx context.Context) {
	if s.pendingWALTruncate.Load() {
		_ = s.truncateWAL(ctx)
	}
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
	// One pending consent per Connection: a new authorize replaces any
	// earlier one, so repeated calls cannot grow the table within the TTL.
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE connection_uid = ?`, consent.ConnectionUID); err != nil {
		return fmt.Errorf("replace pending connector consents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_consents
		(nonce, connection_uid, namespace, name, subject_digest, provider, mode, verifier_nonce, verifier_ciphertext, authority_digest, revocation_digest, scopes, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		consent.Nonce, consent.ConnectionUID, consent.Namespace, consent.Name, consent.SubjectDigest, consent.Provider,
		consent.Mode, verifierNonce, verifierCiphertext, consent.AuthorityDigest, consent.RevocationDigest, strings.Join(consent.Scopes, " "), consent.ExpiresAt.UTC(), now); err != nil {
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
	var scopes string
	err = tx.QueryRowContext(ctx, `SELECT nonce, connection_uid, namespace, name, subject_digest, provider, mode,
		verifier_nonce, verifier_ciphertext, authority_digest, revocation_digest, scopes, expires_at FROM connector_consents WHERE nonce = ?`, nonce).
		Scan(&consent.Nonce, &consent.ConnectionUID, &consent.Namespace, &consent.Name, &consent.SubjectDigest,
			&consent.Provider, &consent.Mode, &verifierNonce, &verifierCiphertext, &consent.AuthorityDigest, &consent.RevocationDigest, &scopes, &consent.ExpiresAt)
	consent.Scopes = strings.Fields(scopes)
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
	body, err := encodeSealedConnectorCompletionPayload(completion.Credential, sealedConnectorCompletionFields{ConsentAuthorityDigest: completion.ConsentAuthorityDigest})
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
	credential, fields, err := decodeSealedConnectorCompletionPayload(body)
	if err != nil {
		return store.ConnectorCompletion{}, err
	}
	completion.Credential = credential
	completion.ConsentAuthorityDigest = fields.ConsentAuthorityDigest
	completion.Committed = fields.Committed
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
	s.finishPendingWALTruncate(ctx)
	if strings.TrimSpace(nonce) == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM connector_completions WHERE nonce = ?`, nonce); err != nil {
		return fmt.Errorf("delete connector completion: %w", err)
	}
	return s.truncateWAL(ctx)
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
		credential, fields, err := decodeSealedConnectorCompletionPayload(body)
		if err != nil {
			continue
		}
		completion.Credential = credential
		completion.ConsentAuthorityDigest = fields.ConsentAuthorityDigest
		completion.Committed = fields.Committed
		result = append(result, completion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connector completions: %w", err)
	}
	return result, nil
}

// DeleteConnectorConsentsForConnection implements store.ConnectorConsentStore.
func (s *Store) DeleteConnectorConsentsForConnection(ctx context.Context, connectionUID string) error {
	s.finishPendingWALTruncate(ctx)
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
	if err := tx.Commit(); err != nil {
		return err
	}
	// Parked completions carry token material sealed under the controller
	// key itself; their log frames go with the rows, as custody's do.
	return s.truncateWAL(ctx)
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

// verifyConnectorRowsWithCipher authenticates every retained connector row
// with a candidate key: the wrapped data keys of current and retired
// custody, and the sealed payloads of pending consents and completions.
func (s *Store) verifyConnectorRowsWithCipher(snapshotCipher *AgentExecutionSnapshotCipher) error {
	if snapshotCipher == nil {
		return errors.New("agent execution snapshot cipher is required")
	}
	for _, table := range []string{"connector_credentials", "connector_retired_credentials"} {
		rows, err := s.db.Query(`SELECT connection_uid, dek_nonce, dek_ciphertext FROM ` + table)
		if err != nil {
			return fmt.Errorf("verify connector custody key (%s): %w", table, err)
		}
		for rows.Next() {
			var (
				connectionUID           string
				dekNonce, dekCiphertext []byte
			)
			if err := rows.Scan(&connectionUID, &dekNonce, &dekCiphertext); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan connector custody (%s) while verifying key: %w", table, err)
			}
			if _, err := snapshotCipher.aead.Open(nil, dekNonce, dekCiphertext, connectorDataKeyAdditionalData(connectionUID)); err != nil {
				_ = rows.Close()
				return fmt.Errorf("candidate agent execution snapshot key cannot open connector custody for connection %s (%s); "+
					"restore the previous key: linked accounts would be unusable and unrevocable: %w", connectionUID, table, err)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("iterate connector custody (%s) while verifying key: %w", table, err)
		}
	}
	consents, err := s.db.Query(`SELECT nonce, connection_uid, namespace, name, subject_digest, provider, mode, authority_digest, revocation_digest, scopes,
		expires_at, verifier_nonce, verifier_ciphertext FROM connector_consents`)
	if err != nil {
		return fmt.Errorf("verify connector consents key: %w", err)
	}
	for consents.Next() {
		var (
			consent               store.ConnectorConsent
			scopes                string
			verifierNonce, sealed []byte
		)
		if err := consents.Scan(&consent.Nonce, &consent.ConnectionUID, &consent.Namespace, &consent.Name, &consent.SubjectDigest,
			&consent.Provider, &consent.Mode, &consent.AuthorityDigest, &consent.RevocationDigest, &scopes, &consent.ExpiresAt, &verifierNonce, &sealed); err != nil {
			_ = consents.Close()
			return fmt.Errorf("scan connector consent while verifying key: %w", err)
		}
		consent.Scopes = strings.Fields(scopes)
		consent.ExpiresAt = consent.ExpiresAt.UTC()
		if _, err := snapshotCipher.aead.Open(nil, verifierNonce, sealed, connectorConsentAdditionalData(consent)); err != nil {
			_ = consents.Close()
			return fmt.Errorf("candidate agent execution snapshot key cannot open pending connector consent for connection %s; restore the previous key: %w", consent.ConnectionUID, err)
		}
	}
	err = consents.Err()
	_ = consents.Close()
	if err != nil {
		return fmt.Errorf("iterate connector consents while verifying key: %w", err)
	}
	completions, err := s.db.Query(`SELECT nonce, connection_uid, namespace, name, subject_digest, provider, mode,
		payload_nonce, payload, expires_at FROM connector_completions`)
	if err != nil {
		return fmt.Errorf("verify connector completions key: %w", err)
	}
	for completions.Next() {
		var (
			completion            store.ConnectorCompletion
			payloadNonce, payload []byte
		)
		if err := completions.Scan(&completion.Nonce, &completion.ConnectionUID, &completion.Namespace, &completion.Name, &completion.SubjectDigest,
			&completion.Provider, &completion.Mode, &payloadNonce, &payload, &completion.ExpiresAt); err != nil {
			_ = completions.Close()
			return fmt.Errorf("scan connector completion while verifying key: %w", err)
		}
		completion.ExpiresAt = completion.ExpiresAt.UTC()
		if _, err := snapshotCipher.aead.Open(nil, payloadNonce, payload, connectorCompletionAdditionalData(completion)); err != nil {
			_ = completions.Close()
			return fmt.Errorf("candidate agent execution snapshot key cannot open parked connector completion for connection %s; restore the previous key: %w", completion.ConnectionUID, err)
		}
	}
	err = completions.Err()
	_ = completions.Close()
	if err != nil {
		return fmt.Errorf("iterate connector completions while verifying key: %w", err)
	}
	return nil
}

// RetireConnectorCredential implements store.ConnectorCredentialStore.
func (s *Store) RetireConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	if s.snapshotCipher == nil {
		return errConnectorCipherRequired
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return errors.New("connector credential access token is required")
	}
	now := time.Now().UTC()
	revocableUntil := sql.NullTime{}
	if credential.RefreshToken == "" && !credential.ExpiresAt.IsZero() {
		if !credential.ExpiresAt.After(now) {
			return nil
		}
		revocableUntil = sql.NullTime{Time: credential.ExpiresAt.UTC(), Valid: true}
	}
	row, err := s.sealConnectorCredentialRow(ref, credential)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector retirement transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var tombstoned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_credential_tombstones WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&tombstoned); err != nil {
		return fmt.Errorf("check connector custody tombstone: %w", err)
	}
	if tombstoned > 0 {
		return store.ErrConnectorCustodyTombstoned
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_retired_credentials
		WHERE connection_uid = ? AND revocable_until IS NOT NULL AND revocable_until <= ?`, ref.ConnectionUID, now); err != nil {
		return fmt.Errorf("prune expired retired connector credentials: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_retired_credentials
		(connection_uid, dek_nonce, dek_ciphertext, nonce, ciphertext, revocable_until, retired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ref.ConnectionUID, row.dekNonce, row.dekCiphertext, row.nonce, row.ciphertext, revocableUntil, now); err != nil {
		return fmt.Errorf("retire connector credential: %w", err)
	}
	return tx.Commit()
}
