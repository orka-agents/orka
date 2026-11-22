package v1

import (
	"context"
	"time"
)

type AuthSession struct {
	ID        string
	TokenHash string
	Issuer    string
	Subject   string
	Username  string
	Namespace string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

const MaxAuthSessionsPerSubject = 10

type AuthSessionStore interface {
	CreateAuthSession(ctx context.Context, session AuthSession) error
	GetAuthSessionByHash(ctx context.Context, tokenHash string) (AuthSession, error)
	RevokeAuthSession(ctx context.Context, id string, at time.Time) error
	RenewAuthSession(ctx context.Context, id string, next AuthSession, at time.Time) error
}
