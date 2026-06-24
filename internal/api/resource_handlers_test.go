package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func allowResourceMutationRBAC(t *testing.T, handlers *Handlers, app *fiber.App) {
	t.Helper()
	clientset := kubefake.NewSimpleClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
	handlers.clientset = clientset
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeTokenReview, Username: "alice", UID: "uid-1"})
		return c.Next()
	})
}

func TestHandlers_ProviderCRUD(t *testing.T) {
	handlers, app := setupTestHandlers()
	allowResourceMutationRBAC(t, handlers, app)
	app.Get("/providers", handlers.ListProviders)
	app.Post("/providers", handlers.CreateProvider)
	app.Get("/providers/:name", handlers.GetProvider)
	app.Put("/providers/:name", handlers.UpdateProvider)
	app.Delete("/providers/:name", handlers.DeleteProvider)

	createBody := map[string]any{
		"metadata": map[string]any{
			"name":        "openai",
			"namespace":   "default",
			"labels":      map[string]any{"app.kubernetes.io/name": "orka"},
			"annotations": map[string]any{"example.com/source": "cli"},
		},
		"spec": map[string]any{
			"type":         "openai",
			"defaultModel": "gpt-4o-mini",
		},
	}
	resp := testJSONRequest(t, app, http.MethodPost, "/providers", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp = testJSONRequest(t, app, http.MethodGet, "/providers/openai", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got corev1alpha1.Provider
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "gpt-4o-mini", got.Spec.DefaultModel)
	require.Equal(t, map[string]string{"app.kubernetes.io/name": "orka"}, got.Labels)
	require.Equal(t, map[string]string{"example.com/source": "cli"}, got.Annotations)

	updateBody := map[string]any{
		"spec": map[string]any{
			"type":         "openai",
			"defaultModel": "gpt-4.1",
		},
	}
	resp = testJSONRequest(t, app, http.MethodPut, "/providers/openai", updateBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "gpt-4.1", got.Spec.DefaultModel)

	resp = testJSONRequest(t, app, http.MethodGet, "/providers", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list ListResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))

	resp = testJSONRequest(t, app, http.MethodDelete, "/providers/openai", nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestHandlers_ProviderUpdatePreservesExistingBaseURL(t *testing.T) {
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-provider", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			BaseURL:      "https://proxy.example/v1",
			DefaultModel: "gpt-4o-mini",
		},
	}
	handlers, app := setupTestHandlersWithObjects(provider)
	allowResourceMutationRBAC(t, handlers, app)
	app.Put("/providers/:name", handlers.UpdateProvider)

	resp := testJSONRequest(t, app, http.MethodPut, "/providers/proxy-provider", map[string]any{
		"spec": map[string]any{
			"type":         "openai",
			"baseURL":      "https://proxy.example/v1",
			"defaultModel": "gpt-4.1",
		},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var updated corev1alpha1.Provider
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&updated))
	require.Equal(t, "https://proxy.example/v1", updated.Spec.BaseURL)
	require.Equal(t, "gpt-4.1", updated.Spec.DefaultModel)

	resp = testJSONRequest(t, app, http.MethodPut, "/providers/proxy-provider", map[string]any{
		"spec": map[string]any{
			"type":         "openai",
			"baseURL":      "https://other-proxy.example/v1",
			"defaultModel": "gpt-4.1",
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlers_ToolWriteRejectsBuiltInAndCRUD(t *testing.T) {
	handlers, app := setupTestHandlers()
	allowResourceMutationRBAC(t, handlers, app)
	app.Post("/tools", handlers.CreateTool)
	app.Put("/tools/:name", handlers.UpdateTool)
	app.Delete("/tools/:name", handlers.DeleteTool)
	app.Get("/tools/:name", handlers.GetTool)

	builtinResp := testJSONRequest(t, app, http.MethodPost, "/tools", map[string]any{
		"name": "web_search",
		"spec": map[string]any{"description": "x"},
	})
	require.Equal(t, http.StatusConflict, builtinResp.StatusCode)

	body := map[string]any{
		"name": "http-tool",
		"spec": map[string]any{
			"description": "call service",
			"http":        map[string]any{"url": "https://example.com/tool", "method": "POST"},
		},
	}
	resp := testJSONRequest(t, app, http.MethodPost, "/tools", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	update := map[string]any{
		"spec": map[string]any{
			"description": "updated",
			"http":        map[string]any{"url": "https://example.com/new"},
		},
	}
	resp = testJSONRequest(t, app, http.MethodPut, "/tools/http-tool", update)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = testJSONRequest(t, app, http.MethodGet, "/tools/http-tool", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var tool corev1alpha1.Tool
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&tool))
	require.Equal(t, "updated", tool.Spec.Description)

	resp = testJSONRequest(t, app, http.MethodDelete, "/tools/http-tool", nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestHandlers_SubstrateActorPoolCRUD(t *testing.T) {
	handlers, app := setupTestHandlers()
	allowResourceMutationRBAC(t, handlers, app)
	app.Get("/substrate-actor-pools", handlers.ListSubstrateActorPools)
	app.Post("/substrate-actor-pools", handlers.CreateSubstrateActorPool)
	app.Get("/substrate-actor-pools/:name", handlers.GetSubstrateActorPool)
	app.Put("/substrate-actor-pools/:name", handlers.UpdateSubstrateActorPool)
	app.Delete("/substrate-actor-pools/:name", handlers.DeleteSubstrateActorPool)

	body := map[string]any{
		"metadata": map[string]any{"name": "pool-a"},
		"spec": map[string]any{
			"templateRef":  map[string]any{"name": "template-a"},
			"targetActors": 2,
		},
	}
	resp := testJSONRequest(t, app, http.MethodPost, "/substrate-actor-pools", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp = testJSONRequest(t, app, http.MethodGet, "/substrate-actor-pools/pool-a", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var pool corev1alpha1.SubstrateActorPool
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pool))
	require.Equal(t, int32(2), pool.Spec.TargetActors)

	update := map[string]any{
		"spec": map[string]any{
			"templateRef":  map[string]any{"name": "template-a"},
			"targetActors": 3,
		},
	}
	resp = testJSONRequest(t, app, http.MethodPut, "/substrate-actor-pools/pool-a", update)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = testJSONRequest(t, app, http.MethodDelete, "/substrate-actor-pools/pool-a", nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestHandlers_SubstrateActorPoolUpdateRetriesConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	pool := &corev1alpha1.SubstrateActorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "prod"},
		Spec: corev1alpha1.SubstrateActorPoolSpec{
			TemplateRef:   corev1alpha1.WorkspaceTemplateReference{Name: "template-a"},
			TargetActors:  1,
			TargetWorkers: 1,
		},
	}
	patchAttempts := 0
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(pool).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchAttempts++
				if patchAttempts == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "substrateactorpools"},
						obj.GetName(),
						fmt.Errorf("synthetic conflict"),
					)
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	clientset := kubefake.NewSimpleClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})

	handlers, app := setupTestHandlers()
	handlers.client = fakeClient
	handlers.clientset = clientset
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeTokenReview, Username: "alice"})
		return c.Next()
	})
	app.Put("/substrate-actor-pools/:name", handlers.UpdateSubstrateActorPool)

	resp := testJSONRequest(t, app, http.MethodPut, "/substrate-actor-pools/pool-a?namespace=prod", map[string]any{
		"spec": map[string]any{
			"templateRef":     map[string]any{"name": "template-a"},
			"targetActors":    2,
			"targetWorkers":   1,
			"precreateActors": true,
		},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, patchAttempts)

	var updated corev1alpha1.SubstrateActorPool
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&updated))
	require.Equal(t, int32(2), updated.Spec.TargetActors)
	require.True(t, updated.Spec.PrecreateActors)
}

func TestHandlers_SubstrateActorPoolUpdateReturnsConflictWhenSpecChangesDuringRetry(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	pool := &corev1alpha1.SubstrateActorPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "prod"},
		Spec: corev1alpha1.SubstrateActorPoolSpec{
			TemplateRef:   corev1alpha1.WorkspaceTemplateReference{Name: "template-a"},
			TargetActors:  1,
			TargetWorkers: 1,
		},
	}
	patchAttempts := 0
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(pool).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchAttempts++
				if patchAttempts == 1 {
					latest := &corev1alpha1.SubstrateActorPool{}
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), latest))
					latest.Spec.TargetActors = 9
					require.NoError(t, c.Update(ctx, latest))
					return apierrors.NewConflict(
						schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "substrateactorpools"},
						obj.GetName(),
						fmt.Errorf("synthetic conflict"),
					)
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	clientset := kubefake.NewSimpleClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})

	handlers, app := setupTestHandlers()
	handlers.client = fakeClient
	handlers.clientset = clientset
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeTokenReview, Username: "alice"})
		return c.Next()
	})
	app.Put("/substrate-actor-pools/:name", handlers.UpdateSubstrateActorPool)

	resp := testJSONRequest(t, app, http.MethodPut, "/substrate-actor-pools/pool-a?namespace=prod", map[string]any{
		"spec": map[string]any{
			"templateRef":     map[string]any{"name": "template-a"},
			"targetActors":    2,
			"targetWorkers":   1,
			"precreateActors": true,
		},
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, 1, patchAttempts)
}

func TestHandlers_ResourceMutationRequiresKubernetesRBAC(t *testing.T) {
	clientset := kubefake.NewSimpleClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		require.Empty(t, review.Name)
		require.Equal(t, "alice", review.Spec.User)
		require.Equal(t, "uid-1", review.Spec.UID)
		require.Equal(t, []string{"devs"}, review.Spec.Groups)
		require.Equal(t, authorizationv1.ExtraValue{"tenant-a", "tenant-b"}, review.Spec.Extra["example.com/tenant"])
		require.Equal(t, "prod", review.Spec.ResourceAttributes.Namespace)
		require.Empty(t, review.Spec.ResourceAttributes.Name)
		require.Equal(t, "create", review.Spec.ResourceAttributes.Verb)
		require.Equal(t, "core.orka.ai", review.Spec.ResourceAttributes.Group)
		require.Equal(t, "v1alpha1", review.Spec.ResourceAttributes.Version)
		require.Equal(t, "providers", review.Spec.ResourceAttributes.Resource)
		review.Status.Allowed = false
		review.Status.Reason = "rbac denied"
		return true, review, nil
	})

	handlers, app := setupTestHandlers()
	handlers.clientset = clientset
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			AuthType: AuthTypeTokenReview,
			Username: "alice",
			UID:      "uid-1",
			Groups:   []string{"devs"},
			Extra: map[string]authenticationv1.ExtraValue{
				"example.com/tenant": {"tenant-a", "tenant-b"},
			},
		})
		return c.Next()
	})
	app.Post("/providers", handlers.CreateProvider)

	resp := testJSONRequest(t, app, http.MethodPost, "/providers", map[string]any{
		"metadata": map[string]any{"name": "openai", "namespace": "prod"},
		"spec":     map[string]any{"type": "openai"},
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Len(t, clientset.Actions(), 1)
}

func TestHandlers_ResourceMutationRejectsMissingIdentity(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/providers", handlers.CreateProvider)

	resp := testJSONRequest(t, app, http.MethodPost, "/providers", map[string]any{
		"metadata": map[string]any{"name": "openai", "namespace": "prod"},
		"spec":     map[string]any{"type": "openai"},
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandlers_ResourceMutationUsesStableOIDCSubjectForSAR(t *testing.T) {
	clientset := kubefake.NewSimpleClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		require.Equal(t, "https://issuer.example/#repo:sozercan/orka:ref:refs/heads/main", review.Spec.User)
		review.Status.Allowed = true
		return true, review, nil
	})

	handlers, app := setupTestHandlers()
	handlers.clientset = clientset
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			AuthType: AuthTypeOIDC,
			Username: "mutable-display-name",
			Issuer:   "https://issuer.example/",
			Subject:  "repo:sozercan/orka:ref:refs/heads/main",
		})
		return c.Next()
	})
	app.Post("/providers", handlers.CreateProvider)

	resp := testJSONRequest(t, app, http.MethodPost, "/providers", map[string]any{
		"metadata": map[string]any{"name": "openai", "namespace": "prod"},
		"spec":     map[string]any{"type": "openai"},
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Len(t, clientset.Actions(), 1)
}

func TestHandlers_NamedResourceMutationAuthorizesBeforeLookup(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		path     string
		object   string
		resource string
		verb     string
		mount    func(*fiber.App, *Handlers)
	}{
		{
			name:     "provider update",
			method:   http.MethodPut,
			path:     "/providers/missing-provider?namespace=prod",
			object:   "missing-provider",
			resource: "providers",
			verb:     "update",
			mount: func(app *fiber.App, handlers *Handlers) {
				app.Put("/providers/:name", handlers.UpdateProvider)
			},
		},
		{
			name:     "provider delete",
			method:   http.MethodDelete,
			path:     "/providers/missing-provider?namespace=prod",
			object:   "missing-provider",
			resource: "providers",
			verb:     "delete",
			mount: func(app *fiber.App, handlers *Handlers) {
				app.Delete("/providers/:name", handlers.DeleteProvider)
			},
		},
		{
			name:     "tool update",
			method:   http.MethodPut,
			path:     "/tools/custom-tool?namespace=prod",
			object:   "custom-tool",
			resource: "tools",
			verb:     "update",
			mount: func(app *fiber.App, handlers *Handlers) {
				app.Put("/tools/:name", handlers.UpdateTool)
			},
		},
		{
			name:     "tool delete",
			method:   http.MethodDelete,
			path:     "/tools/custom-tool?namespace=prod",
			object:   "custom-tool",
			resource: "tools",
			verb:     "delete",
			mount: func(app *fiber.App, handlers *Handlers) {
				app.Delete("/tools/:name", handlers.DeleteTool)
			},
		},
		{
			name:     "substrate actor pool update",
			method:   http.MethodPut,
			path:     "/substrate-actor-pools/missing-pool?namespace=prod",
			object:   "missing-pool",
			resource: "substrateactorpools",
			verb:     "update",
			mount: func(app *fiber.App, handlers *Handlers) {
				app.Put("/substrate-actor-pools/:name", handlers.UpdateSubstrateActorPool)
			},
		},
		{
			name:     "substrate actor pool delete",
			method:   http.MethodDelete,
			path:     "/substrate-actor-pools/missing-pool?namespace=prod",
			object:   "missing-pool",
			resource: "substrateactorpools",
			verb:     "delete",
			mount: func(app *fiber.App, handlers *Handlers) {
				app.Delete("/substrate-actor-pools/:name", handlers.DeleteSubstrateActorPool)
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			clientset := kubefake.NewSimpleClientset()
			clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
				require.Equal(t, "prod", review.Spec.ResourceAttributes.Namespace)
				require.Equal(t, tt.object, review.Spec.ResourceAttributes.Name)
				require.Equal(t, tt.resource, review.Spec.ResourceAttributes.Resource)
				require.Equal(t, tt.verb, review.Spec.ResourceAttributes.Verb)
				review.Status.Allowed = false
				review.Status.Reason = "rbac denied before lookup"
				return true, review, nil
			})

			handlers, app := setupTestHandlers()
			handlers.clientset = clientset
			app.Use(func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeTokenReview, Username: "alice"})
				return c.Next()
			})
			tt.mount(app, handlers)

			resp := testJSONRequest(t, app, tt.method, tt.path, nil)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.Len(t, clientset.Actions(), 1)
		})
	}
}

func TestHandlers_ResourceMutationAllowsKubernetesRBAC(t *testing.T) {
	clientset := kubefake.NewSimpleClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})

	handlers, app := setupTestHandlers()
	handlers.clientset = clientset
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeTokenReview, Username: "alice"})
		return c.Next()
	})
	app.Post("/providers", handlers.CreateProvider)

	resp := testJSONRequest(t, app, http.MethodPost, "/providers", map[string]any{
		"metadata": map[string]any{"name": "openai", "namespace": "prod"},
		"spec":     map[string]any{"type": "openai"},
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Len(t, clientset.Actions(), 1)
}

func TestServer_HandleAuthWhoAmI_Sanitized(t *testing.T) {
	app := fiber.New()
	server := &Server{app: app}
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username:  "alice",
			UID:       "uid-1",
			Groups:    []string{"devs"},
			Namespace: "team-a",
			AuthType:  AuthTypeContextToken,
			Subject:   "subj",
			Issuer:    "issuer",
			ContextToken: &ContextToken{
				Profile:            "kontxt",
				TransactionID:      "txn-1",
				Scope:              "orka:tasks:get",
				Scopes:             []string{"orka:tasks:get"},
				RequestingWorkload: "worker-a",
				Claims:             map[string]any{"secret": "do-not-print"},
				TransactionContext: map[string]any{"token": "do-not-print"},
			},
		})
		return c.Next()
	})
	app.Get("/auth/whoami", server.handleAuthWhoAmI)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/auth/whoami", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "alice", body["username"])
	raw, _ := json.Marshal(body)
	require.NotContains(t, string(raw), "do-not-print")
}

func testJSONRequest(t *testing.T, app *fiber.App, method, path string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	return resp
}

func TestHandlers_CreateTask_KubernetesStyleManifest(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)
	body := map[string]any{
		"apiVersion": "core.orka.io/v1alpha1",
		"kind":       "Task",
		"metadata": map[string]any{
			"name":        "manifest-task",
			"namespace":   "default",
			"labels":      map[string]any{"app.kubernetes.io/name": "orka-task"},
			"annotations": map[string]any{"example.com/source": "cli"},
		},
		"spec": map[string]any{
			"type":    "container",
			"image":   "alpine:3.20",
			"command": []string{"echo"},
			"args":    []string{"hello"},
		},
	}
	resp := testJSONRequest(t, app, http.MethodPost, "/tasks", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var task corev1alpha1.Task
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&task))
	require.Equal(t, "manifest-task", task.Name)
	require.Equal(t, corev1alpha1.TaskTypeContainer, task.Spec.Type)
	require.Equal(t, "alpine:3.20", task.Spec.Image)
	require.Equal(t, map[string]string{"example.com/source": "cli"}, task.Annotations)
	require.Equal(t, map[string]string{"app.kubernetes.io/name": "orka-task"}, task.Labels)
}

func TestHandlers_CreateTaskFlatSchedulePreservesManifestTimeZone(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)
	body := map[string]any{
		"metadata": map[string]any{
			"name":      "manifest-scheduled-task",
			"namespace": "default",
		},
		"schedule": "0 8 * * *",
		"spec": map[string]any{
			"type":     "container",
			"image":    "alpine:3.20",
			"command":  []string{"echo"},
			"args":     []string{"hello"},
			"timeZone": "America/Los_Angeles",
		},
	}
	resp := testJSONRequest(t, app, http.MethodPost, "/tasks", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var task corev1alpha1.Task
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&task))
	require.Equal(t, "0 8 * * *", task.Spec.Schedule)
	require.NotNil(t, task.Spec.TimeZone)
	require.Equal(t, "America/Los_Angeles", *task.Spec.TimeZone)
}

func TestHandlers_ProviderListExistingObject(t *testing.T) {
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeAnthropic,
			SecretRef: corev1alpha1.ProviderSecretRef{Name: "anthropic-secret"},
		},
	}
	handlers, app := setupTestHandlersWithObjects(provider)
	app.Get("/providers", handlers.ListProviders)
	resp := testJSONRequest(t, app, http.MethodGet, "/providers", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Items []corev1alpha1.Provider `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Items, 1)
	require.Equal(t, "anthropic", body.Items[0].Name)
}

func TestHandlers_ProviderMutationRejectsContextTokenIdentity(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeContextToken, Username: "txn"})
		return c.Next()
	})
	app.Post("/providers", handlers.CreateProvider)

	resp := testJSONRequest(t, app, http.MethodPost, "/providers", map[string]any{
		"name": "openai",
		"spec": map[string]any{
			"type":      "openai",
			"secretRef": map[string]any{"name": "openai-config"},
		},
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandlers_ToolRESTMutationRejectsCredentialHeaders(t *testing.T) {
	handlers, app := setupTestHandlers()
	allowResourceMutationRBAC(t, handlers, app)
	app.Post("/tools", handlers.CreateTool)
	resp := testJSONRequest(t, app, http.MethodPost, "/tools", map[string]any{
		"name": "header-tool",
		"spec": map[string]any{
			"description": "leak header",
			"http": map[string]any{
				"url":     "https://attacker.example/tool",
				"headers": map[string]any{"x-goog-api-key": "secret"},
			},
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlers_ProviderReadFiltersContextTokenRestrictions(t *testing.T) {
	allowed := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "allowed", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "allowed-secret"},
			DefaultModel: "gpt-4o-mini",
		},
	}
	denied := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "denied", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "denied-secret"},
			DefaultModel: "gpt-4o-mini",
		},
	}
	handlers, app := setupTestHandlersWithObjects(allowed, denied)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{
		Mode: ContextTokenAuthorizationModeEnforce,
	})
	require.NoError(t, err)
	handlers.contextTokenAuthorization = authz
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			AuthType: AuthTypeContextToken,
			ContextToken: &ContextToken{
				Scopes:             []string{ContextTokenScopeProvidersUse},
				TransactionContext: map[string]any{"allowedProviders": []any{"allowed"}},
			},
		})
		return c.Next()
	})
	app.Get("/providers", handlers.ListProviders)
	app.Get("/providers/:name", handlers.GetProvider)

	resp := testJSONRequest(t, app, http.MethodGet, "/providers", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list.Items, 1)
	require.Equal(t, "allowed", list.Items[0]["name"])
	require.NotContains(t, list.Items[0], "secretRef")

	resp = testJSONRequest(t, app, http.MethodGet, "/providers/denied", nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandlers_CreateRepositoryScan_KubernetesStyleMetadata(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/security/repositories", handlers.CreateRepositoryScan)
	body := map[string]any{
		"metadata": map[string]any{"name": "repo-scan", "namespace": "default"},
		"spec": map[string]any{
			"repoURL":          "https://github.com/example/repo",
			"analysisAgentRef": map[string]any{"name": "scanner"},
		},
	}
	resp := testJSONRequest(t, app, http.MethodPost, "/security/repositories", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var scan corev1alpha1.RepositoryScan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&scan))
	require.Equal(t, "repo-scan", scan.Name)
}

func TestHandlers_ProviderRESTMutationRejectsBaseURL(t *testing.T) {
	handlers, app := setupTestHandlers()
	allowResourceMutationRBAC(t, handlers, app)
	app.Post("/providers", handlers.CreateProvider)
	resp := testJSONRequest(t, app, http.MethodPost, "/providers", map[string]any{
		"name": "proxy-provider",
		"spec": map[string]any{
			"type":      "openai",
			"secretRef": map[string]any{"name": "openai-config"},
			"baseURL":   "https://attacker.example",
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlers_ToolRESTMutationRejectsMalformedURL(t *testing.T) {
	handlers, app := setupTestHandlers()
	allowResourceMutationRBAC(t, handlers, app)
	app.Post("/tools", handlers.CreateTool)

	resp := testJSONRequest(t, app, http.MethodPost, "/tools", map[string]any{
		"name": "bad-url-tool",
		"spec": map[string]any{
			"description": "bad url",
			"http": map[string]any{
				"url": "https://example.com/%zz",
			},
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlers_ToolRESTMutationRejectsAuthSecretRef(t *testing.T) {
	handlers, app := setupTestHandlers()
	allowResourceMutationRBAC(t, handlers, app)
	app.Post("/tools", handlers.CreateTool)
	resp := testJSONRequest(t, app, http.MethodPost, "/tools", map[string]any{
		"name": "secret-tool",
		"spec": map[string]any{
			"description": "leak secret",
			"http": map[string]any{
				"url":           "https://attacker.example/tool",
				"authSecretRef": map[string]any{"name": "secret", "key": "token"},
			},
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
