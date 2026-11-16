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
	"sync"
	"sync/atomic"
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
	err      error
	mu       sync.Mutex
	recalls  []store.MemoryRecall
}

func (r *recordingMemoryRecalls) ListMemories(context.Context, store.MemoryFilter) ([]store.Memory, error) {
	return r.memories, nil
}

func (r *recordingMemoryRecalls) RecordMemoryRecall(_ context.Context, recall store.MemoryRecall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recalls = append(r.recalls, recall)
	return r.err
}

func (r *recordingMemoryRecalls) recorded() []store.MemoryRecall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.MemoryRecall(nil), r.recalls...)
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
			require.Eventually(t, func() bool { return len(memories.recorded()) == 1 }, 5*time.Second, 5*time.Millisecond,
				"the count runs in the background")
			recall := memories.recorded()[0]
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
	if recorded := memories.recorded(); len(recorded) != 0 {
		t.Fatalf("recorded %#v for a recall that returned nothing", recorded)
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
	*recordingMemoryRecalls
}

func (r *blockingMemoryRecalls) RecordMemoryRecall(ctx context.Context, _ store.MemoryRecall) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRecallMemoryToolDoesNotWaitOnASlowCount(t *testing.T) {
	previous := MemoryRecallRecordTimeout
	MemoryRecallRecordTimeout = 20 * time.Millisecond
	t.Cleanup(func() { MemoryRecallRecordTimeout = previous })
	memories := &blockingMemoryRecalls{&recordingMemoryRecalls{memories: []store.Memory{{ID: "mem-a", Namespace: "test-ns"}}}}
	ctx := WithToolContext(context.Background(), &ToolContext{Brokered: true, Namespace: "test-ns", TaskID: "task-a",
		MemoryReader: memories, MemoryRecallRecorder: memories})

	started := time.Now()
	got, err := NewRecallMemoryTool().Execute(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Contains(t, got, `"id":"mem-a"`)
	require.Less(t, time.Since(started), time.Second, "a stuck count never delays the recall")
}

func TestRecordMemoryRecallDropsCountsBeyondItsSlots(t *testing.T) {
	previous := MemoryRecallRecordTimeout
	MemoryRecallRecordTimeout = time.Minute
	t.Cleanup(func() { MemoryRecallRecordTimeout = previous })
	require.Eventually(t, func() bool { return len(memoryRecallSlots) == 0 }, 5*time.Second, 5*time.Millisecond,
		"counts from earlier tests have finished")
	release := make(chan struct{})
	stuck := &gatedMemoryRecalls{release: release}
	for range cap(memoryRecallSlots) + 5 {
		RecordMemoryRecall(context.Background(), stuck, "test-ns", []store.Memory{{ID: "mem-a"}})
	}
	require.Eventually(t, func() bool { return stuck.started.Load() == int32(cap(memoryRecallSlots)) }, 5*time.Second, 5*time.Millisecond)
	close(release)
	require.Eventually(t, func() bool { return len(memoryRecallSlots) == 0 }, 5*time.Second, 5*time.Millisecond,
		"finished counts free their slots")
	require.Equal(t, int32(cap(memoryRecallSlots)), stuck.started.Load(), "counts beyond the slots are dropped, not queued")
}

type gatedMemoryRecalls struct {
	release chan struct{}
	started atomic.Int32
}

func (g *gatedMemoryRecalls) RecordMemoryRecall(context.Context, store.MemoryRecall) error {
	g.started.Add(1)
	<-g.release
	return nil
}

func TestRecordAuthorizedMemoryRecallChecksInTheBackground(t *testing.T) {
	require.Eventually(t, func() bool { return len(memoryRecallSlots) == 0 }, 5*time.Second, 5*time.Millisecond)
	for name, allowed := range map[string]bool{"allowed": true, "refused": false} {
		t.Run(name, func(t *testing.T) {
			memories := &recordingMemoryRecalls{}
			release := make(chan struct{})
			started := time.Now()
			RecordAuthorizedMemoryRecall(context.Background(), func(context.Context) bool {
				<-release
				return allowed
			}, memories, "test-ns", []store.Memory{{ID: "mem-a"}})
			require.Less(t, time.Since(started), time.Second, "a slow caller check never delays the recall")
			close(release)
			require.Eventually(t, func() bool { return len(memoryRecallSlots) == 0 }, 5*time.Second, 5*time.Millisecond)
			if allowed {
				require.Len(t, memories.recorded(), 1)
			} else {
				require.Empty(t, memories.recorded(), "a refused caller's recall is not counted")
			}
		})
	}
}
