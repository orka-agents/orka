/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/metrics"
)

const (
	// AuthHeader is the header name for authorization
	AuthHeader = "Authorization"

	// XAPIKeyHeader is the header name for the Anthropic-style API key
	XAPIKeyHeader = "x-api-key"

	// BearerPrefix is the prefix for bearer tokens
	BearerPrefix = "Bearer "

	// UserInfoContextKey is the context key for storing user info
	UserInfoContextKey = "userInfo"

	// AuthTypeTokenReview identifies Kubernetes TokenReview authentication.
	AuthTypeTokenReview = "tokenReview"

	// AuthTypeOIDC identifies OIDC JWT authentication.
	AuthTypeOIDC = "oidc"
)

type tokenCacheEntry struct {
	userInfo *UserInfo
	expiry   time.Time
}

var (
	tokenCache     sync.Map
	tokenCacheSize atomic.Int64
	cacheTTL       = 60 * time.Second
)

const tokenCacheCleanupInterval = 1000

// cleanupTokenCache removes expired entries from the token cache.
func cleanupTokenCache() {
	tokenCache.Range(func(key, value any) bool {
		if entry, ok := value.(*tokenCacheEntry); ok {
			if time.Now().After(entry.expiry) {
				tokenCache.Delete(key)
				tokenCacheSize.Add(-1)
			}
		}
		return true
	})
}

func getTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// UserInfo contains information about the authenticated user.
type UserInfo struct {
	Username  string
	UID       string
	Groups    []string
	Extra     map[string]authenticationv1.ExtraValue
	Namespace string // Extracted from ServiceAccount username (system:serviceaccount:<ns>:<name>)

	AuthType     string
	Subject      string
	Email        string
	Issuer       string
	Roles        []string
	ContextToken *ContextToken
}

// OIDCConfig holds OpenID Connect JWT validation settings.
type OIDCConfig struct {
	Issuer          string
	Audience        string
	JWKSURL         string
	AllowedSubjects []string
	Namespace       string
}

// Enabled reports whether OIDC authentication is configured.
func (c OIDCConfig) Enabled() bool {
	return c.Issuer != "" && c.Audience != ""
}

// AuthConfig holds authentication middleware configuration.
type AuthConfig struct {
	OIDC          OIDCConfig
	ContextTokens ContextTokenConfig

	// TokenSources optionally overrides the ordered request headers used to
	// extract authentication tokens. When empty, Authorization: Bearer is used
	// first and x-api-key remains the fallback.
	TokenSources []AuthTokenSource
}

// parseServiceAccountNamespace extracts the namespace from a ServiceAccount username.
// Format: system:serviceaccount:<namespace>:<name>
func parseServiceAccountNamespace(username string) string {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(username, prefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) < 2 || parts[0] == "" {
		return ""
	}
	return parts[0]
}

// NewAuthMiddleware creates a new authentication middleware.
func NewAuthMiddleware(c client.Client, configs ...AuthConfig) fiber.Handler {
	var cfg AuthConfig
	if len(configs) > 0 {
		cfg = configs[0]
	}

	tokenExtractor := AuthTokenExtractor{Sources: cfg.TokenSources}

	return func(ctx fiber.Ctx) error {
		contextToken, profile, ok, err := extractContextTokenCandidate(ctx, cfg.ContextTokens)
		if err != nil {
			log.Info("authentication failed: invalid context token header format", "ip", ctx.IP())
			return fiber.NewError(fiber.StatusUnauthorized, "invalid context token header format")
		}

		var userInfo *UserInfo
		if ok {
			userInfo, err = authenticateContextToken(ctx.Context(), contextToken, profile)
		} else {
			bearerContextToken, bearerErr := isUnconfiguredBearerContextToken(ctx, cfg.ContextTokens)
			if bearerErr != nil {
				log.Info("authentication failed: invalid authorization header format", "ip", ctx.IP())
				return fiber.NewError(fiber.StatusUnauthorized, "invalid authorization header format")
			}
			if bearerContextToken {
				log.Info("authentication failed: authorization bearer context token is not configured", "ip", ctx.IP())
				return fiber.NewError(fiber.StatusUnauthorized, "transaction tokens must be sent via the Txn-Token header unless Authorization: Bearer is explicitly enabled")
			}

			var token string
			token, err = tokenExtractor.Extract(ctx)
			if err != nil {
				if errors.Is(err, errInvalidAuthHeaderFormat) {
					log.Info("authentication failed: invalid authorization header format", "ip", ctx.IP())
					return fiber.NewError(fiber.StatusUnauthorized, "invalid authorization header format")
				}

				log.Info("authentication failed: missing authorization header", "ip", ctx.IP())
				return fiber.NewError(fiber.StatusUnauthorized, "missing authorization header")
			}

			userInfo, err = authenticateToken(ctx.Context(), c, token, cfg)
		}
		if err != nil {
			log.Error(err, "token validation failed")
			return fiber.NewError(fiber.StatusUnauthorized, "invalid token")
		}

		// Store user info in context
		ctx.Locals(UserInfoContextKey, userInfo)

		return ctx.Next()
	}
}

func authenticateToken(ctx context.Context, c client.Client, token string, cfg AuthConfig) (*UserInfo, error) {
	if !cfg.OIDC.Enabled() {
		return validateToken(ctx, c, token)
	}

	parsedOIDC, oidcErr := parseOIDCTokenCandidate(token, cfg.OIDC)
	if oidcErr == nil {
		userInfo, oidcErr := validateParsedOIDCToken(ctx, parsedOIDC, cfg.OIDC)
		if oidcErr == nil {
			return userInfo, nil
		}

		return nil, oidcErr
	}

	if c == nil {
		return nil, oidcErr
	}

	userInfo, tokenReviewErr := validateToken(ctx, c, token)
	if tokenReviewErr == nil {
		return userInfo, nil
	}

	return nil, fmt.Errorf("OIDC validation skipped: %w; TokenReview validation failed: %v", oidcErr, tokenReviewErr)
}

func authenticateContextToken(ctx context.Context, token string, profile ContextTokenProfileConfig) (*UserInfo, error) {
	contextToken, err := validateContextToken(ctx, token, profile)
	if err != nil {
		metrics.RecordContextTokenAuth(profile.Name, "failure")
		return nil, err
	}
	metrics.RecordContextTokenAuth(contextToken.Profile, "success")
	return contextTokenToUserInfo(contextToken), nil
}

// validateToken validates a ServiceAccount token using TokenReview with caching
func validateToken(ctx context.Context, c client.Client, token string) (*UserInfo, error) {
	hash := getTokenHash(token)

	// Check cache
	if entry, ok := tokenCache.Load(hash); ok {
		if cached := entry.(*tokenCacheEntry); time.Now().Before(cached.expiry) {
			return cached.userInfo, nil
		}
		tokenCache.Delete(hash)
	}

	// Create a TokenReview request
	review := &authenticationv1.TokenReview{
		ObjectMeta: metav1.ObjectMeta{
			Name: "orka-token-review",
		},
		Spec: authenticationv1.TokenReviewSpec{
			Token: token,
		},
	}

	// Submit the token review
	if err := c.Create(ctx, review); err != nil {
		return nil, err
	}

	// Check if the token is valid
	if !review.Status.Authenticated {
		tokenCache.Delete(hash)
		return nil, fiber.NewError(fiber.StatusUnauthorized, "token not authenticated")
	}

	userInfo := &UserInfo{
		Username:  review.Status.User.Username,
		UID:       review.Status.User.UID,
		Groups:    review.Status.User.Groups,
		Extra:     review.Status.User.Extra,
		Namespace: parseServiceAccountNamespace(review.Status.User.Username),
		AuthType:  AuthTypeTokenReview,
	}

	// Cache the successful result
	tokenCache.Store(hash, &tokenCacheEntry{
		userInfo: userInfo,
		expiry:   time.Now().Add(cacheTTL),
	})
	if tokenCacheSize.Add(1)%tokenCacheCleanupInterval == 0 {
		cleanupTokenCache()
	}

	return userInfo, nil
}

// GetUserInfo retrieves user info from the context
func GetUserInfo(ctx fiber.Ctx) *UserInfo {
	userInfo, ok := ctx.Locals(UserInfoContextKey).(*UserInfo)
	if !ok {
		return nil
	}
	return userInfo
}

// GetEffectiveNamespace returns the namespace to use for a request.
// Priority: explicit namespace > SA namespace from token > "default"
func GetEffectiveNamespace(ctx fiber.Ctx, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if ui := GetUserInfo(ctx); ui != nil && ui.Namespace != "" {
		return ui.Namespace
	}
	return defaultNamespace
}
