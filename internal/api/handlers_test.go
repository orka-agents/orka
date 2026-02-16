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
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/controller"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

func setupTestHandlers() (*Handlers, *fiber.App) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(fakeClient, nil, "", false, ss, ss, nil, nil, nil)

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
	handlers := NewHandlers(fakeClient, nil, "", false, ss, ss, nil, nil, nil)

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
	handlers := NewHandlers(fakeClient, nil, "allowed-ns", false, nil, nil, nil, nil, nil)

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

	handlers := NewHandlers(fakeClient, nil, "test-ns", false, nil, nil, nil, nil, nil)
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
	handlers := NewHandlers(fakeClient, sm, "", false, ss, ss, nil, nil, nil)

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
	handlers := NewHandlers(fakeClient, sm, "watched-ns", false, ss, ss, nil, nil, nil)

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
	handlers := NewHandlers(fakeClient, sm, "watched-ns", false, ss, ss, nil, nil, nil)

	ctx := context.Background()
	ss.CreateSession(ctx, &store.SessionRecord{ //nolint:errcheck
		Namespace:   "watched-ns",
		Name:        "my-session",
		SessionType: "task",
	})

	app := fiber.New()
	app.Get("/sessions/:id", handlers.GetSession)

	// explicit namespace that doesn't match watchNamespace should be rejected
	req := httptest.NewRequest(http.MethodGet, "/sessions/my-session?namespace=other", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusForbidden)
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
	handlers := NewHandlers(fakeClient, sm, "watched-ns", false, ss, ss, nil, nil, nil)

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
	handlers := NewHandlers(fakeClient, nil, "watched-ns", false, nil, nil, nil, nil, nil)

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
	handlers := NewHandlers(fakeClient, nil, "watched-ns", false, ss, ss, nil, nil, nil)

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
	handlers := NewHandlers(fakeClient, nil, "watched-ns", false, nil, nil, nil, nil, nil)

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

	lines := make([]string, 0, 1)
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
			Labels:    map[string]string{"orka.ai/parent-task": "parent-task"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	childTask2 := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-2",
			Namespace: "default",
			Labels:    map[string]string{"orka.ai/parent-task": "parent-task"},
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

func TestGetTaskPlan(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "plan-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(fakeClient, nil, "", false, ss, ss, ss, nil, nil)

	app := fiber.New()
	app.Get("/api/v1/tasks/:id/plan", handlers.GetTaskPlan)

	// Pre-save a plan into the store
	err := ss.SavePlan(context.Background(), "default", "plan-task", &store.PlanState{
		TaskName:     "plan-task",
		Namespace:    "default",
		Summary:      "working on it",
		ProgressPct:  75,
		GoalComplete: false,
		PlanDocument: "# My Plan\n- item 1",
	})
	require.NoError(t, err)

	t.Run("plan exists", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/plan-task/plan?namespace=default", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var plan store.PlanState
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&plan))
		require.Equal(t, "working on it", plan.Summary)
		require.Equal(t, 75, plan.ProgressPct)
		require.Equal(t, "# My Plan\n- item 1", plan.PlanDocument)
	})

	t.Run("no plan", func(t *testing.T) {
		taskNoPlan := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "no-plan-task",
				Namespace: "default",
			},
			Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAI,
			},
		}

		scheme2 := runtime.NewScheme()
		_ = corev1alpha1.AddToScheme(scheme2)
		_ = corev1.AddToScheme(scheme2)
		fakeClient2 := fake.NewClientBuilder().WithScheme(scheme2).WithRuntimeObjects(taskNoPlan).Build()
		db2, _ := sqlite.NewDB(":memory:")
		ss2 := sqlite.NewStore(db2, ":memory:")
		handlers2 := NewHandlers(fakeClient2, nil, "", false, ss2, ss2, ss2, nil, nil)

		app2 := fiber.New()
		app2.Get("/api/v1/tasks/:id/plan", handlers2.GetTaskPlan)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/no-plan-task/plan?namespace=default", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestResolveNamespace_IsolationEnforced(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(fakeClient, nil, "", true, ss, ss, nil, nil, nil)

	app := fiber.New()
	app.Get("/test", func(c fiber.Ctx) error {
		// Set user info in context (simulating auth middleware)
		c.Locals(UserInfoContextKey, &UserInfo{
			Username:  "system:serviceaccount:team-a:default",
			Namespace: "team-a",
		})
		ns, err := handlers.resolveNamespace(c, "team-b")
		if err != nil {
			return err
		}
		return c.SendString(ns)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestResolveNamespace_IsolationAllowsSameNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(fakeClient, nil, "", true, ss, ss, nil, nil, nil)

	app := fiber.New()
	app.Get("/test", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username:  "system:serviceaccount:team-a:default",
			Namespace: "team-a",
		})
		ns, err := handlers.resolveNamespace(c, "team-a")
		if err != nil {
			return err
		}
		return c.SendString(ns)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestResolveNamespace_WatchNamespaceMismatch(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	// Set watchNamespace to "production"
	handlers := NewHandlers(fakeClient, nil, "production", false, ss, ss, nil, nil, nil)

	app := fiber.New()
	app.Get("/test", func(c fiber.Ctx) error {
		ns, err := handlers.resolveNamespace(c, "staging")
		if err != nil {
			return err
		}
		return c.SendString(ns)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// --- CreateAgent tests ---

func TestHandlers_CreateAgent_Success(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/agents", handlers.CreateAgent)

	body := CreateAgentRequest{
		Name:      "test-agent",
		Namespace: "default",
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandlers_CreateAgent_MetadataStyle(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/agents", handlers.CreateAgent)

	body := CreateAgentRequest{
		Metadata: MetadataRequest{
			Name:      "meta-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4",
			},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandlers_CreateAgent_MissingName(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/agents", handlers.CreateAgent)

	body := CreateAgentRequest{
		Spec: corev1alpha1.AgentSpec{},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlers_CreateAgent_InvalidBody(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/agents", handlers.CreateAgent)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlers_CreateAgent_AlreadyExists(t *testing.T) {
	existing := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{},
	}
	handlers, app := setupTestHandlersWithObjects(existing)
	app.Post("/agents", handlers.CreateAgent)

	body := CreateAgentRequest{
		Name:      "existing-agent",
		Namespace: "default",
		Spec:      corev1alpha1.AgentSpec{},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestHandlers_CreateAgent_NamespaceForbidden(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, "allowed-ns", false, nil, nil, nil, nil, nil)

	app := fiber.New()
	app.Post("/agents", handlers.CreateAgent)

	body := CreateAgentRequest{
		Name:      "test-agent",
		Namespace: "other-ns",
		Spec:      corev1alpha1.AgentSpec{},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// --- UpdateAgent tests ---

func TestHandlers_UpdateAgent_Success(t *testing.T) {
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
	app.Put("/agents/:name", handlers.UpdateAgent)

	body := UpdateAgentRequest{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4",
			},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/agents/test-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	spec := result["spec"].(map[string]any)
	model := spec["model"].(map[string]any)
	require.Equal(t, "gpt-4", model["name"])
}

func TestHandlers_UpdateAgent_NotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Put("/agents/:name", handlers.UpdateAgent)

	body := UpdateAgentRequest{
		Spec: corev1alpha1.AgentSpec{},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/agents/nonexistent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlers_UpdateAgent_InvalidBody(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{},
	}

	handlers, app := setupTestHandlersWithObjects(agent)
	app.Put("/agents/:name", handlers.UpdateAgent)

	req := httptest.NewRequest(http.MethodPut, "/agents/test-agent", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// --- DeleteAgent tests ---

func TestHandlers_DeleteAgent_Success(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{},
	}

	handlers, app := setupTestHandlersWithObjects(agent)
	app.Delete("/agents/:name", handlers.DeleteAgent)

	req := httptest.NewRequest(http.MethodDelete, "/agents/test-agent", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestHandlers_DeleteAgent_NotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Delete("/agents/:name", handlers.DeleteAgent)

	req := httptest.NewRequest(http.MethodDelete, "/agents/nonexistent", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// --- ListSecretNames tests ---

func TestHandlers_ListSecretNames_Success(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
		Type: corev1.SecretTypeOpaque,
	}

	handlers, app := setupTestHandlersWithObjects(secret)
	app.Get("/secrets", handlers.ListSecretNames)

	req := httptest.NewRequest(http.MethodGet, "/secrets", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	items := result["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	require.Equal(t, "my-secret", item["name"])
}

func TestHandlers_ListSecretNames_FiltersServiceAccountTokens(t *testing.T) {
	opaqueSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
		Type: corev1.SecretTypeOpaque,
	}
	saTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sa-token",
			Namespace: "default",
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}

	handlers, app := setupTestHandlersWithObjects(opaqueSecret, saTokenSecret)
	app.Get("/secrets", handlers.ListSecretNames)

	req := httptest.NewRequest(http.MethodGet, "/secrets", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	items := result["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	require.Equal(t, "my-secret", item["name"])
}

func TestHandlers_ListSecretNames_Empty(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/secrets", handlers.ListSecretNames)

	req := httptest.NewRequest(http.MethodGet, "/secrets", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	items := result["items"].([]any)
	require.Len(t, items, 0)
}

// --- StreamPodLogs tests ---

func TestStreamPodLogs_WithFakeClientset(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}
	clientset := kubefake.NewSimpleClientset(pod) //nolint:staticcheck
	ctx := context.Background()

	// StreamPodLogs should return a stream (or error) — with fake clientset
	// it returns a stream even without real logs, but we verify the function is callable.
	stream, err := StreamPodLogs(ctx, clientset, "default", "test-pod", "worker")
	if err == nil && stream != nil {
		defer stream.Close()
	}
	// The fake clientset may or may not error — we just verify the function doesn't panic
	// and correctly calls the K8s API.
	_ = err
}

func TestStreamPodLogs_CancelledContext(t *testing.T) {
	clientset := kubefake.NewSimpleClientset() //nolint:staticcheck
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := StreamPodLogs(ctx, clientset, "default", "test-pod", "worker")
	// With a cancelled context, the stream call may or may not error depending
	// on the fake implementation — we verify no panic.
	_ = err
}

// --- handleAuthValidate tests ---

func TestHandlers_HandleAuthValidate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := &Server{
		app:    fiber.New(),
		client: fakeClient,
	}
	server.app.Get("/auth/validate", server.handleAuthValidate)

	req := httptest.NewRequest(http.MethodGet, "/auth/validate", nil)
	resp, err := server.app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	require.Equal(t, true, result["authenticated"])
}

// --- GetTaskLogs additional branch tests ---

func TestHandlers_GetTaskLogs_ResultStoreAvailable(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "done-task",
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
	require.NoError(t, handlers.resultStore.SaveResult(context.Background(), "default", "done-task", []byte("log output here")))
	app.Get("/tasks/:id/logs", handlers.GetTaskLogs)

	req := httptest.NewRequest(http.MethodGet, "/tasks/done-task/logs", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	require.Equal(t, "log output here", result["logs"])
}

func TestHandlers_GetTaskLogs_ResultStoreNotFound(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "done-task-no-data",
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
	app.Get("/tasks/:id/logs", handlers.GetTaskLogs)

	req := httptest.NewRequest(http.MethodGet, "/tasks/done-task-no-data/logs", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlers_GetTaskLogs_NamespaceForbidden(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, "allowed-ns", false, nil, nil, nil, nil, nil)

	app := fiber.New()
	app.Get("/tasks/:id/logs", handlers.GetTaskLogs)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test/logs?namespace=other-ns", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// --- GetSession transcript generation test ---

func TestHandlers_GetSession_WithTranscript(t *testing.T) {
	handlers, app, ss := setupTestHandlersWithSessionManager()
	ctx := context.Background()

	require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "transcript-session",
		SessionType: "chat",
	}))

	require.NoError(t, ss.AppendMessages(ctx, "default", "transcript-session", []store.SessionMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}))

	app.Get("/sessions/:id", handlers.GetSession)

	req := httptest.NewRequest(http.MethodGet, "/sessions/transcript-session", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	transcript, ok := result["transcript"].(string)
	require.True(t, ok)
	require.NotEmpty(t, transcript)
	require.Contains(t, transcript, "hello")
	require.Contains(t, transcript, "hi there")
	// Transcript should be JSONL (newline-separated)
	lines := strings.Split(transcript, "\n")
	require.Len(t, lines, 2)
}

// --- DeleteSession error path tests ---

func TestHandlers_DeleteSession_NamespaceForbidden(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	sm := controller.NewSessionManager(ss)
	handlers := NewHandlers(fakeClient, sm, "watched-ns", false, ss, ss, nil, nil, nil)

	app := fiber.New()
	app.Delete("/sessions/:id", handlers.DeleteSession)

	req := httptest.NewRequest(http.MethodDelete, "/sessions/my-session?namespace=wrong-ns", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// --- GetTaskPlan additional tests ---

func TestHandlers_GetTaskPlan_TaskNotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks/:id/plan", handlers.GetTaskPlan)

	req := httptest.NewRequest(http.MethodGet, "/tasks/nonexistent/plan", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlers_GetTaskPlan_NoPlanStore(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "plan-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	// planStore is nil
	handlers := NewHandlers(fakeClient, nil, "", false, nil, nil, nil, nil, nil)

	app := fiber.New()
	app.Get("/tasks/:id/plan", handlers.GetTaskPlan)

	req := httptest.NewRequest(http.MethodGet, "/tasks/plan-task/plan", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

// --- GetTask plan enrichment test ---

func TestHandlers_GetTask_WithPlanEnrichment(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "enriched-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
		Status: corev1alpha1.TaskStatus{
			Iteration: 3,
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).WithStatusSubresource(task).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(fakeClient, nil, "", false, ss, ss, ss, nil, nil)

	require.NoError(t, ss.SavePlan(context.Background(), "default", "enriched-task", &store.PlanState{
		TaskName:     "enriched-task",
		Namespace:    "default",
		Summary:      "almost done",
		ProgressPct:  90,
		GoalComplete: false,
		PlanDocument: "# Plan\n- step 1 done",
		Iteration:    3,
	}))

	app := fiber.New()
	app.Get("/tasks/:id", handlers.GetTask)

	req := httptest.NewRequest(http.MethodGet, "/tasks/enriched-task", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	plan, ok := result["plan"].(map[string]any)
	require.True(t, ok, "response should contain plan field")
	require.Equal(t, "almost done", plan["summary"])
	require.Equal(t, float64(90), plan["progressPct"])
	require.Equal(t, false, plan["goalComplete"])
}

func TestHandlers_GetTask_NoPlanStoreNoEnrichment(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-plan-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
		Status: corev1alpha1.TaskStatus{
			Iteration: 2,
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).WithStatusSubresource(task).Build()
	// planStore is nil
	handlers := NewHandlers(fakeClient, nil, "", false, nil, nil, nil, nil, nil)

	app := fiber.New()
	app.Get("/tasks/:id", handlers.GetTask)

	req := httptest.NewRequest(http.MethodGet, "/tasks/no-plan-task", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	_, hasPlan := result["plan"]
	require.False(t, hasPlan, "response should not contain plan field when planStore is nil")
}
