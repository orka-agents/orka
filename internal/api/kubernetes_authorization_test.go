/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAuthorizeKubernetesTaskAccess_SubjectAccessReviewAllowed(t *testing.T) {
	handlers, app := setupSubjectAccessReviewTestHandlers(t, true)

	app.Get("/", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username:  "system:serviceaccount:team-a:caller",
			Groups:    []string{"system:serviceaccounts", "system:serviceaccounts:team-a"},
			Extra:     map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"caller"}},
			AuthType:  AuthTypeTokenReview,
			Namespace: "team-a",
		})
		return handlers.authorizeKubernetesTaskAccess(c, "create", "team-a", "task-a")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAuthorizeKubernetesTaskAccess_SubjectAccessReviewDenied(t *testing.T) {
	handlers, app := setupSubjectAccessReviewTestHandlers(t, false)

	app.Get("/", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:team-a:caller", AuthType: AuthTypeTokenReview})
		return handlers.authorizeKubernetesTaskAccess(c, "create", "team-b", "task-a")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func setupSubjectAccessReviewTestHandlers(t *testing.T, allowed bool) (*Handlers, *fiber.App) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, authorizationv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if sar, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
					require.Equal(t, "system:serviceaccount:team-a:caller", sar.Spec.User)
					require.NotNil(t, sar.Spec.ResourceAttributes)
					require.Equal(t, "core.orka.ai", sar.Spec.ResourceAttributes.Group)
					require.Equal(t, "tasks", sar.Spec.ResourceAttributes.Resource)
					require.Equal(t, "create", sar.Spec.ResourceAttributes.Verb)
					sar.Status.Allowed = allowed
					if !allowed {
						sar.Status.Reason = "denied by test"
					}
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()

	return NewHandlers(HandlersConfig{Client: fakeClient}), fiber.New()
}
