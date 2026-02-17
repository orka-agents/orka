/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AuthHeader is the header name for authorization
	AuthHeader = "Authorization"

	// BearerPrefix is the prefix for bearer tokens
	BearerPrefix = "Bearer "

	// UserInfoContextKey is the context key for storing user info
	UserInfoContextKey = "userInfo"
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

// UserInfo contains information about the authenticated user
type UserInfo struct {
	Username  string
	UID       string
	Groups    []string
	Namespace string // Extracted from ServiceAccount username (system:serviceaccount:<ns>:<name>)
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

// NewAuthMiddleware creates a new authentication middleware
func NewAuthMiddleware(c client.Client) fiber.Handler {
	return func(ctx fiber.Ctx) error {
		// Get the authorization header
		authHeader := ctx.Get(AuthHeader)
		if authHeader == "" {
			log.Info("authentication failed: missing authorization header", "ip", ctx.IP())
			return fiber.NewError(fiber.StatusUnauthorized, "missing authorization header")
		}

		// Check for bearer token
		if !strings.HasPrefix(authHeader, BearerPrefix) {
			log.Info("authentication failed: invalid authorization header format", "ip", ctx.IP())
			return fiber.NewError(fiber.StatusUnauthorized, "invalid authorization header format")
		}

		token := strings.TrimPrefix(authHeader, BearerPrefix)
		if token == "" {
			log.Info("authentication failed: empty bearer token", "ip", ctx.IP())
			return fiber.NewError(fiber.StatusUnauthorized, "empty bearer token")
		}

		// Validate the token using TokenReview
		userInfo, err := validateToken(ctx.Context(), c, token)
		if err != nil {
			log.Error(err, "token validation failed")
			return fiber.NewError(fiber.StatusUnauthorized, "invalid token")
		}

		// Store user info in context
		ctx.Locals(UserInfoContextKey, userInfo)

		return ctx.Next()
	}
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
		Namespace: parseServiceAccountNamespace(review.Status.User.Username),
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
