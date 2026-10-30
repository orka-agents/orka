/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package ahphost serves the Agent Host Protocol: connection tokens, the
// per-user host core and its journal, the WebSocket endpoint, and the session
// backends that run sessions.
package ahphost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/orka-agents/orka/internal/store"
)

const (
	// TokenPrefix marks AHP connection tokens so secret scanners can find them.
	TokenPrefix = "orka_ahp_"
	// DefaultTokenTTL is the lifetime of a token minted without one.
	DefaultTokenTTL = 30 * 24 * time.Hour
	// DefaultMaxTokenTTL caps any requested lifetime.
	DefaultMaxTokenTTL = 90 * 24 * time.Hour

	maxTokenLength       = 256
	maxDescriptionLength = 256
)

// ErrInvalidToken is returned for a missing, unknown, revoked or expired token.
var ErrInvalidToken = errors.New("invalid AHP connection token")

// ErrInvalidMintRequest is returned when a mint request is refused as asked.
var ErrInvalidMintRequest = errors.New("invalid AHP token request")

// Principal is the user behind an AHP connection.
type Principal struct {
	UserKey   string
	Issuer    string
	Subject   string
	Username  string
	Namespace string
	TokenID   string
	ExpiresAt time.Time

	tokenHash string
}

// UserKey derives the opaque key that scopes a user's AHP state.
func UserKey(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(sum[:])
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MintRequest describes a token to mint for a person signed in themselves.
type MintRequest struct {
	Issuer    string
	Subject   string
	Username  string
	Namespace string
	// TTL is the requested lifetime; zero means DefaultTokenTTL, or the
	// maximum when that is lower.
	TTL time.Duration
	// Description names the token for its owner, for example the device it
	// was pasted into.
	Description string
}

// MintedToken is a new token. The secret is returned only once.
type MintedToken struct {
	ID          string
	Token       string
	Description string
	ExpiresAt   time.Time
}

// TokenInfo describes a token without its secret.
type TokenInfo struct {
	ID          string
	Description string
	Namespace   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

// TokenService mints, verifies and revokes AHP connection tokens.
type TokenService struct {
	Store  store.AHPStore
	MaxTTL time.Duration
	Now    func() time.Time
	// OnRevoke is called after a token is revoked, to close its connections.
	OnRevoke func(tokenID string)
	// Admit, when set, decides whether a token's owner may still connect, for
	// example whether the OIDC subject allowlist still admits them. Tokens of
	// an owner it refuses are invalid, at the door and on every recheck.
	Admit func(Principal) error
}

func (s *TokenService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *TokenService) maxTTL() time.Duration {
	if s.MaxTTL > 0 {
		return s.MaxTTL
	}
	return DefaultMaxTokenTTL
}

// Mint creates a token for an authenticated person.
func (s *TokenService) Mint(ctx context.Context, req MintRequest) (MintedToken, error) {
	if strings.TrimSpace(req.Issuer) == "" || strings.TrimSpace(req.Subject) == "" {
		return MintedToken{}, fmt.Errorf("%w: an issuer and subject are required", ErrInvalidMintRequest)
	}
	description := strings.TrimSpace(req.Description)
	if len(description) > maxDescriptionLength {
		return MintedToken{}, fmt.Errorf("%w: description must be at most %d characters", ErrInvalidMintRequest, maxDescriptionLength)
	}
	if strings.IndexFunc(description, unicode.IsControl) >= 0 {
		return MintedToken{}, fmt.Errorf("%w: description must not contain control characters", ErrInvalidMintRequest)
	}
	ttl := req.TTL
	if ttl == 0 {
		// An omitted lifetime fits under a maximum set below the default.
		ttl = min(DefaultTokenTTL, s.maxTTL())
	}
	if ttl < time.Minute || ttl > s.maxTTL() {
		return MintedToken{}, fmt.Errorf("%w: ttl must be between 1m and %s", ErrInvalidMintRequest, s.maxTTL())
	}
	now := s.now()
	expires := now.Add(ttl)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return MintedToken{}, err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return MintedToken{}, err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	id := "ahpt_" + hex.EncodeToString(idBytes)
	if err := s.Store.CreateAHPToken(ctx, store.AHPToken{
		ID:          id,
		TokenHash:   hashToken(token),
		UserKey:     UserKey(req.Issuer, req.Subject),
		Issuer:      req.Issuer,
		Subject:     req.Subject,
		Username:    req.Username,
		Namespace:   req.Namespace,
		Description: description,
		CreatedAt:   now,
		ExpiresAt:   expires,
	}); err != nil {
		return MintedToken{}, err
	}
	return MintedToken{ID: id, Token: token, Description: description, ExpiresAt: expires}, nil
}

// Verify resolves a presented token to its principal.
func (s *TokenService) Verify(ctx context.Context, token string) (Principal, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, TokenPrefix) || len(token) > maxTokenLength {
		return Principal{}, ErrInvalidToken
	}
	return s.verifyHash(ctx, hashToken(token))
}

// Recheck confirms that a connection's token is still valid.
func (s *TokenService) Recheck(ctx context.Context, principal Principal) error {
	current, err := s.verifyHash(ctx, principal.tokenHash)
	if err != nil {
		return err
	}
	if current.TokenID != principal.TokenID || current.UserKey != principal.UserKey {
		return ErrInvalidToken
	}
	return nil
}

func (s *TokenService) verifyHash(ctx context.Context, hash string) (Principal, error) {
	record, err := s.Store.GetAHPTokenByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrInvalidToken
	}
	if err != nil {
		return Principal{}, err
	}
	if subtle.ConstantTimeCompare([]byte(record.TokenHash), []byte(hash)) != 1 ||
		record.RevokedAt != nil || !s.now().Before(record.ExpiresAt) ||
		record.UserKey != UserKey(record.Issuer, record.Subject) {
		return Principal{}, ErrInvalidToken
	}
	principal := Principal{
		UserKey:   record.UserKey,
		Issuer:    record.Issuer,
		Subject:   record.Subject,
		Username:  record.Username,
		Namespace: record.Namespace,
		TokenID:   record.ID,
		ExpiresAt: record.ExpiresAt,
		tokenHash: hash,
	}
	if s.Admit != nil && s.Admit(principal) != nil {
		return Principal{}, ErrInvalidToken
	}
	return principal, nil
}

// List returns a person's tokens without their secrets.
func (s *TokenService) List(ctx context.Context, issuer, subject string) ([]TokenInfo, error) {
	records, err := s.Store.ListAHPTokens(ctx, UserKey(issuer, subject))
	if err != nil {
		return nil, err
	}
	infos := make([]TokenInfo, 0, len(records))
	for _, record := range records {
		infos = append(infos, TokenInfo{
			ID: record.ID, Description: record.Description, Namespace: record.Namespace,
			CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt, RevokedAt: record.RevokedAt,
		})
	}
	return infos, nil
}

// Revoke revokes one of a person's tokens and closes its connections.
func (s *TokenService) Revoke(ctx context.Context, issuer, subject, id string) error {
	if err := s.Store.RevokeAHPToken(ctx, UserKey(issuer, subject), id, s.now()); err != nil {
		return err
	}
	if s.OnRevoke != nil {
		s.OnRevoke(id)
	}
	return nil
}
