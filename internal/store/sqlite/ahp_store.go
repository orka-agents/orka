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
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

var _ store.AHPStore = (*Store)(nil)

// ahpSchemaStatements defines Agent Host Protocol tokens, client records and
// each user's sequence, replay journal, channel states and session catalog.
func ahpSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS ahp_tokens (
			id          TEXT PRIMARY KEY,
			token_hash  TEXT NOT NULL UNIQUE,
			user_key    TEXT NOT NULL,
			issuer      TEXT NOT NULL,
			subject     TEXT NOT NULL,
			username    TEXT NOT NULL,
			namespace   TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMP NOT NULL,
			expires_at  TIMESTAMP NOT NULL,
			revoked_at  TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ahp_tokens_user
			ON ahp_tokens(user_key, created_at)`,
		`CREATE TABLE IF NOT EXISTS ahp_clients (
			client_id        TEXT PRIMARY KEY,
			user_key         TEXT NOT NULL,
			protocol_version TEXT NOT NULL,
			dialect          TEXT NOT NULL,
			created_at       TIMESTAMP NOT NULL,
			updated_at       TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ahp_users (
			user_key   TEXT PRIMARY KEY,
			server_seq INTEGER NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ahp_journal (
			user_key TEXT NOT NULL,
			seq      INTEGER NOT NULL,
			channel  TEXT NOT NULL,
			envelope BLOB NOT NULL,
			size     INTEGER NOT NULL,
			PRIMARY KEY (user_key, seq)
		)`,
		`CREATE TABLE IF NOT EXISTS ahp_channel_states (
			user_key TEXT NOT NULL,
			channel  TEXT NOT NULL,
			state    BLOB NOT NULL,
			from_seq INTEGER NOT NULL,
			PRIMARY KEY (user_key, channel)
		)`,
		`CREATE TABLE IF NOT EXISTS ahp_sessions (
			user_key    TEXT NOT NULL,
			session     TEXT NOT NULL,
			backend     TEXT NOT NULL,
			summary     BLOB NOT NULL,
			created_at  TIMESTAMP NOT NULL,
			modified_at TIMESTAMP NOT NULL,
			PRIMARY KEY (user_key, session)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ahp_sessions_modified
			ON ahp_sessions(user_key, modified_at)`,
		`CREATE TABLE IF NOT EXISTS ahp_records (
			user_key   TEXT NOT NULL,
			kind       TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      BLOB NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (user_key, kind, key)
		)`,
	}
}

// CreateAHPToken stores a new token. The hash must be unique.
func (s *Store) CreateAHPToken(ctx context.Context, token store.AHPToken) error {
	if strings.TrimSpace(token.ID) == "" || strings.TrimSpace(token.TokenHash) == "" || strings.TrimSpace(token.UserKey) == "" {
		return fmt.Errorf("%w: AHP token id, hash and user are required", store.ErrValidation)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ahp_tokens
		(id, token_hash, user_key, issuer, subject, username, namespace, description, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token.ID, token.TokenHash, token.UserKey, token.Issuer, token.Subject, token.Username, token.Namespace,
		token.Description, token.CreatedAt.UTC(), token.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("create AHP token: %w", err)
	}
	return nil
}

const ahpTokenColumns = `id, token_hash, user_key, issuer, subject, username, namespace, description,
	created_at, expires_at, revoked_at`

func scanAHPToken(row interface{ Scan(...any) error }) (store.AHPToken, error) {
	var token store.AHPToken
	var revoked sql.NullTime
	if err := row.Scan(&token.ID, &token.TokenHash, &token.UserKey, &token.Issuer, &token.Subject, &token.Username,
		&token.Namespace, &token.Description, &token.CreatedAt, &token.ExpiresAt, &revoked); err != nil {
		return store.AHPToken{}, err
	}
	if revoked.Valid {
		at := revoked.Time
		token.RevokedAt = &at
	}
	return token, nil
}

// GetAHPTokenByHash returns the token with the given secret hash.
func (s *Store) GetAHPTokenByHash(ctx context.Context, tokenHash string) (store.AHPToken, error) {
	token, err := scanAHPToken(s.db.QueryRowContext(ctx,
		`SELECT `+ahpTokenColumns+` FROM ahp_tokens WHERE token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return store.AHPToken{}, store.ErrNotFound
	}
	if err != nil {
		return store.AHPToken{}, fmt.Errorf("get AHP token: %w", err)
	}
	return token, nil
}

// ListAHPTokens returns a user's tokens, newest first.
func (s *Store) ListAHPTokens(ctx context.Context, userKey string) ([]store.AHPToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ahpTokenColumns+` FROM ahp_tokens WHERE user_key = ? ORDER BY created_at DESC, id`, userKey)
	if err != nil {
		return nil, fmt.Errorf("list AHP tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tokens []store.AHPToken
	for rows.Next() {
		token, err := scanAHPToken(rows)
		if err != nil {
			return nil, fmt.Errorf("list AHP tokens: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// RevokeAHPToken marks one of the user's tokens revoked. Revoking twice keeps
// the first time.
func (s *Store) RevokeAHPToken(ctx context.Context, userKey, id string, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE ahp_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND user_key = ?`,
		at.UTC(), id, userKey)
	if err != nil {
		return fmt.Errorf("revoke AHP token: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetAHPClient returns a client record.
func (s *Store) GetAHPClient(ctx context.Context, clientID string) (store.AHPClient, error) {
	var client store.AHPClient
	err := s.db.QueryRowContext(ctx, `SELECT client_id, user_key, protocol_version, dialect, created_at, updated_at
		FROM ahp_clients WHERE client_id = ?`, clientID).
		Scan(&client.ClientID, &client.UserKey, &client.ProtocolVersion, &client.Dialect, &client.CreatedAt, &client.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AHPClient{}, store.ErrNotFound
	}
	if err != nil {
		return store.AHPClient{}, fmt.Errorf("get AHP client: %w", err)
	}
	return client, nil
}

// PutAHPClient records a client for its user, or updates what it negotiated.
// A clientId stays bound to the first user that used it.
func (s *Store) PutAHPClient(ctx context.Context, client store.AHPClient) error {
	if strings.TrimSpace(client.ClientID) == "" || strings.TrimSpace(client.UserKey) == "" {
		return fmt.Errorf("%w: AHP client id and user are required", store.ErrValidation)
	}
	now := client.UpdatedAt.UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO ahp_clients
		(client_id, user_key, protocol_version, dialect, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET protocol_version = excluded.protocol_version,
			dialect = excluded.dialect, updated_at = excluded.updated_at
		WHERE ahp_clients.user_key = excluded.user_key`,
		client.ClientID, client.UserKey, client.ProtocolVersion, client.Dialect, now, now)
	if err != nil {
		return fmt.Errorf("put AHP client: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return store.ErrConflict
	}
	return nil
}

// AHPServerSeq returns the user's last assigned sequence number.
func (s *Store) AHPServerSeq(ctx context.Context, userKey string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT server_seq FROM ahp_users WHERE user_key = ?`, userKey).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get AHP sequence: %w", err)
	}
	return seq, nil
}

// AppendAHPJournal appends numbered entries, stores channel states and prunes
// the journal in one transaction.
func (s *Store) AppendAHPJournal(
	ctx context.Context,
	userKey string,
	previousSeq int64,
	entries []store.AHPJournalEntry,
	states []store.AHPChannelState,
	limits store.AHPJournalLimits,
) error {
	for i, entry := range entries {
		if entry.Seq != previousSeq+int64(i)+1 {
			return fmt.Errorf("%w: AHP journal entries must be numbered consecutively", store.ErrValidation)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var current int64
	err = tx.QueryRowContext(ctx, `SELECT server_seq FROM ahp_users WHERE user_key = ?`, userKey).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read AHP sequence: %w", err)
	}
	if current != previousSeq {
		return store.ErrConflict
	}
	last := previousSeq + int64(len(entries))
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO ahp_users (user_key, server_seq, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(user_key) DO UPDATE SET server_seq = excluded.server_seq, updated_at = excluded.updated_at`,
		userKey, last, now); err != nil {
		return fmt.Errorf("advance AHP sequence: %w", err)
	}
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ahp_journal (user_key, seq, channel, envelope, size) VALUES (?, ?, ?, ?, ?)`,
			userKey, entry.Seq, entry.Channel, entry.Envelope, len(entry.Envelope)); err != nil {
			return fmt.Errorf("append AHP journal: %w", err)
		}
	}
	for _, state := range states {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ahp_channel_states (user_key, channel, state, from_seq) VALUES (?, ?, ?, ?)
			ON CONFLICT(user_key, channel) DO UPDATE SET state = excluded.state, from_seq = excluded.from_seq`,
			userKey, state.Channel, state.State, state.FromSeq); err != nil {
			return fmt.Errorf("store AHP channel state: %w", err)
		}
	}
	if err := pruneAHPJournal(ctx, tx, userKey, limits); err != nil {
		return err
	}
	return tx.Commit()
}

// pruneAHPJournal keeps at most MaxEntries of the newest entries whose sizes
// total no more than MaxBytes.
func pruneAHPJournal(ctx context.Context, tx *sql.Tx, userKey string, limits store.AHPJournalLimits) error {
	if limits.MaxEntries > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ahp_journal WHERE user_key = ? AND seq <= (
			SELECT seq FROM ahp_journal WHERE user_key = ? ORDER BY seq DESC LIMIT 1 OFFSET ?)`,
			userKey, userKey, limits.MaxEntries); err != nil {
			return fmt.Errorf("prune AHP journal: %w", err)
		}
	}
	if limits.MaxBytes > 0 {
		var cutoff sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM (
			SELECT seq, SUM(size) OVER (ORDER BY seq DESC) AS kept FROM ahp_journal WHERE user_key = ?
		) WHERE kept > ?`, userKey, limits.MaxBytes).Scan(&cutoff); err != nil {
			return fmt.Errorf("prune AHP journal: %w", err)
		}
		if cutoff.Valid {
			if _, err := tx.ExecContext(ctx, `DELETE FROM ahp_journal WHERE user_key = ? AND seq <= ?`,
				userKey, cutoff.Int64); err != nil {
				return fmt.Errorf("prune AHP journal: %w", err)
			}
		}
	}
	return nil
}

// ReadAHPJournal returns the entries after afterSeq, and whether none of them
// was pruned.
func (s *Store) ReadAHPJournal(ctx context.Context, userKey string, afterSeq int64) ([]store.AHPJournalEntry, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current int64
	err = tx.QueryRowContext(ctx, `SELECT server_seq FROM ahp_users WHERE user_key = ?`, userKey).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("read AHP sequence: %w", err)
	}
	if afterSeq < 0 || afterSeq > current {
		return nil, false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, channel, envelope FROM ahp_journal
		WHERE user_key = ? AND seq > ? ORDER BY seq`, userKey, afterSeq)
	if err != nil {
		return nil, false, fmt.Errorf("read AHP journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var entries []store.AHPJournalEntry
	for rows.Next() {
		var entry store.AHPJournalEntry
		if err := rows.Scan(&entry.Seq, &entry.Channel, &entry.Envelope); err != nil {
			return nil, false, fmt.Errorf("read AHP journal: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	want := current - afterSeq
	return entries, int64(len(entries)) == want && (len(entries) == 0 || entries[0].Seq == afterSeq+1), nil
}

// GetAHPChannelState returns the user's state for one channel.
func (s *Store) GetAHPChannelState(ctx context.Context, userKey, channel string) (store.AHPChannelState, error) {
	state := store.AHPChannelState{Channel: channel}
	err := s.db.QueryRowContext(ctx, `SELECT state, from_seq FROM ahp_channel_states WHERE user_key = ? AND channel = ?`,
		userKey, channel).Scan(&state.State, &state.FromSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AHPChannelState{}, store.ErrNotFound
	}
	if err != nil {
		return store.AHPChannelState{}, fmt.Errorf("get AHP channel state: %w", err)
	}
	return state, nil
}

// DeleteAHPChannelStates removes the user's states for the given channels.
func (s *Store) DeleteAHPChannelStates(ctx context.Context, userKey string, channels []string) error {
	for _, channel := range channels {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM ahp_channel_states WHERE user_key = ? AND channel = ?`,
			userKey, channel); err != nil {
			return fmt.Errorf("delete AHP channel state: %w", err)
		}
	}
	return nil
}

// ListAHPChannels returns the user's channels with state whose URI starts with prefix.
func (s *Store) ListAHPChannels(ctx context.Context, userKey, prefix string) ([]string, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := s.db.QueryContext(ctx, `SELECT channel FROM ahp_channel_states WHERE user_key = ? AND channel LIKE ? ESCAPE '\' ORDER BY channel`,
		userKey, escaped+"%")
	if err != nil {
		return nil, fmt.Errorf("list AHP channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var channels []string
	for rows.Next() {
		var channel string
		if err := rows.Scan(&channel); err != nil {
			return nil, fmt.Errorf("scan AHP channel: %w", err)
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

// PutAHPSession records or updates one entry in a user's session catalog.
func (s *Store) PutAHPSession(ctx context.Context, session store.AHPSession) error {
	if strings.TrimSpace(session.UserKey) == "" || strings.TrimSpace(session.Session) == "" {
		return fmt.Errorf("%w: AHP session user and URI are required", store.ErrValidation)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ahp_sessions (user_key, session, backend, summary, created_at, modified_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_key, session) DO UPDATE SET summary = excluded.summary, modified_at = excluded.modified_at`,
		session.UserKey, session.Session, session.Backend, session.Summary, session.CreatedAt.UTC(), session.ModifiedAt.UTC())
	if err != nil {
		return fmt.Errorf("put AHP session: %w", err)
	}
	return nil
}

// GetAHPSession returns one of the user's sessions.
func (s *Store) GetAHPSession(ctx context.Context, userKey, session string) (store.AHPSession, error) {
	record := store.AHPSession{UserKey: userKey, Session: session}
	err := s.db.QueryRowContext(ctx, `SELECT backend, summary, created_at, modified_at FROM ahp_sessions
		WHERE user_key = ? AND session = ?`, userKey, session).
		Scan(&record.Backend, &record.Summary, &record.CreatedAt, &record.ModifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AHPSession{}, store.ErrNotFound
	}
	if err != nil {
		return store.AHPSession{}, fmt.Errorf("get AHP session: %w", err)
	}
	return record, nil
}

// ListAHPSessions returns the user's sessions, most recently modified first.
func (s *Store) ListAHPSessions(ctx context.Context, userKey string) ([]store.AHPSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session, backend, summary, created_at, modified_at FROM ahp_sessions
		WHERE user_key = ? ORDER BY modified_at DESC, session`, userKey)
	if err != nil {
		return nil, fmt.Errorf("list AHP sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sessions []store.AHPSession
	for rows.Next() {
		record := store.AHPSession{UserKey: userKey}
		if err := rows.Scan(&record.Session, &record.Backend, &record.Summary, &record.CreatedAt, &record.ModifiedAt); err != nil {
			return nil, fmt.Errorf("list AHP sessions: %w", err)
		}
		sessions = append(sessions, record)
	}
	return sessions, rows.Err()
}

// DeleteAHPSession removes one of the user's sessions from the catalog.
func (s *Store) DeleteAHPSession(ctx context.Context, userKey, session string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ahp_sessions WHERE user_key = ? AND session = ?`,
		userKey, session); err != nil {
		return fmt.Errorf("delete AHP session: %w", err)
	}
	return nil
}

// PutAHPRecord stores one of a session backend's records for a user.
func (s *Store) PutAHPRecord(ctx context.Context, record store.AHPRecord) error {
	if strings.TrimSpace(record.UserKey) == "" || strings.TrimSpace(record.Kind) == "" || strings.TrimSpace(record.Key) == "" {
		return fmt.Errorf("%w: AHP record user, kind and key are required", store.ErrValidation)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ahp_records (user_key, kind, key, value, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_key, kind, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		record.UserKey, record.Kind, record.Key, record.Value, record.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("put AHP record: %w", err)
	}
	return nil
}

// GetAHPRecord returns one record, or ErrNotFound.
func (s *Store) GetAHPRecord(ctx context.Context, userKey, kind, key string) (store.AHPRecord, error) {
	record := store.AHPRecord{UserKey: userKey, Kind: kind, Key: key}
	err := s.db.QueryRowContext(ctx, `SELECT value, updated_at FROM ahp_records
		WHERE user_key = ? AND kind = ? AND key = ?`, userKey, kind, key).Scan(&record.Value, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AHPRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.AHPRecord{}, fmt.Errorf("get AHP record: %w", err)
	}
	return record, nil
}

// ListAHPRecords returns a user's records of one kind, ordered by key.
func (s *Store) ListAHPRecords(ctx context.Context, userKey, kind string) ([]store.AHPRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, updated_at FROM ahp_records
		WHERE user_key = ? AND kind = ? ORDER BY key`, userKey, kind)
	if err != nil {
		return nil, fmt.Errorf("list AHP records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []store.AHPRecord
	for rows.Next() {
		record := store.AHPRecord{UserKey: userKey, Kind: kind}
		if err := rows.Scan(&record.Key, &record.Value, &record.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list AHP records: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// DeleteAHPRecords removes a user's records of one kind whose key starts with prefix.
func (s *Store) DeleteAHPRecords(ctx context.Context, userKey, kind, prefix string) error {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ahp_records WHERE user_key = ? AND kind = ? AND key LIKE ? ESCAPE '\'`,
		userKey, kind, escaped+"%"); err != nil {
		return fmt.Errorf("delete AHP records: %w", err)
	}
	return nil
}

// DeleteAHPRecord removes one of a user's records.
func (s *Store) DeleteAHPRecord(ctx context.Context, userKey, kind, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ahp_records WHERE user_key = ? AND kind = ? AND key = ?`,
		userKey, kind, key); err != nil {
		return fmt.Errorf("delete AHP record: %w", err)
	}
	return nil
}
