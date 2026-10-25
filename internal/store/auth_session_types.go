/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

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

// AuthSessionStore keeps sign-in sessions.
type AuthSessionStore interface {
	// CreateAuthSession stores a session, drops ended ones, and keeps a
	// person's ten most recently issued live sessions.
	CreateAuthSession(ctx context.Context, session AuthSession) error
	// GetAuthSessionByHash returns ErrNotFound for an unknown hash.
	GetAuthSessionByHash(ctx context.Context, tokenHash string) (AuthSession, error)
	// RevokeAuthSession returns ErrNotFound for an unknown session.
	RevokeAuthSession(ctx context.Context, id string, at time.Time) error
	// RenewAuthSession revokes a live session and stores its successor in
	// one transaction. It returns ErrConflict when the session is revoked
	// or expired, so one session renews at most once.
	RenewAuthSession(ctx context.Context, id string, next AuthSession, at time.Time) error
}
