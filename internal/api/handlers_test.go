/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/controller"
)

func setupTestHandlers() (*Handlers, *fiber.App) {
	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, "")

	app := fiber.New()
	return handlers, app
}

func setupTestHandlersWithObjects(objs ...runtime.Object) (*Handlers, *fiber.App) {
	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	handlers := NewHandlers(fakeClient, nil, "")

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
	corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, "allowed-ns")

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
				ConfigMapName: "test-result",
				Key:           "output",
			},
		},
	}

	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-result",
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
				ConfigMapName: "nonexistent-result",
				Key:           "output",
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

	handlers := NewHandlers(fakeClient, nil, "test-ns")
	if handlers == nil {
		t.Fatal("NewHandlers returned nil")
	}
	if handlers.watchNamespace != "test-ns" {
		t.Errorf("watchNamespace = %s, want test-ns", handlers.watchNamespace)
	}
}

func setupTestHandlersWithSessionManager(objs ...runtime.Object) (*Handlers, *fiber.App) {
	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	sm := controller.NewSessionManager(fakeClient)
	handlers := NewHandlers(fakeClient, sm, "")

	app := fiber.New()
	return handlers, app
}

// --- ListSessions tests ---

func TestHandlers_ListSessions_Success(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-my-session",
			Namespace: "default",
			Labels: map[string]string{
				"mercan.ai/session": "true",
			},
			Annotations: map[string]string{
				"mercan.ai/message-count": "5",
				"mercan.ai/input-tokens":  "100",
				"mercan.ai/output-tokens": "200",
				"mercan.ai/active-task":   "",
				"mercan.ai/created-at":    "2025-01-01T00:00:00Z",
				"mercan.ai/updated-at":    "2025-01-01T01:00:00Z",
			},
		},
		Data: map[string]string{
			"transcript.jsonl": `{"role":"user","content":"hello"}`,
		},
	}

	handlers, app := setupTestHandlersWithSessionManager(sessionCM)
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
	json.NewDecoder(resp.Body).Decode(&result)
	items, ok := result.Items.([]interface{})
	if !ok || len(items) != 1 {
		t.Errorf("Expected 1 session, got %v", result.Items)
	}
}

func TestHandlers_ListSessions_Empty(t *testing.T) {
	handlers, app := setupTestHandlersWithSessionManager()
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
	json.NewDecoder(resp.Body).Decode(&result)
	items, ok := result.Items.([]interface{})
	if !ok || len(items) != 0 {
		t.Errorf("Expected 0 sessions, got %v", result.Items)
	}
}

func TestHandlers_ListSessions_InvalidLimit(t *testing.T) {
	handlers, app := setupTestHandlersWithSessionManager()
	app.Get("/sessions", handlers.ListSessions)

	req := httptest.NewRequest(http.MethodGet, "/sessions?limit=invalid", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandlers_ListSessions_DefaultNamespace(t *testing.T) {
	handlers, app := setupTestHandlersWithSessionManager()
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
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	sm := controller.NewSessionManager(fakeClient)
	handlers := NewHandlers(fakeClient, sm, "watched-ns")

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
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-my-session",
			Namespace: "default",
			Labels: map[string]string{
				"mercan.ai/session": "true",
			},
			Annotations: map[string]string{
				"mercan.ai/message-count": "3",
				"mercan.ai/input-tokens":  "50",
				"mercan.ai/output-tokens": "100",
				"mercan.ai/active-task":   "",
				"mercan.ai/created-at":    "2025-01-01T00:00:00Z",
				"mercan.ai/updated-at":    "2025-01-01T01:00:00Z",
			},
		},
		Data: map[string]string{
			"transcript.jsonl": `{"role":"user","content":"hello"}`,
		},
	}

	handlers, app := setupTestHandlersWithSessionManager(sessionCM)
	app.Get("/sessions/:id", handlers.GetSession)

	req := httptest.NewRequest(http.MethodGet, "/sessions/my-session", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["name"] != "my-session" {
		t.Errorf("name = %v, want my-session", result["name"])
	}
	if result["messageCount"] != "3" {
		t.Errorf("messageCount = %v, want 3", result["messageCount"])
	}
}

func TestHandlers_GetSession_NotFound(t *testing.T) {
	handlers, app := setupTestHandlersWithSessionManager()
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
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-my-session",
			Namespace: "watched-ns",
			Labels: map[string]string{
				"mercan.ai/session": "true",
			},
			Annotations: map[string]string{},
		},
		Data: map[string]string{},
	}

	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(sessionCM).Build()
	sm := controller.NewSessionManager(fakeClient)
	handlers := NewHandlers(fakeClient, sm, "watched-ns")

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
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-my-session",
			Namespace: "default",
			Labels: map[string]string{
				"mercan.ai/session": "true",
			},
		},
	}

	handlers, app := setupTestHandlersWithSessionManager(sessionCM)
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
	handlers, app := setupTestHandlersWithSessionManager()
	app.Delete("/sessions/:id", handlers.DeleteSession)

	req := httptest.NewRequest(http.MethodDelete, "/sessions/nonexistent", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandlers_DeleteSession_WatchNamespace(t *testing.T) {
	sessionCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "session-my-session",
			Namespace: "watched-ns",
			Labels: map[string]string{
				"mercan.ai/session": "true",
			},
		},
	}

	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(sessionCM).Build()
	sm := controller.NewSessionManager(fakeClient)
	handlers := NewHandlers(fakeClient, sm, "watched-ns")

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
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	handlers := NewHandlers(fakeClient, nil, "watched-ns")

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
				ConfigMapName: "test-result",
				Key:           "missing-key",
			},
		},
	}

	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-result",
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
				ConfigMapName: "test-result",
				Key:           "", // empty key defaults to "result"
			},
		},
	}

	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-result",
			Namespace: "default",
		},
		Data: map[string]string{
			"result": "default key content",
		},
	}

	handlers, app := setupTestHandlersWithObjects(task, resultCM)
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
				ConfigMapName: "test-result",
				Key:           "output",
			},
		},
	}

	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-result",
			Namespace: "watched-ns",
		},
		Data: map[string]string{
			"output": "task result content",
		},
	}

	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task, resultCM).Build()
	handlers := NewHandlers(fakeClient, nil, "watched-ns")

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
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	handlers := NewHandlers(fakeClient, nil, "watched-ns")

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

	var lines []string
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

	var lines []string
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

	var lines []string
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

	var lines []string
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
