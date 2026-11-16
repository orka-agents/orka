/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
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

type recordingMemoryRecalls struct {
	memories []store.Memory
	recalls  []store.MemoryRecall
	err      error
}

func (r *recordingMemoryRecalls) ListMemories(context.Context, store.MemoryFilter) ([]store.Memory, error) {
	return r.memories, nil
}

func (r *recordingMemoryRecalls) RecordMemoryRecall(_ context.Context, recall store.MemoryRecall) error {
	r.recalls = append(r.recalls, recall)
	return r.err
}

func TestRecallMemoryToolRecordsBrokeredRecall(t *testing.T) {
	for name, recordErr := range map[string]error{"recorded": nil, "record fails": errors.New("store unavailable")} {
		t.Run(name, func(t *testing.T) {
			memories := &recordingMemoryRecalls{
				memories: []store.Memory{{ID: "mem-a", Namespace: "test-ns"}, {ID: "mem-b", Namespace: "test-ns"}},
				err:      recordErr,
			}
			ctx := WithToolContext(context.Background(), &ToolContext{Brokered: true, Namespace: "test-ns", TaskID: "task-a",
				MemoryReader: memories, MemoryRecallRecorder: memories})

			got, err := NewRecallMemoryTool().Execute(ctx, json.RawMessage(`{"query":"deploy"}`))
			if err != nil {
				t.Fatalf("Execute() error = %v; a failed count must not fail the recall", err)
			}
			if !strings.Contains(got, `"id":"mem-a"`) || !strings.Contains(got, `"id":"mem-b"`) {
				t.Fatalf("Execute() = %s", got)
			}
			if len(memories.recalls) != 1 {
				t.Fatalf("recorded %d recalls, want 1", len(memories.recalls))
			}
			recall := memories.recalls[0]
			if recall.Namespace != "test-ns" || !slices.Equal(recall.IDs, []string{"mem-a", "mem-b"}) || recall.RecalledAt.IsZero() {
				t.Fatalf("recall = %#v", recall)
			}
		})
	}
}

func TestRecallMemoryToolRecordsNothingForAnEmptyRecall(t *testing.T) {
	memories := &recordingMemoryRecalls{}
	ctx := WithToolContext(context.Background(), &ToolContext{Brokered: true, Namespace: "test-ns", TaskID: "task-a",
		MemoryReader: memories, MemoryRecallRecorder: memories})
	if _, err := NewRecallMemoryTool().Execute(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(memories.recalls) != 0 {
		t.Fatalf("recorded %#v for a recall that returned nothing", memories.recalls)
	}
}

func TestRecallMemoryToolMarksNativeRecall(t *testing.T) {
	purposes := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		purposes <- r.URL.Query().Get(MemoryListPurposeParam)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	t.Setenv("ORKA_TASK_NAME", "task-a")
	t.Setenv("ORKA_SA_TOKEN", "test-token")

	if _, err := NewRecallMemoryTool().Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := <-purposes; got != MemoryListPurposeRecall {
		t.Fatalf("purpose = %q, want %q so the controller counts the recall", got, MemoryListPurposeRecall)
	}
}

// blockingMemoryRecalls never finishes recording a recall until its context
// ends, like a storage write that keeps retrying.
type blockingMemoryRecalls struct {
	recordingMemoryRecalls
}

func (r *blockingMemoryRecalls) RecordMemoryRecall(ctx context.Context, _ store.MemoryRecall) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRecallMemoryToolDoesNotWaitOnASlowCount(t *testing.T) {
	previous := MemoryRecallRecordTimeout
	MemoryRecallRecordTimeout = 20 * time.Millisecond
	t.Cleanup(func() { MemoryRecallRecordTimeout = previous })
	memories := &blockingMemoryRecalls{recordingMemoryRecalls{memories: []store.Memory{{ID: "mem-a", Namespace: "test-ns"}}}}
	ctx := WithToolContext(context.Background(), &ToolContext{Brokered: true, Namespace: "test-ns", TaskID: "task-a",
		MemoryReader: memories, MemoryRecallRecorder: memories})

	started := time.Now()
	got, err := NewRecallMemoryTool().Execute(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Contains(t, got, `"id":"mem-a"`)
	require.Less(t, time.Since(started), 2*time.Second, "a stuck count gives up within its budget")
}
