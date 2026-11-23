package v1

import (
	"context"
	"time"
)

// AuthSession is an Orka sign-in session issued after a GitHub sign-in
// (ADR 0037). Only a hash of its token is stored, never the token itself.
type AuthSession struct {
	ID        string
	TokenHash string
	// Issuer and Subject are the person's verified identity: GitHub and
	// their numeric user ID, never their login.
	Issuer    string
	Subject   string
	Username  string
	Namespace string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// MaxAuthSessionsPerSubject bounds the live sign-in sessions one person
// holds: creating one beyond it ends their least recently issued one.
const MaxAuthSessionsPerSubject = 10

// AuthSessionStore keeps GitHub sign-in sessions. A session's ID, token hash,
// issuer and subject are required; a write without them returns
// ErrValidation.
type AuthSessionStore interface {
	// CreateAuthSession stores a session, drops every session that was
	// revoked or expired by the session's CreatedAt, and keeps the person's
	// MaxAuthSessionsPerSubject most recently issued sessions. It returns
	// ErrConflict, changing nothing, when the ID or the hash is already
	// stored.
	CreateAuthSession(ctx context.Context, session AuthSession) error
	// GetAuthSessionByHash returns ErrNotFound for an unknown hash.
	GetAuthSessionByHash(ctx context.Context, tokenHash string) (AuthSession, error)
	// RevokeAuthSession records when a session was revoked; revoking it
	// again keeps the first revocation time. It returns ErrNotFound for an
	// unknown session.
	RevokeAuthSession(ctx context.Context, id string, at time.Time) error
	// RenewAuthSession revokes a session live at at and stores its successor
	// in one transaction, after dropping every session that ended by at. It
	// returns ErrConflict, changing nothing, when the session is revoked,
	// expired or unknown, so one session renews at most once.
	RenewAuthSession(ctx context.Context, id string, next AuthSession, at time.Time) error
}
