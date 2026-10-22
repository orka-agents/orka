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

var _ store.AuthSessionStore = (*Store)(nil)

// authSessionSchemaStatements defines GitHub sign-in sessions (ADR 0037).
func authSessionSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			id         TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL UNIQUE,
			issuer     TEXT NOT NULL,
			subject    TEXT NOT NULL,
			username   TEXT NOT NULL,
			namespace  TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			revoked_at TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_sessions_subject
			ON auth_sessions(issuer, subject, created_at)`,
	}
}

func validAuthSession(session store.AuthSession) error {
	if strings.TrimSpace(session.ID) == "" || strings.TrimSpace(session.TokenHash) == "" ||
		strings.TrimSpace(session.Issuer) == "" || strings.TrimSpace(session.Subject) == "" {
		return fmt.Errorf("%w: sign-in session id, hash, issuer and subject are required", store.ErrValidation)
	}
	return nil
}

const insertAuthSession = `INSERT INTO auth_sessions
	(id, token_hash, issuer, subject, username, namespace, created_at, expires_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

func authSessionArgs(session store.AuthSession) []any {
	return []any{session.ID, session.TokenHash, session.Issuer, session.Subject, session.Username, session.Namespace,
		session.CreatedAt.UTC(), session.ExpiresAt.UTC()}
}

// CreateAuthSession stores a new session. The hash must be unique. Sessions
// that ended are dropped, and a person keeps their maxAuthSessionsPerSubject
// most recently issued ones.
func (s *Store) CreateAuthSession(ctx context.Context, session store.AuthSession) error {
	if err := validAuthSession(session); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create sign-in session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := pruneAuthSessionsTx(ctx, tx, session.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, insertAuthSession, authSessionArgs(session)...); err != nil {
		return fmt.Errorf("create sign-in session: %w", err)
	}
	// A person holds a bounded number of sessions; a sign-in beyond it ends
	// their least recently issued one.
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE issuer = ? AND subject = ? AND id NOT IN (
		SELECT id FROM auth_sessions WHERE issuer = ? AND subject = ? ORDER BY created_at DESC, rowid DESC LIMIT ?)`,
		session.Issuer, session.Subject, session.Issuer, session.Subject, maxAuthSessionsPerSubject); err != nil {
		return fmt.Errorf("bound sign-in sessions: %w", err)
	}
	return tx.Commit()
}

// maxAuthSessionsPerSubject bounds the live sessions one person holds.
const maxAuthSessionsPerSubject = 10

// pruneAuthSessionsTx drops sessions that ended before now: nothing accepts
// or renews them again.
func pruneAuthSessionsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE revoked_at IS NOT NULL OR expires_at <= ?`, now.UTC()); err != nil {
		return fmt.Errorf("prune sign-in sessions: %w", err)
	}
	return nil
}

// GetAuthSessionByHash returns the session with the given token hash.
func (s *Store) GetAuthSessionByHash(ctx context.Context, tokenHash string) (store.AuthSession, error) {
	var session store.AuthSession
	var revoked sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, token_hash, issuer, subject, username, namespace, created_at,
		expires_at, revoked_at FROM auth_sessions WHERE token_hash = ?`, tokenHash).Scan(&session.ID, &session.TokenHash,
		&session.Issuer, &session.Subject, &session.Username, &session.Namespace, &session.CreatedAt, &session.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuthSession{}, store.ErrNotFound
	}
	if err != nil {
		return store.AuthSession{}, fmt.Errorf("get sign-in session: %w", err)
	}
	if revoked.Valid {
		at := revoked.Time
		session.RevokedAt = &at
	}
	return session, nil
}

// RevokeAuthSession marks a session revoked. Revoking twice keeps the first
// time.
func (s *Store) RevokeAuthSession(ctx context.Context, id string, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE auth_sessions SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, at.UTC(), id)
	if err != nil {
		return fmt.Errorf("revoke sign-in session: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// RenewAuthSession revokes a live session and stores its successor
// atomically, so a session token renews at most once.
func (s *Store) RenewAuthSession(ctx context.Context, id string, next store.AuthSession, at time.Time) error {
	if err := validAuthSession(next); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("renew sign-in session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := pruneAuthSessionsTx(ctx, tx, at); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE auth_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL AND expires_at > ?`,
		at.UTC(), id, at.UTC())
	if err != nil {
		return fmt.Errorf("renew sign-in session: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: the sign-in session is no longer live", store.ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, insertAuthSession, authSessionArgs(next)...); err != nil {
		return fmt.Errorf("renew sign-in session: %w", err)
	}
	return tx.Commit()
}
