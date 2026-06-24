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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testNamespace = "test-ns"

func setupTestApp(c client.Client) *fiber.App {
	app := fiber.New()
	app.Use(NewAuthMiddleware(c))
	app.Get("/test", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})
	return app
}

func TestNewAuthMiddleware_OIDCPreservesServiceAccountFallback(t *testing.T) {
	// Clear cache to avoid interference.
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	provider := newTestOIDCProvider(t)
	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	const serviceAccountToken = "valid-service-account-token-for-oidc-fallback"
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					if tr.Spec.Token != serviceAccountToken {
						t.Fatalf("TokenReview token = %q, want %q", tr.Spec.Token, serviceAccountToken)
					}
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{
						Username: "system:serviceaccount:test-ns:test-sa",
						UID:      "uid-fallback",
						Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:test-ns"},
					}
				}
				return nil
			},
		}).
		Build()

	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{OIDC: provider.config()}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		userInfo := GetUserInfo(ctx)
		if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview || userInfo.Namespace != testNamespace {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(AuthHeader, "Bearer "+serviceAccountToken)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestNewAuthMiddleware_MissingAuthHeader(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestNewAuthMiddleware_InvalidFormat(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz") // Not Bearer

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestNewAuthMiddleware_EmptyToken(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer ") // Empty token

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestGetUserInfo_ValidContext(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(ctx fiber.Ctx) error {
		// Set user info in context
		userInfo := &UserInfo{
			Username: "test-user",
			UID:      "uid-123",
			Groups:   []string{"group1", "group2"},
		}
		ctx.Locals(UserInfoContextKey, userInfo)

		// Get user info
		retrieved := GetUserInfo(ctx)
		if retrieved == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "user info is nil")
		}
		if retrieved.Username != "test-user" {
			return fiber.NewError(fiber.StatusInternalServerError, "username mismatch")
		}
		if retrieved.UID != "uid-123" {
			return fiber.NewError(fiber.StatusInternalServerError, "UID mismatch")
		}
		if len(retrieved.Groups) != 2 {
			return fiber.NewError(fiber.StatusInternalServerError, "groups mismatch")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestGetUserInfo_NilContext(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(ctx fiber.Ctx) error {
		// Don't set user info - should return nil
		retrieved := GetUserInfo(ctx)
		if retrieved != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "expected nil user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestGetUserInfo_WrongType(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(ctx fiber.Ctx) error {
		// Set wrong type in context
		ctx.Locals(UserInfoContextKey, "not a UserInfo")

		// Should return nil for wrong type
		retrieved := GetUserInfo(ctx)
		if retrieved != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "expected nil for wrong type")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestUserInfo_Fields(t *testing.T) {
	userInfo := &UserInfo{
		Username: "system:serviceaccount:default:my-sa",
		UID:      "abc-123",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:default"},
	}

	if userInfo.Username != "system:serviceaccount:default:my-sa" {
		t.Errorf("Username = %s, want system:serviceaccount:default:my-sa", userInfo.Username)
	}
	if userInfo.UID != "abc-123" {
		t.Errorf("UID = %s, want abc-123", userInfo.UID)
	}
	if len(userInfo.Groups) != 2 {
		t.Errorf("Groups len = %d, want 2", len(userInfo.Groups))
	}
}

func TestConstants(t *testing.T) {
	if AuthHeader != "Authorization" {
		t.Errorf("AuthHeader = %s, want Authorization", AuthHeader)
	}
	if BearerPrefix != "Bearer " {
		t.Errorf("BearerPrefix = %s, want 'Bearer '", BearerPrefix)
	}
	if UserInfoContextKey != "userInfo" {
		t.Errorf("UserInfoContextKey = %s, want userInfo", UserInfoContextKey)
	}
}

func TestGetTokenHash(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"simple token", "my-token"},
		{"empty token", ""},
		{"long token", "eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMzQ1Njc4OTAifQ.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := getTokenHash(tt.token)

			// Verify it produces the correct SHA-256 hex digest
			expected := sha256.Sum256([]byte(tt.token))
			want := hex.EncodeToString(expected[:])
			if hash != want {
				t.Errorf("getTokenHash(%q) = %s, want %s", tt.token, hash, want)
			}

			// Verify determinism
			if hash2 := getTokenHash(tt.token); hash2 != hash {
				t.Errorf("getTokenHash not deterministic: %s != %s", hash, hash2)
			}
		})
	}

	// Different tokens produce different hashes
	h1 := getTokenHash("token-a")
	h2 := getTokenHash("token-b")
	if h1 == h2 {
		t.Error("different tokens should produce different hashes")
	}
}

func TestValidateToken_Authenticated(t *testing.T) {
	// Clear cache to avoid interference
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{
						Username: "system:serviceaccount:test-ns:test-sa",
						UID:      "uid-123",
						Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:test-ns"},
					}
				}
				return nil
			},
		}).
		Build()

	userInfo, err := validateToken(context.Background(), fakeClient, "valid-token")
	if err != nil {
		t.Fatalf("validateToken returned error: %v", err)
	}
	if userInfo.Username != "system:serviceaccount:test-ns:test-sa" {
		t.Errorf("Username = %s, want system:serviceaccount:test-ns:test-sa", userInfo.Username)
	}
	if userInfo.UID != "uid-123" {
		t.Errorf("UID = %s, want uid-123", userInfo.UID)
	}
	if userInfo.Namespace != testNamespace {
		t.Errorf("Namespace = %s, want test-ns", userInfo.Namespace)
	}
}

func TestValidateToken_CacheHit(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	callCount := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				callCount++
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{
						Username: "system:serviceaccount:ns:sa",
						UID:      "uid-cached",
					}
				}
				return nil
			},
		}).
		Build()

	// First call should hit the API
	_, err := validateToken(context.Background(), fakeClient, "cache-token")
	if err != nil {
		t.Fatalf("first validateToken failed: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 API call, got %d", callCount)
	}

	// Second call should use cache
	userInfo, err := validateToken(context.Background(), fakeClient, "cache-token")
	if err != nil {
		t.Fatalf("second validateToken failed: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected cache hit (1 API call total), got %d", callCount)
	}
	if userInfo.UID != "uid-cached" {
		t.Errorf("UID = %s, want uid-cached", userInfo.UID)
	}
}

func TestValidateToken_NotAuthenticated(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					tr.Status.Authenticated = false
				}
				return nil
			},
		}).
		Build()

	_, err := validateToken(context.Background(), fakeClient, "invalid-token")
	if err == nil {
		t.Fatal("expected error for unauthenticated token")
	}
}

func TestValidateToken_CreateError(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return errors.New("api server unavailable")
			},
		}).
		Build()

	_, err := validateToken(context.Background(), fakeClient, "err-token")
	if err == nil {
		t.Fatal("expected error when Create fails")
	}
}

func TestNewAuthMiddleware_ValidToken(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{
						Username: "system:serviceaccount:ns1:worker",
						UID:      "uid-valid",
						Groups:   []string{"system:serviceaccounts"},
					}
				}
				return nil
			},
		}).
		Build()

	var capturedUserInfo *UserInfo
	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient))
	app.Get("/test", func(ctx fiber.Ctx) error {
		capturedUserInfo = GetUserInfo(ctx)
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer valid-test-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if capturedUserInfo == nil {
		t.Fatal("expected UserInfo to be set in context")
	}
	if capturedUserInfo.Username != "system:serviceaccount:ns1:worker" {
		t.Errorf("Username = %s, want system:serviceaccount:ns1:worker", capturedUserInfo.Username)
	}
}

func TestNewAuthMiddleware_OIDCInvalidTokenFallsBackToTokenReview(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	createCalls := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					createCalls++
					if tr.Spec.Token == "service-account-token" {
						tr.Status.Authenticated = true
						tr.Status.User = authenticationv1.UserInfo{
							Username: "system:serviceaccount:ns1:fallback-sa",
							UID:      "uid-fallback",
							Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:ns1"},
						}
					}
				}
				return nil
			},
		}).
		Build()

	var capturedUserInfo *UserInfo
	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{OIDC: OIDCConfig{
		Issuer:   "https://issuer.example",
		Audience: "orka",
		JWKSURL:  "https://issuer.example/jwks",
	}}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		capturedUserInfo = GetUserInfo(ctx)
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer service-account-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if createCalls != 1 {
		t.Fatalf("TokenReview Create calls = %d, want 1", createCalls)
	}
	if capturedUserInfo == nil {
		t.Fatal("expected UserInfo to be set in context")
	}
	if capturedUserInfo.AuthType != AuthTypeTokenReview {
		t.Fatalf("AuthType = %q, want %q", capturedUserInfo.AuthType, AuthTypeTokenReview)
	}
	if capturedUserInfo.Namespace != "ns1" {
		t.Fatalf("Namespace = %q, want ns1", capturedUserInfo.Namespace)
	}
}

func TestNewAuthMiddleware_OIDCAuthorizationFailureSkipsTokenReviewFallback(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	provider := newTestOIDCProvider(t)
	cfg := provider.config()
	cfg.AllowedSubjects = []string{"repo:trusted-org/trusted-repo:*"}
	token := provider.issueToken(t, testOIDCTokenOptions{Subject: "repo:untrusted-org/untrusted-repo:ref:refs/heads/main"})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	createCalls := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					createCalls++
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{Username: "github:untrusted"}
				}
				return nil
			},
		}).
		Build()

	_, err := authenticateToken(context.Background(), fakeClient, token, AuthConfig{OIDC: cfg})
	if err == nil || !errors.Is(err, errOIDCAuthorization) {
		t.Fatalf("authenticateToken error = %v, want OIDC authorization error", err)
	}
	if createCalls != 0 {
		t.Fatalf("TokenReview Create calls = %d, want 0 for terminal OIDC authorization failure", createCalls)
	}
}

func TestNewAuthMiddleware_MalformedConfiguredNonAuthorizationContextTokenHeaderPreservesBearerFallback(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "X-Kontxt:Bearer")

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	const serviceAccountToken = "context-token-header-fallback-service-account-token"
	var tokenSeen string
	createCalls := 0

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					createCalls++
					tokenSeen = tr.Spec.Token
					if tr.Spec.Token == serviceAccountToken {
						tr.Status.Authenticated = true
						tr.Status.User = authenticationv1.UserInfo{
							Username: "system:serviceaccount:ns1:fallback-sa",
							UID:      "uid-context-token-header-fallback",
							Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:ns1"},
						}
					}
				}
				return nil
			},
		}).
		Build()

	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		userInfo := GetUserInfo(ctx)
		if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Kontxt", "Basic malformed-context-token-header")
	req.Header.Set("Authorization", "Bearer "+serviceAccountToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if createCalls != 1 {
		t.Fatalf("TokenReview Create calls = %d, want 1", createCalls)
	}
	if tokenSeen != serviceAccountToken {
		t.Fatalf("TokenReview token = %q, want %q", tokenSeen, serviceAccountToken)
	}
}

func TestNewAuthMiddleware_OIDCEnabledNonOIDCJWTUsesTokenReviewBeforeDiscovery(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{Issuer: "https://kubernetes.default.svc"})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	createCalls := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					createCalls++
					if tr.Spec.Token != token {
						t.Errorf("TokenReview token = %q, want original bearer token", tr.Spec.Token)
					}
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{
						Username: "system:serviceaccount:ns1:worker",
						UID:      "uid-worker",
						Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:ns1"},
					}
				}
				return nil
			},
		}).
		Build()

	var capturedUserInfo *UserInfo
	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{OIDC: provider.configWithoutJWKSURL()}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		capturedUserInfo = GetUserInfo(ctx)
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if createCalls != 1 {
		t.Fatalf("TokenReview Create calls = %d, want 1", createCalls)
	}
	if got := provider.discoveryHits.Load(); got != 0 {
		t.Fatalf("OIDC discovery hits = %d, want 0", got)
	}
	if got := provider.jwksHits.Load(); got != 0 {
		t.Fatalf("JWKS hits = %d, want 0", got)
	}
	if capturedUserInfo == nil || capturedUserInfo.AuthType != AuthTypeTokenReview {
		t.Fatalf("captured user info = %#v, want TokenReview user", capturedUserInfo)
	}
	if capturedUserInfo.Namespace != "ns1" {
		t.Fatalf("Namespace = %q, want ns1", capturedUserInfo.Namespace)
	}
}

func TestNewAuthMiddleware_OIDCEnabledNonOIDCTokenUsesCacheBeforeDiscovery(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{Issuer: "https://kubernetes.default.svc"})
	hash := getTokenHash(token)
	tokenCache.Store(hash, &tokenCacheEntry{
		userInfo: &UserInfo{
			Username:  "system:serviceaccount:ns1:cached-worker",
			Namespace: "ns1",
			AuthType:  AuthTypeTokenReview,
		},
		expiry: time.Now().Add(time.Minute),
	})
	t.Cleanup(func() { tokenCache.Delete(hash) })

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	createCalls := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				createCalls++
				return errors.New("TokenReview should not be called for cached token")
			},
		}).
		Build()

	var capturedUserInfo *UserInfo
	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{OIDC: provider.configWithoutJWKSURL()}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		capturedUserInfo = GetUserInfo(ctx)
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if createCalls != 0 {
		t.Fatalf("TokenReview Create calls = %d, want 0", createCalls)
	}
	if got := provider.discoveryHits.Load(); got != 0 {
		t.Fatalf("OIDC discovery hits = %d, want 0", got)
	}
	if got := provider.jwksHits.Load(); got != 0 {
		t.Fatalf("JWKS hits = %d, want 0", got)
	}
	if capturedUserInfo == nil || capturedUserInfo.Username != "system:serviceaccount:ns1:cached-worker" {
		t.Fatalf("captured user info = %#v, want cached user", capturedUserInfo)
	}
}

func TestNewAuthMiddleware_XAPIKeyOnly(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{
						Username: "system:serviceaccount:ns1:worker",
						UID:      "uid-xapi",
						Groups:   []string{"system:serviceaccounts"},
					}
				}
				return nil
			},
		}).
		Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("x-api-key", "xapi-test-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestNewAuthMiddleware_BothHeadersPrefersAuthorization(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					// Return different UIDs based on token to verify which was used
					switch tr.Spec.Token {
					case "bearer-token":
						tr.Status.Authenticated = true
						tr.Status.User = authenticationv1.UserInfo{
							Username: "system:serviceaccount:ns1:bearer-sa",
							UID:      "uid-bearer",
						}
					case "xapi-token":
						tr.Status.Authenticated = true
						tr.Status.User = authenticationv1.UserInfo{
							Username: "system:serviceaccount:ns1:xapi-sa",
							UID:      "uid-xapi",
						}
					}
				}
				return nil
			},
		}).
		Build()

	var capturedUserInfo *UserInfo
	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient))
	app.Get("/test", func(ctx fiber.Ctx) error {
		capturedUserInfo = GetUserInfo(ctx)
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer bearer-token")
	req.Header.Set("x-api-key", "xapi-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if capturedUserInfo == nil {
		t.Fatal("expected UserInfo to be set in context")
	}
	if capturedUserInfo.UID != "uid-bearer" {
		t.Errorf("UID = %s, want uid-bearer (Authorization header should take priority)", capturedUserInfo.UID)
	}
}

func TestNewAuthMiddleware_InvalidAuthorizationPreventsXAPIKeyFallback(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	createCalls := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				createCalls++
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					tr.Status.Authenticated = true
					tr.Status.User = authenticationv1.UserInfo{Username: "system:serviceaccount:ns:xapi-sa"}
				}
				return nil
			},
		}).
		Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	req.Header.Set("x-api-key", "xapi-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if createCalls != 0 {
		t.Fatalf("TokenReview Create calls = %d, want 0", createCalls)
	}
}

func TestNewAuthMiddleware_CustomTokenSource(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	var tokenSeen string
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					tokenSeen = tr.Spec.Token
					if tr.Spec.Token == "custom-source-token" {
						tr.Status.Authenticated = true
						tr.Status.User = authenticationv1.UserInfo{
							Username: "system:serviceaccount:ns1:custom-source",
							UID:      "uid-custom-source",
						}
					}
				}
				return nil
			},
		}).
		Build()

	app := fiber.New()
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{
		TokenSources: []AuthTokenSource{{Header: "X-Custom-Token"}},
	}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Custom-Token", "custom-source-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if tokenSeen != "custom-source-token" {
		t.Fatalf("TokenReview token = %q, want %q", tokenSeen, "custom-source-token")
	}
}

func TestNewAuthMiddleware_NeitherHeader(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestNewAuthMiddleware_TokenValidationFails(t *testing.T) {
	tokenCache.Range(func(key, _ any) bool {
		tokenCache.Delete(key)
		return true
	})

	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return errors.New("api server error")
			},
		}).
		Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestGetEffectiveNamespace(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		userInfo *UserInfo
		wantNS   string
	}{
		{
			name:     "explicit namespace takes priority",
			explicit: "my-ns",
			userInfo: &UserInfo{Namespace: "sa-ns"},
			wantNS:   "my-ns",
		},
		{
			name:     "falls back to SA namespace",
			explicit: "",
			userInfo: &UserInfo{Namespace: "sa-ns"},
			wantNS:   "sa-ns",
		},
		{
			name:     "falls back to default when no SA namespace",
			explicit: "",
			userInfo: &UserInfo{Namespace: ""},
			wantNS:   "default",
		},
		{
			name:     "falls back to default when no user info",
			explicit: "",
			userInfo: nil,
			wantNS:   "default",
		},
		{
			name:     "explicit empty string uses SA namespace",
			explicit: "",
			userInfo: &UserInfo{Namespace: "from-sa"},
			wantNS:   "from-sa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			var gotNS string
			app.Get("/test", func(ctx fiber.Ctx) error {
				if tt.userInfo != nil {
					ctx.Locals(UserInfoContextKey, tt.userInfo)
				}
				gotNS = GetEffectiveNamespace(ctx, tt.explicit)
				return ctx.SendString("OK")
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			_, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if gotNS != tt.wantNS {
				t.Errorf("GetEffectiveNamespace() = %s, want %s", gotNS, tt.wantNS)
			}
		})
	}
}

func TestParseServiceAccountNamespace(t *testing.T) {
	tests := []struct {
		name     string
		username string
		want     string
	}{
		{"valid SA username", "system:serviceaccount:my-ns:my-sa", "my-ns"},
		{"non-SA username", "admin", ""},
		{"partial prefix", "system:serviceaccount:", ""},
		{"missing name part", "system:serviceaccount:ns", ""},
		{"empty namespace", "system:serviceaccount::sa", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseServiceAccountNamespace(tt.username)
			if got != tt.want {
				t.Errorf("parseServiceAccountNamespace(%q) = %q, want %q", tt.username, got, tt.want)
			}
		})
	}
}
