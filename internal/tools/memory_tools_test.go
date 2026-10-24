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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
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

func TestRecallMemoryToolUsesStrictSearchAndNormalizesMemoryArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/memories/test-ns/search" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["query"] != "sqlite" || request["mode"] != "keyword" {
			t.Fatalf("search request = %#v", request)
		}
		if request["limit"] != float64(maxRecallMemoryToolPageLimit) {
			t.Fatalf("limit = %#v", request["limit"])
		}
		trust, _ := request["trust"].([]any)
		if len(trust) != 2 || trust[0] != "reviewed" || trust[1] != "trusted" {
			t.Fatalf("trust = %#v", request["trust"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"memory":{"id":"mem-1","content":"use sqlite"},"score":1}],"actualMode":"keyword","exhausted":true,"complete":true}`))
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "task-a")
	t.Setenv("ORKA_SA_TOKEN", "test-token")
	t.Setenv(workerenv.TransactionTokenFile, "")
	result, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"query":"sqlite","limit":5}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != `[{"id":"mem-1","content":"use sqlite"}]` {
		t.Fatalf("result = %s", result)
	}
}

func TestRecallMemoryToolPropagatesMountedTaskTransactionToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "transaction-token")
	if err := os.WriteFile(tokenPath, []byte(" task-scoped-token \n"), 0o600); err != nil {
		t.Fatalf("write transaction token: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer service-account-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(internalMemoryTransactionTokenHeader); got != "task-scoped-token" {
			t.Errorf("%s = %q", internalMemoryTransactionTokenHeader, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"actualMode":"keyword","exhausted":true,"complete":true}`))
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, tokenPath)

	if _, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"query":"sqlite"}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestRecallMemoryToolFailsClosedWhenMountedTransactionTokenCannotBeRead(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, filepath.Join(t.TempDir(), "missing-token"))

	_, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"query":"sqlite"}`))
	if err == nil || !strings.Contains(err.Error(), "failed to load task transaction token") {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("controller requests = %d, want 0", got)
	}
}

func TestRecallMemoryToolUsesBoundedPageWhenLimitNotProvided(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["limit"] != float64(maxRecallMemoryToolPageLimit) {
			t.Fatalf("bounded default page limit = %#v", request["limit"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"actualMode":"keyword","exhausted":true,"complete":true}`))
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "task-a")
	t.Setenv("ORKA_SA_TOKEN", "test-token")
	t.Setenv(workerenv.TransactionTokenFile, "")
	if _, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"query":"sqlite"}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestRecallMemoryToolPaginatesResponsesBeyondBodyLimit(t *testing.T) {
	const total = 17
	content := strings.Repeat("<", 64<<10)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var request struct {
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Limit <= 0 || request.Limit > maxRecallMemoryToolPageLimit {
			t.Fatalf("page limit = %d, want 1..%d", request.Limit, maxRecallMemoryToolPageLimit)
		}
		index := 0
		if request.Cursor != "" {
			parsed, err := strconv.Atoi(request.Cursor)
			if err != nil {
				t.Fatalf("cursor = %q: %v", request.Cursor, err)
			}
			index = parsed
		}
		count := min(request.Limit, total-index)
		exhausted := index+count >= total
		next := ""
		if !exhausted {
			next = strconv.Itoa(index + count)
		}
		items := make([]any, 0, count)
		for itemIndex := index; itemIndex < index+count; itemIndex++ {
			items = append(items, map[string]any{"memory": map[string]any{
				"id": "memory-" + strconv.Itoa(itemIndex), "content": content,
			}})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"items":  items,
			"cursor": next, "actualMode": "keyword", "exhausted": exhausted, "complete": true,
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, "")

	result, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"limit":17}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var memories []map[string]any
	if err := json.Unmarshal([]byte(result), &memories); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	wantRequests := int32((total + maxRecallMemoryToolPageLimit - 1) / maxRecallMemoryToolPageLimit)
	if len(memories) == 0 || len(memories) >= total || len(result) > internalMemoryToolResultLimit ||
		requests.Load() <= 1 || requests.Load() > wantRequests {
		t.Fatalf("memories/bytes/requests = %d/%d/%d, want bounded nonempty subset within %d bytes and <= %d requests",
			len(memories), len(result), requests.Load(), internalMemoryToolResultLimit, wantRequests)
	}
}

func TestRecallMemoryToolReloadsRotatedTransactionTokenBetweenPages(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "transaction-credential")
	firstValue := strings.Join([]string{"task", "scoped", "one"}, "-")
	secondValue := strings.Join([]string{"task", "scoped", "two"}, "-")
	if err := os.WriteFile(credentialPath, []byte(firstValue), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		wantValue := firstValue
		if requestNumber == 2 {
			wantValue = secondValue
		}
		if got := r.Header.Get(internalMemoryTransactionTokenHeader); got != wantValue {
			t.Fatalf("request %d credential = %q, want %q", requestNumber, got, wantValue)
		}
		w.Header().Set("Content-Type", "application/json")
		if requestNumber == 1 {
			if err := os.WriteFile(credentialPath, []byte(secondValue), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"items":[{"memory":{"id":"mem-1"}}],"cursor":"next","actualMode":"keyword","exhausted":false,"complete":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"memory":{"id":"mem-2"}}],"actualMode":"keyword","exhausted":true,"complete":true}`))
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, credentialPath)
	result, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"limit":2}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var memories []map[string]any
	if err := json.Unmarshal([]byte(result), &memories); err != nil || len(memories) != 2 || requests.Load() != 2 {
		t.Fatalf("memories/requests = %#v/%d, err=%v", memories, requests.Load(), err)
	}
}

func TestRecallMemoryToolAcceptsMaximumLimitWithBoundedPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["limit"] != float64(maxRecallMemoryToolPageLimit) {
			t.Fatalf("page limit = %#v", request["limit"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"actualMode":"keyword","exhausted":true,"complete":true}`))
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, "")
	if _, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"limit":200}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestRecallMemoryToolRejectsOversizedControllerPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", internalMemoryToolBodyLimit+1)))
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, "")
	_, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"limit":1}`))
	if err == nil || !strings.Contains(err.Error(), "controller response exceeded") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestRecallMemoryToolRejectsNonAdvancingCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"cursor":"same","actualMode":"keyword","exhausted":false,"complete":true}`))
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, "")
	_, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"limit":2}`))
	if err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestRecallMemoryToolRejectsNonPositiveLimit(t *testing.T) {
	if _, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"limit":0}`)); err == nil ||
		!strings.Contains(err.Error(), "limit must be between 1 and 200") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestRecallMemoryToolRejectsLimitAboveHardCap(t *testing.T) {
	if _, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{"limit":201}`)); err == nil ||
		!strings.Contains(err.Error(), "limit must be between 1 and 200") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestMemoryProposalToolsPropagateMountedTaskTransactionToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "transaction-token")
	if err := os.WriteFile(tokenPath, []byte(" task-scoped-token \n"), 0o600); err != nil {
		t.Fatalf("write transaction token: %v", err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer service-account-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(internalMemoryTransactionTokenHeader); got != "task-scoped-token" {
			t.Errorf("%s = %q", internalMemoryTransactionTokenHeader, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"proposed"}`))
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, tokenPath)

	if _, err := NewRememberMemoryTool().Execute(context.Background(), json.RawMessage(`{"content":"remember this"}`)); err != nil {
		t.Fatalf("remember Execute() error = %v", err)
	}
	if _, err := NewProposeMemoryTool().Execute(context.Background(), json.RawMessage(`{"title":"proposal","content":"propose this"}`)); err != nil {
		t.Fatalf("propose Execute() error = %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("controller requests = %d, want 2", got)
	}
}

func TestMemoryProposalToolsFailClosedWhenMountedTransactionTokenCannotBeRead(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "test-ns")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")
	t.Setenv(workerenv.TransactionTokenFile, filepath.Join(t.TempDir(), "missing-token"))

	for name, execute := range map[string]func() (string, error){
		"remember": func() (string, error) {
			return NewRememberMemoryTool().Execute(context.Background(), json.RawMessage(`{"content":"remember this"}`))
		},
		"propose": func() (string, error) {
			return NewProposeMemoryTool().Execute(context.Background(), json.RawMessage(`{"title":"proposal","content":"propose this"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execute()
			if err == nil || !strings.Contains(err.Error(), "failed to load task transaction token") {
				t.Fatalf("Execute() error = %v", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("controller requests = %d, want 0", got)
	}
}
