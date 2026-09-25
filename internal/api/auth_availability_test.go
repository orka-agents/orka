package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestNewAuthMiddlewareTokenReviewUnavailableOptIn(t *testing.T) {
	for _, test := range []struct {
		name                                              string
		optIn, backendFailure, statusError, authenticated bool
		status                                            int
	}{
		{name: "default backend failure", backendFailure: true, status: 401},
		{name: "opt-in backend failure", optIn: true, backendFailure: true, status: 503},
		{name: "default status error", statusError: true, status: 401},
		{name: "opt-in status error", optIn: true, statusError: true, status: 503},
		{name: "default authenticated status error", statusError: true, authenticated: true, status: 401},
		{name: "opt-in authenticated status error", optIn: true, statusError: true, authenticated: true, status: 503},
		{name: "default invalid token", status: 401},
		{name: "opt-in invalid token", optIn: true, status: 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Capture the actual middleware logger, not only the returned HTTP body.
			var logs bytes.Buffer
			previousLog := log
			log = zap.New(zap.WriteTo(&logs))
			t.Cleanup(func() { log = previousLog })
			scheme := runtime.NewScheme()
			require.NoError(t, authenticationv1.AddToScheme(scheme))
			var reviews atomic.Int32
			kube := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if review, ok := obj.(*authenticationv1.TokenReview); ok {
						reviews.Add(1)
						if test.backendFailure {
							return errors.New("private backend diagnostic")
						}
						review.Status.Authenticated = test.authenticated
						if test.statusError {
							review.Status.Error = "private authenticator diagnostic"
						}
						return nil
					}
					return c.Create(ctx, obj, opts...)
				},
			}).Build()
			var authenticated, handled atomic.Bool
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				err := c.Next()
				authenticated.Store(GetUserInfo(c) != nil)
				return err
			})
			app.Use(NewAuthMiddleware(kube, AuthConfig{ReportTokenReviewUnavailable: test.optIn}))
			app.Get("/private", func(c fiber.Ctx) error { handled.Store(true); return c.SendStatus(200) })
			// None of these failures may cache an identity, including across repeats.
			token := t.Name()
			_, cached := tokenCache.Load(getTokenHash(token))
			require.False(t, cached)
			for attempt := int32(1); attempt <= 2; attempt++ {
				req := httptest.NewRequest(http.MethodGet, "/private", nil)
				req.Header.Set(AuthHeader, BearerPrefix+token)
				response, err := app.Test(req)
				require.NoError(t, err)
				body, err := io.ReadAll(response.Body)
				require.NoError(t, response.Body.Close())
				require.NoError(t, err)
				require.Equal(t, test.status, response.StatusCode)
				require.NotContains(t, string(body), "private")
				require.False(t, authenticated.Load())
				require.False(t, handled.Load())
				require.Equal(t, attempt, reviews.Load())
				_, cached = tokenCache.Load(getTokenHash(token))
				require.False(t, cached)
			}
			require.Contains(t, logs.String(), "token validation failed")
			require.NotContains(t, logs.String(), "private", "middleware logs must not expose opaque backend diagnostics")
			if test.backendFailure || test.statusError {
				user, err := validateToken(t.Context(), kube, token)
				require.Nil(t, user)
				require.ErrorIs(t, err, errTokenReviewUnavailable)
				require.NotContains(t, err.Error(), "private", "logged errors must not expose opaque backend diagnostics")
			}
		})
	}
}
