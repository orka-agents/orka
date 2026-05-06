/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRememberMemoryToolExecute_BlankContent(t *testing.T) {
	tool := NewRememberMemoryTool()

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"content":" \n\t "}`))
	if err == nil {
		t.Fatal("expected error for blank content")
	}
	if !strings.Contains(err.Error(), "content is required") {
		t.Fatalf("expected content required error, got %v", err)
	}
}

func TestRememberMemoryToolExecute_PostsMemoryProposal(t *testing.T) {
	received := make(chan proposeMemoryPayload, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/internal/v1/memory-proposals/test-ns" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Errorf("content-type = %q", got)
		}

		var payload proposeMemoryPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		received <- payload

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"proposed"}`))
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL+"/")
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "task-a")
	t.Setenv("ORKA_AGENT_NAME", "agent-a")
	t.Setenv("ORKA_SA_TOKEN", "test-token")

	tool := NewRememberMemoryTool()
	got, err := tool.Execute(context.Background(), json.RawMessage(`{
		"title": "  Use batched APIs  ",
		"description": "  Useful convention  ",
		"content": "  Batch API calls when possible.  ",
		"tags": [" convention ", "", "test"]
	}`))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got != `{"status":"proposed"}` {
		t.Fatalf("response = %s", got)
	}

	payload := <-received
	if payload.Namespace != "test-ns" {
		t.Errorf("namespace = %q", payload.Namespace)
	}
	if payload.TaskName != "task-a" {
		t.Errorf("task_name = %q", payload.TaskName)
	}
	if payload.AgentName != "agent-a" {
		t.Errorf("agent_name = %q", payload.AgentName)
	}
	if payload.Type != "memory" {
		t.Errorf("type = %q", payload.Type)
	}
	if payload.Title != "Use batched APIs" {
		t.Errorf("title = %q", payload.Title)
	}
	if payload.Content != "Batch API calls when possible." {
		t.Errorf("content = %q", payload.Content)
	}
	if payload.Description != "Useful convention\n\nTags: convention, test" {
		t.Errorf("description = %q", payload.Description)
	}
	if payload.SkillName != "" {
		t.Errorf("skill_name = %q, want empty", payload.SkillName)
	}
}

func TestRememberMemoryToolExecute_DerivesTitleWhenOmitted(t *testing.T) {
	received := make(chan proposeMemoryPayload, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload proposeMemoryPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		received <- payload
		_, _ = w.Write([]byte(`ok`))
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "task-a")
	t.Setenv("ORKA_AGENT_NAME", "agent-a")
	t.Setenv("ORKA_SA_TOKEN", "test-token")

	tool := NewRememberMemoryTool()
	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"content": "\n\n  Future tasks should run go test ./internal/tools before handoff.\nDetails follow."
	}`))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	payload := <-received
	if payload.Title != "Future tasks should run go test ./internal/tools before handoff." {
		t.Fatalf("title = %q", payload.Title)
	}
	if payload.Type != "memory" {
		t.Errorf("type = %q", payload.Type)
	}
}
