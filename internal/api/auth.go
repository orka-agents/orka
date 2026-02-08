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
	tokenCache sync.Map
	cacheTTL   = 60 * time.Second
)

func getTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// UserInfo contains information about the authenticated user
type UserInfo struct {
	Username string
	UID      string
	Groups   []string
}

// NewAuthMiddleware creates a new authentication middleware
func NewAuthMiddleware(c client.Client) fiber.Handler {
	return func(ctx fiber.Ctx) error {
		// Get the authorization header
		authHeader := ctx.Get(AuthHeader)
		if authHeader == "" {
			return fiber.NewError(fiber.StatusUnauthorized, "missing authorization header")
		}

		// Check for bearer token
		if !strings.HasPrefix(authHeader, BearerPrefix) {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid authorization header format")
		}

		token := strings.TrimPrefix(authHeader, BearerPrefix)
		if token == "" {
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
			Name: "mercan-token-review",
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
		Username: review.Status.User.Username,
		UID:      review.Status.User.UID,
		Groups:   review.Status.User.Groups,
	}

	// Cache the successful result
	tokenCache.Store(hash, &tokenCacheEntry{
		userInfo: userInfo,
		expiry:   time.Now().Add(cacheTTL),
	})

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
