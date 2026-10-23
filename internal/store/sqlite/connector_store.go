/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Connector custody is stored sealed. The storage client seals token
// material, consent verifiers and completion payloads under the snapshot key
// before they reach this store, and decides everything that needs the
// plaintext. This store keeps the sealed bytes exactly, decides each
// conditional command from the plain columns, and crypto-shreds deleted
// records: secure_delete zeroes them in the database file and the
// write-ahead log is truncated so its frames do not outlive them.

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
			sequence            INTEGER NOT NULL DEFAULT 0,
			expires_at          TIMESTAMP NOT NULL,
			created_at          TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_connector_consents_connection
			ON connector_consents(connection_uid)`,
		// consent_grant marks a grant a re-consent replaced; only these
		// count toward the per-Connection bound.
		`CREATE TABLE IF NOT EXISTS connector_retired_credentials (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			connection_uid TEXT NOT NULL,
			dek_nonce      BLOB NOT NULL,
			dek_ciphertext BLOB NOT NULL,
			nonce          BLOB NOT NULL,
			ciphertext     BLOB NOT NULL,
			revocable_until TIMESTAMP,
			consent_grant  INTEGER NOT NULL DEFAULT 0,
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
			consent_sequence INTEGER NOT NULL DEFAULT 0,
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
		// issued numbers each Connection's consents; committed is the
		// newest consent whose tokens became custody, so an older consent
		// can never be committed over it.
		`CREATE TABLE IF NOT EXISTS connector_consent_sequences (
			connection_uid  TEXT PRIMARY KEY,
			issued          INTEGER NOT NULL,
			committed       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS connector_credential_tombstones (
			connection_uid  TEXT PRIMARY KEY,
			deleted_at      TIMESTAMP NOT NULL
		)`,
	}
}

// maxRetiredConnectorCredentials bounds the superseded consent grants one
// Connection retains for revocation at disconnect.
const maxRetiredConnectorCredentials = storagev1.MaxRetiredConnectorConsentGrants

// maxRefreshRetiredConnectorCredentials bounds the grants a refresh rotated
// away that one Connection keeps until disconnect.
const maxRefreshRetiredConnectorCredentials = storagev1.MaxRetiredConnectorRefreshGrants

// connectorTombstoneRetention is how long a tombstone outlives its
// Connection; see storagev1.ConnectorTombstoneRetention.
const connectorTombstoneRetention = storagev1.ConnectorTombstoneRetention

const connectorCredentialColumns = `connection_uid, namespace, name, subject_digest, provider,
	dek_nonce, dek_ciphertext, nonce, ciphertext, expires_at, version, grant_sequence, updated_at`

const connectorRetiredColumns = `connection_uid, dek_nonce, dek_ciphertext, nonce, ciphertext, revocable_until, consent_grant, retired_at`

const connectorConsentColumns = `nonce, connection_uid, namespace, name, subject_digest, provider, mode,
	authority_digest, revocation_digest, scopes, sequence, expires_at, verifier_nonce, verifier_ciphertext`

const connectorCompletionColumns = `nonce, connection_uid, namespace, name, subject_digest, provider, mode,
	consent_sequence, payload_nonce, payload, expires_at`

type connectorRowScanner interface {
	Scan(dest ...any) error
}

func scanConnectorCredential(row connectorRowScanner) (storagev1.SealedConnectorCredential, error) {
	var (
		credential storagev1.SealedConnectorCredential
		expiresAt  sql.NullTime
	)
	if err := row.Scan(&credential.ConnectionUID, &credential.Namespace, &credential.Name, &credential.SubjectDigest, &credential.Provider,
		&credential.DataKeyNonce, &credential.DataKeyCiphertext, &credential.Nonce, &credential.Ciphertext,
		&expiresAt, &credential.Version, &credential.GrantSequence, &credential.UpdatedAt); err != nil {
		return storagev1.SealedConnectorCredential{}, err
	}
	if expiresAt.Valid {
		credential.ExpiresAt = expiresAt.Time.UTC()
	}
	credential.UpdatedAt = credential.UpdatedAt.UTC()
	return credential, nil
}

func scanConnectorRetired(row connectorRowScanner) (storagev1.SealedRetiredConnectorCredential, error) {
	var (
		retired        storagev1.SealedRetiredConnectorCredential
		revocableUntil sql.NullTime
		consentGrant   int64
	)
	if err := row.Scan(&retired.ConnectionUID, &retired.DataKeyNonce, &retired.DataKeyCiphertext, &retired.Nonce, &retired.Ciphertext,
		&revocableUntil, &consentGrant, &retired.RetiredAt); err != nil {
		return storagev1.SealedRetiredConnectorCredential{}, err
	}
	if revocableUntil.Valid {
		retired.RevocableUntil = revocableUntil.Time.UTC()
	}
	retired.ConsentGrant = consentGrant != 0
	retired.RetiredAt = retired.RetiredAt.UTC()
	return retired, nil
}

func scanConnectorConsent(row connectorRowScanner) (storagev1.SealedConnectorConsent, error) {
	var (
		consent storagev1.SealedConnectorConsent
		scopes  string
	)
	if err := row.Scan(&consent.Nonce, &consent.ConnectionUID, &consent.Namespace, &consent.Name, &consent.SubjectDigest,
		&consent.Provider, &consent.Mode, &consent.AuthorityDigest, &consent.RevocationDigest, &scopes, &consent.Sequence,
		&consent.ExpiresAt, &consent.VerifierNonce, &consent.VerifierCiphertext); err != nil {
		return storagev1.SealedConnectorConsent{}, err
	}
	consent.Scopes = strings.Fields(scopes)
	consent.ExpiresAt = consent.ExpiresAt.UTC()
	return consent, nil
}

func scanConnectorCompletion(row connectorRowScanner) (storagev1.SealedConnectorCompletion, error) {
	var completion storagev1.SealedConnectorCompletion
	if err := row.Scan(&completion.Nonce, &completion.ConnectionUID, &completion.Namespace, &completion.Name, &completion.SubjectDigest,
		&completion.Provider, &completion.Mode, &completion.ConsentSequence, &completion.PayloadNonce, &completion.Payload,
		&completion.ExpiresAt); err != nil {
		return storagev1.SealedConnectorCompletion{}, err
	}
	completion.ExpiresAt = completion.ExpiresAt.UTC()
	return completion, nil
}

type connectorQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryConnectorRecords runs query and scans every row with scan.
func queryConnectorRecords[T any](
	ctx context.Context, q connectorQueryer, what string, scan func(connectorRowScanner) (T, error), query string, args ...any,
) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var records []T
	for rows.Next() {
		record, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", what, err)
	}
	return records, nil
}

func requireConnectionUID(connectionUID string) error {
	if strings.TrimSpace(connectionUID) == "" {
		return store.ValidationErrorf("connector custody connection UID is required")
	}
	return nil
}

func requireConnectorRequestTime(now time.Time) error {
	if now.IsZero() {
		return store.ValidationErrorf("connector custody request time is required")
	}
	return nil
}

func nullableConnectorTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func utcConnectorTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

func connectorTombstonedTx(ctx context.Context, tx *sql.Tx, connectionUID string) (bool, error) {
	var tombstoned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_credential_tombstones WHERE connection_uid = ?)`,
		connectionUID).Scan(&tombstoned); err != nil {
		return false, fmt.Errorf("check connector custody tombstone: %w", err)
	}
	return tombstoned, nil
}

// connectorCredentialTx returns the Connection's current custody, or nil.
func connectorCredentialTx(ctx context.Context, tx *sql.Tx, connectionUID string) (*storagev1.SealedConnectorCredential, error) {
	credential, err := scanConnectorCredential(tx.QueryRowContext(ctx, `SELECT `+connectorCredentialColumns+`
		FROM connector_credentials WHERE connection_uid = ?`, connectionUID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read connector credential: %w", err)
	}
	return &credential, nil
}

func connectorLastGrantTx(ctx context.Context, tx *sql.Tx, connectionUID string) (int64, error) {
	var grant int64
	err := tx.QueryRowContext(ctx, `SELECT grant_sequence FROM connector_credential_grants WHERE connection_uid = ?`, connectionUID).Scan(&grant)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read connector grant sequence: %w", err)
	}
	return grant, nil
}

// connectorConsentSequencesTx returns the last consent sequence issued and
// the newest one committed, or zeros.
func connectorConsentSequencesTx(ctx context.Context, tx *sql.Tx, connectionUID string) (int64, int64, error) {
	var issued, committed int64
	err := tx.QueryRowContext(ctx, `SELECT issued, committed FROM connector_consent_sequences WHERE connection_uid = ?`,
		connectionUID).Scan(&issued, &committed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read connector consent sequences: %w", err)
	}
	return issued, committed, nil
}

func connectorCompletionsTx(ctx context.Context, tx *sql.Tx, connectionUID string) ([]storagev1.SealedConnectorCompletion, error) {
	return queryConnectorRecords(ctx, tx, "connector completions", scanConnectorCompletion,
		`SELECT `+connectorCompletionColumns+` FROM connector_completions WHERE connection_uid = ? ORDER BY nonce`, connectionUID)
}

// requireConnectorCompletionSet reports ErrConflict unless the Connection's
// parked completions are exactly expected, each with the same sealed
// payload.
func requireConnectorCompletionSet(current []storagev1.SealedConnectorCompletion, expected []storagev1.SealedConnectorCompletionRef) error {
	want := make(map[string]string, len(expected))
	for _, ref := range expected {
		want[ref.Nonce] = ref.Digest
	}
	if len(want) != len(current) {
		return store.ConflictErrorf("connector completions changed")
	}
	for _, completion := range current {
		if digest, ok := want[completion.Nonce]; !ok || digest != completion.Ref().Digest {
			return store.ConflictErrorf("connector completions changed")
		}
	}
	return nil
}

// validateConnectorCompletionRefs rejects expected completions that repeat a
// nonce, and named completions outside them.
func validateConnectorCompletionRefs(expected []storagev1.SealedConnectorCompletionRef, named []string, field string) error {
	set := make(map[string]bool, len(expected))
	for _, ref := range expected {
		if strings.TrimSpace(ref.Nonce) == "" || set[ref.Nonce] {
			return store.ValidationErrorf("expected connector completions must name distinct nonces")
		}
		if err := store.ValidateCanonicalDigest("expected connector completion digest", ref.Digest); err != nil {
			return err
		}
		set[ref.Nonce] = true
	}
	for _, nonce := range named {
		if !set[nonce] {
			return store.ValidationErrorf("%s must name expected connector completions", field)
		}
	}
	return nil
}

// ReadConnectorCustody implements storagev1.SealedConnectorCustodyStore.
func (s *Store) ReadConnectorCustody(ctx context.Context, connectionUID string) (*storagev1.ConnectorCustody, error) {
	s.finishPendingWALTruncate(ctx)
	if err := requireConnectionUID(connectionUID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin connector custody read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	custody := &storagev1.ConnectorCustody{ConnectionUID: connectionUID}
	if custody.Tombstoned, err = connectorTombstonedTx(ctx, tx, connectionUID); err != nil {
		return nil, err
	}
	if custody.Credential, err = connectorCredentialTx(ctx, tx, connectionUID); err != nil {
		return nil, err
	}
	if custody.Retired, err = queryConnectorRecords(ctx, tx, "retired connector credentials", scanConnectorRetired,
		`SELECT `+connectorRetiredColumns+` FROM connector_retired_credentials WHERE connection_uid = ? ORDER BY id`, connectionUID); err != nil {
		return nil, err
	}
	if custody.Completions, err = connectorCompletionsTx(ctx, tx, connectionUID); err != nil {
		return nil, err
	}
	if custody.LastGrantSequence, err = connectorLastGrantTx(ctx, tx, connectionUID); err != nil {
		return nil, err
	}
	if custody.IssuedConsentSequence, custody.CommittedConsentSequence, err = connectorConsentSequencesTx(ctx, tx, connectionUID); err != nil {
		return nil, err
	}
	return custody, nil
}

// GetSealedConnectorCredential implements storagev1.SealedConnectorCustodyStore.
func (s *Store) GetSealedConnectorCredential(ctx context.Context, connectionUID string) (*storagev1.SealedConnectorCredential, error) {
	s.finishPendingWALTruncate(ctx)
	if err := requireConnectionUID(connectionUID); err != nil {
		return nil, err
	}
	credential, err := scanConnectorCredential(s.db.QueryRowContext(ctx, `SELECT `+connectorCredentialColumns+`
		FROM connector_credentials WHERE connection_uid = ?`, connectionUID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read connector credential: %w", err)
	}
	return &credential, nil
}

// PutSealedConnectorCredential implements storagev1.SealedConnectorCustodyStore.
func (s *Store) PutSealedConnectorCredential(ctx context.Context, request storagev1.PutSealedConnectorCredentialRequest) (*storagev1.SealedConnectorCredential, error) {
	s.finishPendingWALTruncate(ctx)
	if err := request.Credential.Validate(); err != nil {
		return nil, err
	}
	if err := requireConnectorRequestTime(request.Now); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin connector credential transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if tombstoned, err := connectorTombstonedTx(ctx, tx, request.Credential.ConnectionUID); err != nil {
		return nil, err
	} else if tombstoned {
		return nil, store.ErrConnectorCustodyTombstoned
	}
	stored, err := s.putConnectorCredentialTx(ctx, tx, request.Credential, request.Now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit connector credential: %w", err)
	}
	return stored, nil
}

// putConnectorCredentialTx stores a new grant as custody inside tx. The grant
// must be the next one: grants are numbered from a per-Connection counter
// that outlives the record, so a grant after a shred never repeats a number.
// Versions likewise come from a counter that survives a shred, so a record
// recreated after one never reuses a version a stale fenced write could still
// hold.
func (s *Store) putConnectorCredentialTx(
	ctx context.Context, tx *sql.Tx, credential storagev1.SealedConnectorCredential, now time.Time,
) (*storagev1.SealedConnectorCredential, error) {
	last, err := connectorLastGrantTx(ctx, tx, credential.ConnectionUID)
	if err != nil {
		return nil, err
	}
	if credential.GrantSequence != last+1 {
		return nil, store.ConflictErrorf("connector grant sequence %d is not the next grant", credential.GrantSequence)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_credential_grants (connection_uid, grant_sequence) VALUES (?, ?)
		ON CONFLICT(connection_uid) DO UPDATE SET grant_sequence = excluded.grant_sequence`,
		credential.ConnectionUID, credential.GrantSequence); err != nil {
		return nil, fmt.Errorf("assign connector grant sequence: %w", err)
	}
	version, err := nextConnectorCredentialVersion(ctx, tx, credential.ConnectionUID)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
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
		credential.ConnectionUID, credential.Namespace, credential.Name, credential.SubjectDigest, credential.Provider,
		credential.DataKeyNonce, credential.DataKeyCiphertext, credential.Nonce, credential.Ciphertext,
		nullableConnectorTime(credential.ExpiresAt), version, credential.GrantSequence, now, now)
	if err != nil {
		return nil, fmt.Errorf("persist connector credential: %w", err)
	}
	credential.ExpiresAt = utcConnectorTime(credential.ExpiresAt)
	credential.Version = version
	credential.UpdatedAt = now
	return &credential, nil
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

// ReplaceSealedConnectorCredential implements storagev1.SealedConnectorCustodyStore.
func (s *Store) ReplaceSealedConnectorCredential(
	ctx context.Context, request storagev1.ReplaceSealedConnectorCredentialRequest,
) (*storagev1.SealedConnectorCredential, error) {
	s.finishPendingWALTruncate(ctx)
	credential := request.Credential
	if err := credential.Validate(); err != nil {
		return nil, err
	}
	if err := requireConnectorRequestTime(request.Now); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin connector credential replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// A disconnect fences custody before it reads the material to revoke; a
	// refresh that lands afterwards must not add a rotated credential outside
	// that revocation set.
	if tombstoned, err := connectorTombstonedTx(ctx, tx, credential.ConnectionUID); err != nil {
		return nil, err
	} else if tombstoned {
		return nil, store.ErrConnectorCustodyTombstoned
	}
	// The version fence is checked next so a stale writer retires nothing.
	current, err := connectorCredentialTx(ctx, tx, credential.ConnectionUID)
	switch {
	case err != nil:
		return nil, err
	case current == nil:
		return nil, store.ErrNotFound
	case current.Version != request.ExpectedVersion:
		return nil, store.ErrConflict
	case current.GrantSequence != credential.GrantSequence:
		// Only a consent starts a new grant.
		return nil, store.ConflictErrorf("a replacement must carry the stored grant")
	}
	now := request.Now.UTC()
	if err := pruneRetiredConnectorCredentialsTx(ctx, tx, credential.ConnectionUID, now); err != nil {
		return nil, err
	}
	if request.Retire != nil {
		if err := s.retireConnectorCredentialTx(ctx, tx, *current, request.Retire.RevocableUntil, false, now); err != nil {
			return nil, err
		}
	}
	version, err := nextConnectorCredentialVersion(ctx, tx, credential.ConnectionUID)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE connector_credentials SET
			dek_nonce = ?, dek_ciphertext = ?, nonce = ?, ciphertext = ?, expires_at = ?,
			version = ?, updated_at = ?
		WHERE connection_uid = ? AND version = ?`,
		credential.DataKeyNonce, credential.DataKeyCiphertext, credential.Nonce, credential.Ciphertext,
		nullableConnectorTime(credential.ExpiresAt), version, now, credential.ConnectionUID, request.ExpectedVersion)
	if err != nil {
		return nil, fmt.Errorf("replace connector credential: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("inspect connector credential write: %w", err)
	} else if affected == 0 {
		return nil, store.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit connector credential replacement: %w", err)
	}
	// A replacement does not rewrite the identity columns.
	stored := *current
	stored.DataKeyNonce, stored.DataKeyCiphertext = credential.DataKeyNonce, credential.DataKeyCiphertext
	stored.Nonce, stored.Ciphertext = credential.Nonce, credential.Ciphertext
	stored.ExpiresAt, stored.Version, stored.UpdatedAt = utcConnectorTime(credential.ExpiresAt), version, now
	return &stored, nil
}

// pruneRetiredConnectorCredentialsTx drops retired material that has nothing
// left to revoke, so the table stays bounded by the provider's token
// lifetime rather than by refresh count.
func pruneRetiredConnectorCredentialsTx(ctx context.Context, tx *sql.Tx, connectionUID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_retired_credentials
		WHERE connection_uid = ? AND revocable_until IS NOT NULL AND revocable_until <= ?`, connectionUID, now); err != nil {
		return fmt.Errorf("prune expired retired connector credentials: %w", err)
	}
	return nil
}

// retireConnectorCredentialTx keeps replaced custody sealed, exactly as
// stored, so disconnect can still revoke it. Grants a refresh rotated away
// are kept until disconnect only up to a bound, so a long-lived Connection
// cannot grow custody (and the serial revocations at disconnect) without
// limit. The oldest are dropped, never revoked: providers that rotate refresh
// tokens invalidate the old one, and revoking a rotated token can revoke the
// whole grant at providers that detect reuse.
func (s *Store) retireConnectorCredentialTx(
	ctx context.Context, tx *sql.Tx, replaced storagev1.SealedConnectorCredential, revocableUntil time.Time, consentGrant bool, now time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_retired_credentials
		(connection_uid, dek_nonce, dek_ciphertext, nonce, ciphertext, revocable_until, consent_grant, retired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		replaced.ConnectionUID, replaced.DataKeyNonce, replaced.DataKeyCiphertext, replaced.Nonce, replaced.Ciphertext,
		nullableConnectorTime(revocableUntil), consentGrant, now); err != nil {
		return fmt.Errorf("retire replaced connector credential: %w", err)
	}
	if !consentGrant && revocableUntil.IsZero() {
		return s.boundRefreshRetiredTx(ctx, tx, replaced.ConnectionUID)
	}
	return nil
}

// boundRefreshRetiredTx keeps only the newest refresh-retired grants a
// Connection holds until disconnect.
func (s *Store) boundRefreshRetiredTx(ctx context.Context, tx *sql.Tx, connectionUID string) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM connector_retired_credentials WHERE id IN (
		SELECT id FROM connector_retired_credentials
		WHERE connection_uid = ? AND consent_grant = 0 AND revocable_until IS NULL
		ORDER BY id DESC LIMIT -1 OFFSET ?)`, connectionUID, maxRefreshRetiredConnectorCredentials)
	if err != nil {
		return fmt.Errorf("bound refresh-retired connector credentials: %w", err)
	}
	if dropped, _ := result.RowsAffected(); dropped > 0 {
		s.pendingWALTruncate.Store(true)
	}
	return nil
}

// RetireSealedConnectorCredential implements storagev1.SealedConnectorCustodyStore.
func (s *Store) RetireSealedConnectorCredential(ctx context.Context, retired storagev1.SealedRetiredConnectorCredential) error {
	s.finishPendingWALTruncate(ctx)
	if err := requireConnectionUID(retired.ConnectionUID); err != nil {
		return err
	}
	if len(retired.DataKeyNonce) == 0 || len(retired.DataKeyCiphertext) == 0 || len(retired.Nonce) == 0 || len(retired.Ciphertext) == 0 {
		return store.ValidationErrorf("retired connector credential requires a sealed data key and ciphertext")
	}
	if retired.ConsentGrant {
		return store.ValidationErrorf("only a commit retires a consent grant")
	}
	if err := requireConnectorRequestTime(retired.RetiredAt); err != nil {
		return err
	}
	now := retired.RetiredAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector retirement transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if tombstoned, err := connectorTombstonedTx(ctx, tx, retired.ConnectionUID); err != nil {
		return err
	} else if tombstoned {
		return store.ErrConnectorCustodyTombstoned
	}
	if err := pruneRetiredConnectorCredentialsTx(ctx, tx, retired.ConnectionUID, now); err != nil {
		return err
	}
	// Refresh material retired here is bounded like any rotated grant, so a
	// store that keeps failing replacement cannot grow custody without limit.
	if err := s.retireConnectorCredentialTx(ctx, tx, storagev1.SealedConnectorCredential{
		ConnectionUID: retired.ConnectionUID, DataKeyNonce: retired.DataKeyNonce, DataKeyCiphertext: retired.DataKeyCiphertext,
		Nonce: retired.Nonce, Ciphertext: retired.Ciphertext,
	}, retired.RevocableUntil, false, now); err != nil {
		return err
	}
	return tx.Commit()
}

// TombstoneConnectorCustody implements storagev1.SealedConnectorCustodyStore.
func (s *Store) TombstoneConnectorCustody(ctx context.Context, connectionUID string) error {
	s.finishPendingWALTruncate(ctx)
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

// reapConnectorTombstonesTx drops tombstones past their retention together
// with the sequence counters of those Connections, so create/delete churn
// does not grow the store without bound.
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consent_sequences WHERE connection_uid IN
		(SELECT connection_uid FROM connector_credential_tombstones WHERE deleted_at < ?)`, cutoff); err != nil {
		return fmt.Errorf("reap connector consent counters: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_credential_tombstones WHERE deleted_at < ?`, cutoff); err != nil {
		return fmt.Errorf("reap connector tombstones: %w", err)
	}
	return nil
}

// ShredConnectorCredential implements storagev1.SealedConnectorCustodyStore.
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

// DeleteConnectorCredential implements storagev1.SealedConnectorCustodyStore.
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

// CreateSealedConnectorConsent implements storagev1.SealedConnectorConsentStore.
func (s *Store) CreateSealedConnectorConsent(ctx context.Context, request storagev1.CreateSealedConnectorConsentRequest) error {
	s.finishPendingWALTruncate(ctx)
	consent := request.Consent
	if err := consent.Validate(); err != nil {
		return err
	}
	if err := requireConnectorRequestTime(request.Now); err != nil {
		return err
	}
	now := request.Now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector consent transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Each consent takes the Connection's next sequence, sealed with its
	// verifier; commits follow this order, not callback arrival.
	issued, _, err := connectorConsentSequencesTx(ctx, tx, consent.ConnectionUID)
	if err != nil {
		return err
	}
	if consent.Sequence != issued+1 {
		return store.ConflictErrorf("connector consent sequence %d is not the next consent", consent.Sequence)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_consent_sequences (connection_uid, issued, committed) VALUES (?, ?, 0)
		ON CONFLICT(connection_uid) DO UPDATE SET issued = excluded.issued`, consent.ConnectionUID, consent.Sequence); err != nil {
		return fmt.Errorf("number connector consent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE expires_at < ?`, now); err != nil {
		return fmt.Errorf("purge expired connector consents: %w", err)
	}
	// One pending consent per Connection: a new authorize replaces any
	// earlier one, so repeated calls cannot grow the table within the TTL.
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE connection_uid = ?`, consent.ConnectionUID); err != nil {
		return fmt.Errorf("replace pending connector consents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_consents
		(nonce, connection_uid, namespace, name, subject_digest, provider, mode, verifier_nonce, verifier_ciphertext, authority_digest, revocation_digest, scopes, sequence, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		consent.Nonce, consent.ConnectionUID, consent.Namespace, consent.Name, consent.SubjectDigest, consent.Provider,
		consent.Mode, consent.VerifierNonce, consent.VerifierCiphertext, consent.AuthorityDigest, consent.RevocationDigest,
		strings.Join(consent.Scopes, " "), consent.Sequence, consent.ExpiresAt.UTC(), now); err != nil {
		return fmt.Errorf("persist connector consent: %w", err)
	}
	return tx.Commit()
}

// ConsumeSealedConnectorConsent implements storagev1.SealedConnectorConsentStore.
func (s *Store) ConsumeSealedConnectorConsent(ctx context.Context, nonce string, now time.Time) (*storagev1.SealedConnectorConsent, error) {
	s.finishPendingWALTruncate(ctx)
	if strings.TrimSpace(nonce) == "" {
		return nil, store.ErrNotFound
	}
	if err := requireConnectorRequestTime(now); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin connector consent transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE expires_at < ?`, now.UTC()); err != nil {
		return nil, fmt.Errorf("purge expired connector consents: %w", err)
	}
	consent, err := scanConnectorConsent(tx.QueryRowContext(ctx, `SELECT `+connectorConsentColumns+`
		FROM connector_consents WHERE nonce = ?`, nonce))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read connector consent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_consents WHERE nonce = ?`, nonce); err != nil {
		return nil, fmt.Errorf("consume connector consent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit connector consent: %w", err)
	}
	return &consent, nil
}

// CreateSealedConnectorCompletion implements storagev1.SealedConnectorConsentStore.
func (s *Store) CreateSealedConnectorCompletion(ctx context.Context, request storagev1.CreateSealedConnectorCompletionRequest) error {
	s.finishPendingWALTruncate(ctx)
	completion := request.Completion
	if err := completion.Validate(); err != nil {
		return err
	}
	if err := requireConnectorRequestTime(request.Now); err != nil {
		return err
	}
	if err := validateConnectorCompletionRefs(request.ExpectedCompletions, request.Replace, "replaced completions"); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector completion transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Expired completions are not purged here: the Connection reconciler
	// reaps them (their tokens are left to expire, never revoked).
	if tombstoned, err := connectorTombstonedTx(ctx, tx, completion.ConnectionUID); err != nil {
		return err
	} else if tombstoned {
		return store.ErrConnectorCustodyTombstoned
	}
	if _, committed, err := connectorConsentSequencesTx(ctx, tx, completion.ConnectionUID); err != nil {
		return err
	} else if completion.ConsentSequence <= committed {
		return store.ErrConnectorConsentSuperseded
	}
	current, err := connectorCompletionsTx(ctx, tx, completion.ConnectionUID)
	if err != nil {
		return err
	}
	if err := requireConnectorCompletionSet(current, request.ExpectedCompletions); err != nil {
		return err
	}
	for _, nonce := range request.Replace {
		if _, err := tx.ExecContext(ctx, `DELETE FROM connector_completions WHERE nonce = ?`, nonce); err != nil {
			return fmt.Errorf("replace older connector completion: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_completions
		(nonce, connection_uid, namespace, name, subject_digest, provider, mode, consent_sequence, payload_nonce, payload, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		completion.Nonce, completion.ConnectionUID, completion.Namespace, completion.Name, completion.SubjectDigest,
		completion.Provider, completion.Mode, completion.ConsentSequence, completion.PayloadNonce, completion.Payload,
		completion.ExpiresAt.UTC(), request.Now.UTC()); err != nil {
		return fmt.Errorf("persist connector completion: %w", uniqueConflict(err))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(request.Replace) > 0 {
		// Best effort: a log that readers kept busy is truncated by the next
		// custody deletion.
		_ = s.truncateWAL(ctx)
	}
	return nil
}

// GetSealedConnectorCompletion implements storagev1.SealedConnectorConsentStore.
func (s *Store) GetSealedConnectorCompletion(ctx context.Context, nonce string) (*storagev1.SealedConnectorCompletion, error) {
	s.finishPendingWALTruncate(ctx)
	if strings.TrimSpace(nonce) == "" {
		return nil, store.ErrNotFound
	}
	completion, err := scanConnectorCompletion(s.db.QueryRowContext(ctx, `SELECT `+connectorCompletionColumns+`
		FROM connector_completions WHERE nonce = ?`, nonce))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read connector completion: %w", err)
	}
	return &completion, nil
}

// ConsumeSealedConnectorCompletion implements storagev1.SealedConnectorConsentStore.
func (s *Store) ConsumeSealedConnectorCompletion(ctx context.Context, nonce string) (*storagev1.SealedConnectorCompletion, error) {
	s.finishPendingWALTruncate(ctx)
	if strings.TrimSpace(nonce) == "" {
		return nil, store.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin connector completion transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	completion, err := scanConnectorCompletion(tx.QueryRowContext(ctx, `SELECT `+connectorCompletionColumns+`
		FROM connector_completions WHERE nonce = ?`, nonce))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read connector completion: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_completions WHERE nonce = ?`, nonce); err != nil {
		return nil, fmt.Errorf("consume connector completion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit connector completion: %w", err)
	}
	// The consumed row held sealed tokens: its log copy goes too.
	_ = s.truncateWAL(ctx)
	return &completion, nil
}

// CommitSealedConnectorCompletion implements storagev1.SealedConnectorConsentStore.
func (s *Store) CommitSealedConnectorCompletion(
	ctx context.Context, request storagev1.CommitSealedConnectorCompletionRequest,
) (*storagev1.SealedConnectorCredential, error) {
	s.finishPendingWALTruncate(ctx)
	if err := validateConnectorCommit(request); err != nil {
		return nil, err
	}
	uid := request.Credential.ConnectionUID
	now := request.Now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin connector completion commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	parked, current, err := connectorCommitBasisTx(ctx, tx, request)
	if err != nil {
		return nil, err
	}
	// The token's lifetime is judged at the commit too: a commit that stalled
	// past the deadline redeems nothing.
	if !parked.ExpiresAt.After(now) {
		return nil, store.ErrNotFound
	}
	// A newer consent's grant is custody already: this older consent's
	// tokens never replace it.
	if _, committed, err := connectorConsentSequencesTx(ctx, tx, uid); err != nil {
		return nil, err
	} else if parked.ConsentSequence <= committed {
		return nil, store.ErrConnectorConsentSuperseded
	}
	// Grants a re-consent superseded are kept so disconnect can revoke them;
	// a bound keeps repeated relinking from growing custody without limit.
	// Records with nothing left to revoke are pruned first, so they never
	// count.
	if err := pruneRetiredConnectorCredentialsTx(ctx, tx, uid, now); err != nil {
		return nil, err
	}
	var retained int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_retired_credentials WHERE connection_uid = ? AND consent_grant = 1`,
		uid).Scan(&retained); err != nil {
		return nil, fmt.Errorf("count retired connector credentials: %w", err)
	}
	if retained >= maxRetiredConnectorCredentials {
		return nil, store.ErrConnectorRetiredLimit
	}
	if tombstoned, err := connectorTombstonedTx(ctx, tx, uid); err != nil {
		return nil, err
	} else if tombstoned {
		return nil, store.ErrConnectorCustodyTombstoned
	}
	if request.Retire != nil {
		if err := s.retireConnectorCredentialTx(ctx, tx, *current, request.Retire.RevocableUntil, true, now); err != nil {
			return nil, err
		}
	}
	stored, err := s.putConnectorCredentialTx(ctx, tx, request.Credential, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connector_completions SET payload_nonce = ?, payload = ? WHERE nonce = ?`,
		request.PayloadNonce, request.Payload, request.Nonce); err != nil {
		return nil, fmt.Errorf("mark connector completion committed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_consent_sequences (connection_uid, issued, committed) VALUES (?, ?, ?)
		ON CONFLICT(connection_uid) DO UPDATE SET committed = excluded.committed, issued = MAX(issued, excluded.issued)`,
		uid, parked.ConsentSequence, parked.ConsentSequence); err != nil {
		return nil, fmt.Errorf("record committed connector consent: %w", err)
	}
	for _, nonce := range request.Drop {
		if _, err := tx.ExecContext(ctx, `DELETE FROM connector_completions WHERE nonce = ?`, nonce); err != nil {
			return nil, fmt.Errorf("drop superseded connector completion: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(request.Drop) > 0 {
		// The commit stands either way: a log that readers kept busy is
		// truncated by the next custody deletion.
		_ = s.truncateWAL(ctx)
	}
	return stored, nil
}

// connectorCommitBasisTx checks that what Orka read still holds: the parked
// completions it judged, the custody it decided to retire and the grant it
// sealed. It returns the completion being committed and the current custody.
func connectorCommitBasisTx(
	ctx context.Context, tx *sql.Tx, request storagev1.CommitSealedConnectorCompletionRequest,
) (storagev1.SealedConnectorCompletion, *storagev1.SealedConnectorCredential, error) {
	uid := request.Credential.ConnectionUID
	completions, err := connectorCompletionsTx(ctx, tx, uid)
	if err != nil {
		return storagev1.SealedConnectorCompletion{}, nil, err
	}
	if err := requireConnectorCompletionSet(completions, request.ExpectedCompletions); err != nil {
		return storagev1.SealedConnectorCompletion{}, nil, err
	}
	var parked storagev1.SealedConnectorCompletion
	for _, completion := range completions {
		if completion.Nonce == request.Nonce {
			parked = completion
		}
	}
	current, err := connectorCredentialTx(ctx, tx, uid)
	if err != nil {
		return storagev1.SealedConnectorCompletion{}, nil, err
	}
	switch {
	case current == nil && request.ExpectedCredentialVersion != 0,
		current != nil && current.Version != request.ExpectedCredentialVersion:
		return storagev1.SealedConnectorCompletion{}, nil, store.ConflictErrorf("connector custody changed")
	case current == nil && request.Retire != nil:
		return storagev1.SealedConnectorCompletion{}, nil, store.ValidationErrorf("a commit can only retire existing custody")
	}
	last, err := connectorLastGrantTx(ctx, tx, uid)
	if err != nil {
		return storagev1.SealedConnectorCompletion{}, nil, err
	}
	if request.Credential.GrantSequence != last+1 {
		return storagev1.SealedConnectorCompletion{}, nil, store.ConflictErrorf("connector grant sequence %d is not the next grant", request.Credential.GrantSequence)
	}
	return parked, current, nil
}

func validateConnectorCommit(request storagev1.CommitSealedConnectorCompletionRequest) error {
	if strings.TrimSpace(request.Nonce) == "" {
		return store.ValidationErrorf("connector completion nonce is required")
	}
	if len(request.PayloadNonce) == 0 || len(request.Payload) == 0 {
		return store.ValidationErrorf("committed connector completion requires a sealed payload")
	}
	if err := request.Credential.Validate(); err != nil {
		return err
	}
	if err := requireConnectorRequestTime(request.Now); err != nil {
		return err
	}
	if request.ExpectedCredentialVersion < 0 {
		return store.ValidationErrorf("expected connector credential version must not be negative")
	}
	if err := validateConnectorCompletionRefs(request.ExpectedCompletions, append([]string{request.Nonce}, request.Drop...), "committed and dropped completions"); err != nil {
		return err
	}
	if slices.Contains(request.Drop, request.Nonce) {
		return store.ValidationErrorf("a commit cannot drop the completion it commits")
	}
	return nil
}

// DeleteConnectorCompletion implements storagev1.SealedConnectorConsentStore.
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

// DeleteConnectorConsentsForConnection implements storagev1.SealedConnectorConsentStore.
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

// ListSealedConnectorCredentials implements storagev1.SealedConnectorCustodyStore.
func (s *Store) ListSealedConnectorCredentials(ctx context.Context, cursor string, limit int) (*storagev1.SealedConnectorCredentialPage, error) {
	after, err := connectorPageStart(cursor, limit)
	if err != nil {
		return nil, err
	}
	records, err := queryConnectorRecords(ctx, s.db, "sealed connector credentials", scanConnectorCredential,
		`SELECT `+connectorCredentialColumns+` FROM connector_credentials
		WHERE ? = '' OR connection_uid > ? ORDER BY connection_uid LIMIT ?`, cursor, after, limit+1)
	if err != nil {
		return nil, err
	}
	page := &storagev1.SealedConnectorCredentialPage{}
	page.Credentials, page.NextCursor = connectorPage(records, limit, func(record storagev1.SealedConnectorCredential) string {
		return encodeKeysetCursor(record.ConnectionUID, "")
	})
	return page, nil
}

// ListSealedRetiredConnectorCredentials implements storagev1.SealedConnectorCustodyStore.
func (s *Store) ListSealedRetiredConnectorCredentials(ctx context.Context, cursor string, limit int) (*storagev1.SealedRetiredConnectorCredentialPage, error) {
	if err := validateSealedPageLimit(limit); err != nil {
		return nil, err
	}
	afterUID, afterIDText, err := decodeKeysetCursor(cursor)
	if err != nil {
		return nil, err
	}
	var afterID int64
	if cursor != "" {
		if afterID, err = strconv.ParseInt(afterIDText, 10, 64); err != nil {
			return nil, store.ValidationErrorf("invalid page cursor")
		}
	}
	type retiredRow struct {
		id      int64
		retired storagev1.SealedRetiredConnectorCredential
	}
	records, err := queryConnectorRecords(ctx, s.db, "sealed retired connector credentials", func(row connectorRowScanner) (retiredRow, error) {
		var record retiredRow
		retired, err := scanConnectorRetired(idScanner{row: row, id: &record.id})
		record.retired = retired
		return record, err
	}, `SELECT id, `+connectorRetiredColumns+` FROM connector_retired_credentials
		WHERE ? = '' OR connection_uid > ? OR (connection_uid = ? AND id > ?)
		ORDER BY connection_uid, id LIMIT ?`, cursor, afterUID, afterUID, afterID, limit+1)
	if err != nil {
		return nil, err
	}
	rows, next := connectorPage(records, limit, func(record retiredRow) string {
		return encodeKeysetCursor(record.retired.ConnectionUID, strconv.FormatInt(record.id, 10))
	})
	page := &storagev1.SealedRetiredConnectorCredentialPage{Retired: make([]storagev1.SealedRetiredConnectorCredential, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		page.Retired = append(page.Retired, row.retired)
	}
	return page, nil
}

// idScanner scans a leading row ID before the columns another scanner reads.
type idScanner struct {
	row connectorRowScanner
	id  *int64
}

func (s idScanner) Scan(dest ...any) error {
	return s.row.Scan(append([]any{s.id}, dest...)...)
}

// ListSealedConnectorConsents implements storagev1.SealedConnectorConsentStore.
func (s *Store) ListSealedConnectorConsents(ctx context.Context, cursor string, limit int) (*storagev1.SealedConnectorConsentPage, error) {
	after, err := connectorPageStart(cursor, limit)
	if err != nil {
		return nil, err
	}
	records, err := queryConnectorRecords(ctx, s.db, "sealed connector consents", scanConnectorConsent,
		`SELECT `+connectorConsentColumns+` FROM connector_consents
		WHERE ? = '' OR nonce > ? ORDER BY nonce LIMIT ?`, cursor, after, limit+1)
	if err != nil {
		return nil, err
	}
	page := &storagev1.SealedConnectorConsentPage{}
	page.Consents, page.NextCursor = connectorPage(records, limit, func(record storagev1.SealedConnectorConsent) string {
		return encodeKeysetCursor(record.Nonce, "")
	})
	return page, nil
}

// ListSealedConnectorCompletions implements storagev1.SealedConnectorConsentStore.
func (s *Store) ListSealedConnectorCompletions(ctx context.Context, cursor string, limit int) (*storagev1.SealedConnectorCompletionPage, error) {
	after, err := connectorPageStart(cursor, limit)
	if err != nil {
		return nil, err
	}
	records, err := queryConnectorRecords(ctx, s.db, "sealed connector completions", scanConnectorCompletion,
		`SELECT `+connectorCompletionColumns+` FROM connector_completions
		WHERE ? = '' OR nonce > ? ORDER BY nonce LIMIT ?`, cursor, after, limit+1)
	if err != nil {
		return nil, err
	}
	page := &storagev1.SealedConnectorCompletionPage{}
	page.Completions, page.NextCursor = connectorPage(records, limit, func(record storagev1.SealedConnectorCompletion) string {
		return encodeKeysetCursor(record.Nonce, "")
	})
	return page, nil
}

// connectorPageStart validates a page request keyed by one string and
// returns the key the page starts after.
func connectorPageStart(cursor string, limit int) (string, error) {
	if err := validateSealedPageLimit(limit); err != nil {
		return "", err
	}
	after, second, err := decodeKeysetCursor(cursor)
	if err != nil {
		return "", err
	}
	if cursor != "" && (after == "" || second != "") {
		return "", store.ValidationErrorf("invalid page cursor")
	}
	return after, nil
}

// connectorPage trims records read with one extra probe row to limit and
// returns the cursor after the last kept record when more follow. Connector
// records are small, so the probe row is read whole.
func connectorPage[T any](records []T, limit int, key func(T) string) ([]T, string) {
	if len(records) <= limit {
		return append(make([]T, 0, len(records)), records...), ""
	}
	records = records[:limit]
	return records, key(records[limit-1])
}
