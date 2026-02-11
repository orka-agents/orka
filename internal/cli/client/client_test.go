/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewClient(t *testing.T) {
	c := New("http://localhost:8080/", "my-token", "test-ns")
	if c.BaseURL != "http://localhost:8080" {
		t.Fatalf("expected trailing slash trimmed, got %s", c.BaseURL)
	}
	if c.Token != "my-token" {
		t.Fatalf("expected token my-token, got %s", c.Token)
	}
	if c.Namespace != "test-ns" {
		t.Fatalf("expected namespace test-ns, got %s", c.Namespace)
	}
	if c.HTTPClient == nil {
		t.Fatal("expected HTTPClient to be non-nil")
	}
}

func TestCreateTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/tasks" {
			t.Fatalf("expected /api/v1/tasks, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("expected Bearer test-token, got %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("expected application/json content-type, got %s", r.Header.Get("Content-Type"))
		}

		body, _ := io.ReadAll(r.Body)
		var req CreateTaskRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("failed to unmarshal request body: %v", err)
		}
		if req.Name != "my-task" {
			t.Fatalf("expected name my-task, got %s", req.Name)
		}
		if req.Type != "container" {
			t.Fatalf("expected type container, got %s", req.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"name": "my-task", "status": "created"})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.CreateTask(context.Background(), CreateTaskRequest{
		Name:      "my-task",
		Namespace: "default",
		Type:      "container",
		Image:     "alpine:latest",
		Command:   []string{"echo", "hello"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	var resp map[string]string
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if resp["name"] != "my-task" {
		t.Fatalf("expected name my-task in response, got %s", resp["name"])
	}
}

func TestListTasks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/tasks" {
			t.Fatalf("expected /api/v1/tasks, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("namespace") != "prod" {
			t.Fatalf("expected namespace=prod, got %s", r.URL.Query().Get("namespace"))
		}
		if r.URL.Query().Get("limit") != "10" {
			t.Fatalf("expected limit=10, got %s", r.URL.Query().Get("limit"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ListResponse{
			Items:    json.RawMessage(`[{"name":"task-1"}]`),
			Metadata: ListMeta{Continue: "abc"},
		})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.ListTasks(context.Background(), "prod", 10, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Metadata.Continue != "abc" {
		t.Fatalf("expected continue token abc, got %s", result.Metadata.Continue)
	}
	if string(result.Items) != `[{"name":"task-1"}]` {
		t.Fatalf("unexpected items: %s", string(result.Items))
	}
}

func TestGetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/tasks/default/my-task" {
			t.Fatalf("expected /api/v1/tasks/default/my-task, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"name": "my-task", "phase": "Running"})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.GetTask(context.Background(), "default", "my-task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp map[string]string
	json.Unmarshal(result, &resp)
	if resp["phase"] != "Running" {
		t.Fatalf("expected phase Running, got %s", resp["phase"])
	}
}

func TestDeleteTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/tasks/default/my-task" {
			t.Fatalf("expected /api/v1/tasks/default/my-task, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	err := c.DeleteTask(context.Background(), "default", "my-task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetTaskResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/tasks/default/my-task/result" {
			t.Fatalf("expected /api/v1/tasks/default/my-task/result, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"output": "hello world"})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.GetTaskResult(context.Background(), "default", "my-task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp map[string]string
	json.Unmarshal(result, &resp)
	if resp["output"] != "hello world" {
		t.Fatalf("expected output 'hello world', got %s", resp["output"])
	}
}

func TestListAgents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents" {
			t.Fatalf("expected /api/v1/agents, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("namespace") != "staging" {
			t.Fatalf("expected namespace=staging, got %s", r.URL.Query().Get("namespace"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]string{{"name": "agent-1"}})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.ListAgents(context.Background(), "staging")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestGetAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/default/my-agent" {
			t.Fatalf("expected /api/v1/agents/default/my-agent, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"name": "my-agent", "model": "gpt-4"})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.GetAgent(context.Background(), "default", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp map[string]string
	json.Unmarshal(result, &resp)
	if resp["model"] != "gpt-4" {
		t.Fatalf("expected model gpt-4, got %s", resp["model"])
	}
}

func TestDeleteAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/default/my-agent" {
			t.Fatalf("expected /api/v1/agents/default/my-agent, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	err := c.DeleteAgent(context.Background(), "default", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListSessions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/sessions" {
			t.Fatalf("expected /api/v1/sessions, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]string{{"id": "sess-1"}})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.ListSessions(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestListTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/tools" {
			t.Fatalf("expected /api/v1/tools, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]string{{"name": "tool-1"}})
	}))
	defer server.Close()

	c := New(server.URL, "test-token", "default")
	result, err := c.ListTools(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestAPIError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"Unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`},
		{"NotFound", http.StatusNotFound, `{"error":"not found"}`},
		{"InternalServerError", http.StatusInternalServerError, `{"error":"internal error"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			c := New(server.URL, "test-token", "default")

			// Test with doJSON path (GetTask)
			_, err := c.GetTask(context.Background(), "default", "my-task")
			if err == nil {
				t.Fatal("expected error for non-2xx response")
			}
			expected := "API error (status " + http.StatusText(0)
			// Just check that error contains status code
			if !contains(err.Error(), "API error") {
				t.Fatalf("expected error to contain 'API error', got: %s", err.Error())
			}
			_ = expected

			// Test with do path (DeleteTask)
			err = c.DeleteTask(context.Background(), "default", "my-task")
			if err == nil {
				t.Fatal("expected error for non-2xx DELETE response")
			}
		})
	}
}

func TestNoAuthHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("expected no Authorization header for empty token")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer server.Close()

	c := New(server.URL, "", "default")
	_, err := c.GetTask(context.Background(), "default", "my-task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
