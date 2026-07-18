/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
)

func TestTaskAccessLoadReadableMapsTaskNotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/read/:id", func(c fiber.Ctx) error {
		_, err := handlers.taskAccess().loadReadable(c, "testRead", "default", c.Params("id"))
		if err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/read/missing", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTaskAccessLoadReadableReturnsLoadedTask(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task-a", Namespace: "default"}}
	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/read/:id", func(c fiber.Ctx) error {
		loaded, err := handlers.taskAccess().loadReadable(c, "testRead", "default", c.Params("id"))
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"name": loaded.Name, "namespace": loaded.Namespace})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/read/task-a", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestTaskAccessContextTokenOnlyLoadSkipsNonContextTokenCallers(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/maybe/:id", func(c fiber.Ctx) error {
		loaded, err := handlers.taskAccess().loadReadableForContextToken(c, "testRead", "default", c.Params("id"))
		if err != nil {
			return err
		}
		if loaded != nil {
			return c.SendStatus(fiber.StatusConflict)
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/maybe/missing", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestTaskAccessPreservesTraceStoreMissingBeforeTaskLoad(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks/:id/trace", handlers.GetTaskTrace)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/tasks/missing/trace", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 before task load", resp.StatusCode)
	}
}

func TestTaskAccessPreservesForkStoreMissingBeforeBodyAndTaskLoad(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks/:id/fork", handlers.ForkTask)

	req := httptest.NewRequest(http.MethodPost, "/tasks/missing/fork", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 before body parse/task load", resp.StatusCode)
	}
}

func contextScopedTestUser(scopes ...string) *UserInfo {
	ctx := &ContextToken{Scopes: scopes}
	return &UserInfo{AuthType: AuthTypeContextToken, ContextToken: ctx}
}

func TestTaskAccessGatewayTaskRequiresGatewayReadScope(t *testing.T) {
	for _, tt := range []struct {
		name       string
		scopes     []string
		wantStatus int
	}{
		{name: "task read only", scopes: []string{ContextTokenScopeTaskGet}, wantStatus: http.StatusForbidden},
		{name: "task and gateway read", scopes: []string{ContextTokenScopeTaskGet, ContextScopeGatewaysRead}, wantStatus: http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			task := gatewayTaskAccessFixture()
			objects := append(gatewayTaskIdentityObjects("gateway-uid"), task)
			handlers, app := setupTestHandlersWithObjects(objects...)
			cfg, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
			if err != nil {
				t.Fatal(err)
			}
			handlers.contextTokenAuthorization = cfg
			app.Use(func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, contextScopedTestUser(tt.scopes...))
				return c.Next()
			})
			app.Get("/read/:id", func(c fiber.Ctx) error {
				if _, err := handlers.taskAccess().loadReadable(c, "testRead", "default", c.Params("id")); err != nil {
					return err
				}
				return c.SendStatus(fiber.StatusNoContent)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/read/gateway-task", nil))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestListTasksFiltersGatewayTasksWithoutGatewayReadScope(t *testing.T) {
	normal := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ordinary-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Image: "busybox"},
	}
	gatewayTask := gatewayTaskAccessFixture()
	objects := append(gatewayTaskIdentityObjects("gateway-uid"), normal, gatewayTask)
	handlers, app := setupTestHandlersWithObjects(objects...)
	cfg, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}
	handlers.contextTokenAuthorization = cfg
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, contextScopedTestUser(ContextTokenScopeTaskList))
		return c.Next()
	})
	app.Get("/tasks", handlers.ListTasks)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/tasks?namespace=default&limit=0", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var response struct {
		Items []corev1alpha1.Task `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].Name != normal.Name {
		t.Fatalf("listed Tasks = %#v, want only ordinary Task", response.Items)
	}
}

func TestDeleteGatewayTaskRequiresGatewayOperateScope(t *testing.T) {
	task := gatewayTaskAccessFixture()
	objects := append(gatewayTaskIdentityObjects("gateway-uid"), task)
	handlers, app := setupTestHandlersWithObjects(objects...)
	cfg, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}
	handlers.contextTokenAuthorization = cfg
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, contextScopedTestUser(
			ContextTokenScopeTaskDelete, ContextScopeGatewaysRead,
		))
		return c.Next()
	})
	app.Delete("/tasks/:id", handlers.DeleteTask)

	resp, err := app.Test(httptest.NewRequest(http.MethodDelete, "/tasks/gateway-task?namespace=default", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func gatewayTaskAccessFixture() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gateway-task", Namespace: "default",
			Labels: map[string]string{gatewayruntime.TaskGatewayEventLabel: "gev-test"},
			Annotations: map[string]string{
				gatewayruntime.TaskGatewayEventAnnotation: "gev-test",
				gatewayruntime.TaskGatewayNameAnnotation:  "chat",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer, Image: "busybox",
			RequestedBy: &corev1alpha1.RequestedBy{
				Issuer: "gateway.orka.ai/default/namespace-uid/chat/gateway-uid",
			},
		},
	}
}

func TestTaskAccessRejectsRecreatedGatewayIdentity(t *testing.T) {
	task := gatewayTaskAccessFixture()
	objects := append(gatewayTaskIdentityObjects("replacement-gateway-uid"), task)
	handlers, app := setupTestHandlersWithObjects(objects...)
	cfg, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}
	handlers.contextTokenAuthorization = cfg
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, contextScopedTestUser(ContextTokenScopeTaskGet, ContextScopeGatewaysRead))
		return c.Next()
	})
	app.Get("/read/:id", func(c fiber.Ctx) error {
		_, err := handlers.taskAccess().loadReadable(c, "testRead", "default", c.Params("id"))
		return err
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/read/gateway-task", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func gatewayTaskIdentityObjects(gatewayUID string) []runtime.Object {
	return []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("namespace-uid")}},
		&gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{
			Name: "chat", Namespace: "default", UID: types.UID(gatewayUID),
		}},
	}
}

func TestGetTaskChildrenRequiresGatewayRBACForTokenReviewCaller(t *testing.T) {
	parent := gatewayTaskAccessFixture()
	objects := append(gatewayTaskIdentityObjects("gateway-uid"), parent)
	handlers, app := setupTestHandlersWithObjects(objects...)
	handlers.clientset = denyingSubjectAccessReviewClient(t, nil, nil)
	app.Use(tokenReviewUserMiddleware(limitedTokenReviewUser("default")))
	app.Get("/tasks/:id/children", handlers.GetTaskChildren)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/tasks/gateway-task/children?namespace=default", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestTaskAccessAllowsCrossNamespaceGatewayChild(t *testing.T) {
	child := gatewayTaskAccessFixture().DeepCopy()
	child.Name = "gateway-child"
	child.Namespace = "child-ns"
	objects := append(gatewayTaskIdentityObjects("gateway-uid"), child)
	handlers, app := setupTestHandlersWithObjects(objects...)
	cfg, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}
	handlers.contextTokenAuthorization = cfg
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, contextScopedTestUser(ContextTokenScopeTaskGet, ContextScopeGatewaysRead))
		return c.Next()
	})
	app.Get("/read/:id", func(c fiber.Ctx) error {
		_, err := handlers.taskAccess().loadReadable(c, "testRead", "child-ns", c.Params("id"))
		if err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/read/gateway-child", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}
