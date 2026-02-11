/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/controller"
	"github.com/sozercan/mercan/internal/store"
	"github.com/sozercan/mercan/internal/store/sqlite"
)

func setupTestHandlers() (*Handlers, *fiber.App) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(fakeClient, nil, "", ss, ss)

	app := fiber.New()
	return handlers, app
}

func setupTestHandlersWithObjects(objs ...runtime.Object) (*Handlers, *fiber.App) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(fakeClient, nil, "", ss, ss)

	app := fiber.New()
	return handlers, app
}

func TestHandlers_Healthz(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/healthz", handlers.Healthz)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_Readyz(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/readyz", handlers.Readyz)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_CreateTask_Valid(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Name:      "test-task",
		Namespace: "default",
		Type:      corev1alpha1.TaskTypeContainer,
		Image:     "busybox",
		Command:   []string{"echo", "hello"},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
}

func TestHandlers_CreateTask_MissingName(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Type: corev1alpha1.TaskTypeContainer,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandlers_CreateTask_MissingType(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Name: "test-task",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandlers_CreateTask_DefaultNamespace(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Name: "test-task",
		Type: corev1alpha1.TaskTypeContainer,
		// No namespace - should default to "default"
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
}

func TestHandlers_CreateTask_NamespaceScoped(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, "allowed-ns", nil, nil)

	app := fiber.New()
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Name:      "test-task",
		Namespace: "other-ns", // Different from watchNamespace
		Type:      corev1alpha1.TaskTypeContainer,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestHandlers_CreateTask_WithTimeout(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Name:    "test-task",
		Type:    corev1alpha1.TaskTypeContainer,
		Timeout: "5m",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
}

func TestHandlers_CreateTask_InvalidTimeout(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Name:    "test-task",
		Type:    corev1alpha1.TaskTypeContainer,
		Timeout: "invalid",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandlers_CreateTask_AlreadyExists(t *testing.T) {
	existingTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}

	handlers, app := setupTestHandlersWithObjects(existingTask)
	app.Post("/tasks", handlers.CreateTask)

	body := CreateTaskRequest{
		Name:      "existing-task",
		Namespace: "default",
		Type:      corev1alpha1.TaskTypeContainer,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusConflict)
	}
}

func TestHandlers_ListTasks(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/tasks", handlers.ListTasks)

	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_ListTasks_WithPagination(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks", handlers.ListTasks)

	req := httptest.NewRequest(http.MethodGet, "/tasks?limit=10", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_ListTasks_WithNamespaceFilter(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks", handlers.ListTasks)

	req := httptest.NewRequest(http.MethodGet, "/tasks?namespace=custom-ns", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetTask_Found(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/tasks/:id", handlers.GetTask)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetTask_NotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks/:id", handlers.GetTask)

	req := httptest.NewRequest(http.MethodGet, "/tasks/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_DeleteTask_Success(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	app.Delete("/tasks/:id", handlers.DeleteTask)

	req := httptest.NewRequest(http.MethodDelete, "/tasks/test-task", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestHandlers_DeleteTask_NotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Delete("/tasks/:id", handlers.DeleteTask)

	req := httptest.NewRequest(http.MethodDelete, "/tasks/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_GetTaskLogs_NoJob(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			JobName: "", // No job
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/tasks/:id/logs", handlers.GetTaskLogs)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/logs", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_GetTaskLogs_WithJob(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			JobName: "test-job",
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/tasks/:id/logs", handlers.GetTaskLogs)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/logs", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	// Returns OK with placeholder message
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetTaskResult_Success(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	// Save result to store
	require.NoError(t, handlers.resultStore.SaveResult(context.Background(), "default", "test-task", []byte("task result content")))
	app.Get("/tasks/:id/result", handlers.GetTaskResult)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/result", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetTaskResult_NoResult(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: nil, // No result
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/tasks/:id/result", handlers.GetTaskResult)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/result", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_GetTaskResult_ResultNotFound(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/tasks/:id/result", handlers.GetTaskResult)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/result", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_ListTools(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tools", handlers.ListTools)

	req := httptest.NewRequest(http.MethodGet, "/tools", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetTool_Builtin(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tools/:name", handlers.GetTool)

	builtinTools := []string{"web_search", "code_exec", "file_read"}
	for _, toolName := range builtinTools {
		t.Run(toolName, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/tools/"+toolName, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}

			if resp.StatusCode != http.StatusOK {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
			}
		})
	}
}

func TestHandlers_GetTool_NotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tools/:name", handlers.GetTool)

	req := httptest.NewRequest(http.MethodGet, "/tools/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_ListAgents(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/agents", handlers.ListAgents)

	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetAgent_Found(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
		},
	}

	handlers, app := setupTestHandlersWithObjects(agent)
	app.Get("/agents/:name", handlers.GetAgent)

	req := httptest.NewRequest(http.MethodGet, "/agents/test-agent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetAgent_NotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/agents/:name", handlers.GetAgent)

	req := httptest.NewRequest(http.MethodGet, "/agents/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"seconds", "300s", false},
		{"minutes", "5m", false},
		{"hours", "1h", false},
		{"combined", "1h30m", false},
		{"invalid", "invalid", true},
		{"empty", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseDuration() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result == nil {
				t.Error("parseDuration() returned nil for valid input")
			}
		})
	}
}

func TestNewHandlers(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	handlers := NewHandlers(fakeClient, nil, "test-ns", nil, nil)
	if handlers == nil {
		t.Fatal("NewHandlers returned nil")
	}
	if handlers.watchNamespace != "test-ns" {
		t.Errorf("watchNamespace = %s, want test-ns", handlers.watchNamespace)
	}
}

func setupTestHandlersWithSessionManager() (*Handlers, *fiber.App, *sqlite.Store) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	sm := controller.NewSessionManager(ss)
	handlers := NewHandlers(fakeClient, sm, "", ss, ss)

	app := fiber.New()
	return handlers, app, ss
}

// --- ListSessions tests ---

func TestHandlers_ListSessions_Success(t *testing.T) {
	handlers, app, ss := setupTestHandlersWithSessionManager()
	ctx := context.Background()
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:    "default",
		Name:         "my-session",
		SessionType:  "task",
		MessageCount: 5,
		InputTokens:  100,
		OutputTokens: 200,
	})

	app.Get("/sessions", handlers.ListSessions)

	req := httptest.NewRequest(http.MethodGet, "/sessions?namespace=default", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var result ListResponse
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	items, ok := result.Items.([]any)
	if !ok || len(items) != 1 {
		t.Errorf("Expected 1 session, got %v", result.Items)
	}
}

func TestHandlers_ListSessions_Empty(t *testing.T) {
	handlers, app, _ := setupTestHandlersWithSessionManager()
	app.Get("/sessions", handlers.ListSessions)

	req := httptest.NewRequest(http.MethodGet, "/sessions?namespace=default", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var result ListResponse
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	items, ok := result.Items.([]any)
	if !ok || len(items) != 0 {
		t.Errorf("Expected 0 sessions, got %v", result.Items)
	}
}

func TestHandlers_ListSessions_DefaultNamespace(t *testing.T) {
	handlers, app, _ := setupTestHandlersWithSessionManager()
	app.Get("/sessions", handlers.ListSessions)

	// No namespace query param - should default to "default"
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_ListSessions_WatchNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	sm := controller.NewSessionManager(ss)
	handlers := NewHandlers(fakeClient, sm, "watched-ns", ss, ss)

	app := fiber.New()
	app.Get("/sessions", handlers.ListSessions)

	// No namespace provided - should use watchNamespace
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// --- GetSession tests ---

func TestHandlers_GetSession_Success(t *testing.T) {
	handlers, app, ss := setupTestHandlersWithSessionManager()
	ctx := context.Background()
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:    "default",
		Name:         "my-session",
		SessionType:  "task",
		MessageCount: 3,
		InputTokens:  50,
		OutputTokens: 100,
	})

	app.Get("/sessions/:id", handlers.GetSession)

	req := httptest.NewRequest(http.MethodGet, "/sessions/my-session", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	if result["name"] != "my-session" {
		t.Errorf("name = %v, want my-session", result["name"])
	}
	if result["messageCount"] != float64(3) {
		t.Errorf("messageCount = %v, want 3", result["messageCount"])
	}
}

func TestHandlers_GetSession_NotFound(t *testing.T) {
	handlers, app, _ := setupTestHandlersWithSessionManager()
	app.Get("/sessions/:id", handlers.GetSession)

	req := httptest.NewRequest(http.MethodGet, "/sessions/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_GetSession_WatchNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	sm := controller.NewSessionManager(ss)
	handlers := NewHandlers(fakeClient, sm, "watched-ns", ss, ss)

	ctx := context.Background()
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "watched-ns",
		Name:        "my-session",
		SessionType: "task",
	})

	app := fiber.New()
	app.Get("/sessions/:id", handlers.GetSession)

	// namespace query is overridden by watchNamespace
	req := httptest.NewRequest(http.MethodGet, "/sessions/my-session?namespace=other", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// --- DeleteSession tests ---

func TestHandlers_DeleteSession_Success(t *testing.T) {
	handlers, app, ss := setupTestHandlersWithSessionManager()
	ctx := context.Background()
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "default",
		Name:        "my-session",
		SessionType: "task",
	})

	app.Delete("/sessions/:id", handlers.DeleteSession)

	req := httptest.NewRequest(http.MethodDelete, "/sessions/my-session", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestHandlers_DeleteSession_NotFound(t *testing.T) {
	handlers, app, _ := setupTestHandlersWithSessionManager()
	app.Delete("/sessions/:id", handlers.DeleteSession)

	req := httptest.NewRequest(http.MethodDelete, "/sessions/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	// SQLite DELETE is a no-op when not found, not an error
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestHandlers_DeleteSession_WatchNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	sm := controller.NewSessionManager(ss)
	handlers := NewHandlers(fakeClient, sm, "watched-ns", ss, ss)

	ctx := context.Background()
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "watched-ns",
		Name:        "my-session",
		SessionType: "task",
	})

	app := fiber.New()
	app.Delete("/sessions/:id", handlers.DeleteSession)

	req := httptest.NewRequest(http.MethodDelete, "/sessions/my-session", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

// --- GetTaskLogs additional tests ---

func TestHandlers_GetTaskLogs_NotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks/:id/logs", handlers.GetTaskLogs)

	req := httptest.NewRequest(http.MethodGet, "/tasks/nonexistent/logs", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_GetTaskLogs_WatchNamespace(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "watched-ns",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			JobName: "test-job",
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	handlers := NewHandlers(fakeClient, nil, "watched-ns", nil, nil)

	app := fiber.New()
	app.Get("/tasks/:id/logs", handlers.GetTaskLogs)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/logs", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// --- GetTaskResult additional tests ---

func TestHandlers_GetTaskResult_TaskNotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks/:id/result", handlers.GetTaskResult)

	req := httptest.NewRequest(http.MethodGet, "/tasks/nonexistent/result", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_GetTaskResult_MissingKey(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task-result",
			Namespace: "default",
		},
		Data: map[string]string{
			"output": "task result content",
		},
	}

	handlers, app := setupTestHandlersWithObjects(task, resultCM)
	app.Get("/tasks/:id/result", handlers.GetTaskResult)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/result", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_GetTaskResult_DefaultKey(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	handlers, app := setupTestHandlersWithObjects(task)
	handlers.resultStore.SaveResult(context.Background(), "default", "test-task", []byte("default key content")) //nolint:errcheck
	app.Get("/tasks/:id/result", handlers.GetTaskResult)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/result", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlers_GetTaskResult_WatchNamespace(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "watched-ns",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	ss.SaveResult(context.Background(), "watched-ns", "test-task", []byte("task result content")) //nolint:errcheck
	handlers := NewHandlers(fakeClient, nil, "watched-ns", ss, ss)

	app := fiber.New()
	app.Get("/tasks/:id/result", handlers.GetTaskResult)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-task/result", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// --- DeleteTask additional tests ---

func TestHandlers_DeleteTask_WatchNamespace(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "watched-ns",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	handlers := NewHandlers(fakeClient, nil, "watched-ns", nil, nil)

	app := fiber.New()
	app.Delete("/tasks/:id", handlers.DeleteTask)

	req := httptest.NewRequest(http.MethodDelete, "/tasks/test-task", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

// --- readLines tests ---

func TestReadLines(t *testing.T) {
	input := "line1\nline2\nline3\n"
	r := strings.NewReader(input)
	ch := readLines(r)

	lines := make([]string, 0, 3)
	for line := range ch {
		lines = append(lines, line)
	}

	if len(lines) != 3 {
		t.Fatalf("Expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Errorf("Unexpected lines: %v", lines)
	}
}

func TestReadLines_Empty(t *testing.T) {
	r := strings.NewReader("")
	ch := readLines(r)

	lines := make([]string, 0)
	for line := range ch {
		lines = append(lines, line)
	}

	if len(lines) != 0 {
		t.Errorf("Expected 0 lines, got %d", len(lines))
	}
}

func TestReadLines_SingleLine(t *testing.T) {
	r := strings.NewReader("single line")
	ch := readLines(r)

	lines := make([]string, 0, 1)
	for line := range ch {
		lines = append(lines, line)
	}

	if len(lines) != 1 {
		t.Fatalf("Expected 1 line, got %d", len(lines))
	}
	if lines[0] != "single line" {
		t.Errorf("line = %q, want %q", lines[0], "single line")
	}
}

func TestReadLines_MultipleLines_NoTrailingNewline(t *testing.T) {
	r := strings.NewReader("line1\nline2")
	ch := readLines(r)

	lines := make([]string, 0, 2)
	for line := range ch {
		lines = append(lines, line)
	}

	if len(lines) != 2 {
		t.Fatalf("Expected 2 lines, got %d", len(lines))
	}
	if lines[0] != "line1" || lines[1] != "line2" {
		t.Errorf("Unexpected lines: %v", lines)
	}
}

func TestGetTaskChildren(t *testing.T) {
	parentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parent-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	childTask1 := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-1",
			Namespace: "default",
			Labels:    map[string]string{"mercan.ai/parent-task": "parent-task"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	childTask2 := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-2",
			Namespace: "default",
			Labels:    map[string]string{"mercan.ai/parent-task": "parent-task"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	unrelatedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}

	handlers, app := setupTestHandlersWithObjects(parentTask, childTask1, childTask2, unrelatedTask)
	app.Get("/tasks/:id/children", handlers.GetTaskChildren)

	req := httptest.NewRequest(http.MethodGet, "/tasks/parent-task/children", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var result ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	items, ok := result.Items.([]any)
	if !ok {
		t.Fatalf("Items is not a slice")
	}

	if len(items) != 2 {
		t.Errorf("Expected 2 children, got %d", len(items))
	}
}

func TestGetTaskChildren_Empty(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks/:id/children", handlers.GetTaskChildren)

	req := httptest.NewRequest(http.MethodGet, "/tasks/no-parent/children", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var result ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	items, ok := result.Items.([]any)
	if !ok {
		t.Fatalf("Items is not a slice")
	}

	if len(items) != 0 {
		t.Errorf("Expected 0 children, got %d", len(items))
	}
}
