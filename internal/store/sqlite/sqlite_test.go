package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/store"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

func setupTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB(:memory:) failed: %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	return NewStore(db, ":memory:")
}

func TestResultStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	t.Run("save and get result", func(t *testing.T) {
		data := []byte("hello world")
		if err := s.SaveResult(ctx, "ns1", "task1", data); err != nil {
			t.Fatalf("SaveResult: %v", err)
		}

		got, err := s.GetResult(ctx, "ns1", "task1")
		if err != nil {
			t.Fatalf("GetResult: %v", err)
		}
		if string(got) != string(data) {
			t.Errorf("got %q, want %q", got, data)
		}
	})

	t.Run("get nonexistent result returns ErrNotFound", func(t *testing.T) {
		_, err := s.GetResult(ctx, "ns1", "nonexistent")
		if err != store.ErrNotFound {
			t.Errorf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("save overwrites existing result", func(t *testing.T) {
		if err := s.SaveResult(ctx, "ns1", "task-overwrite", []byte("v1")); err != nil {
			t.Fatalf("SaveResult v1: %v", err)
		}
		if err := s.SaveResult(ctx, "ns1", "task-overwrite", []byte("v2")); err != nil {
			t.Fatalf("SaveResult v2: %v", err)
		}

		got, err := s.GetResult(ctx, "ns1", "task-overwrite")
		if err != nil {
			t.Fatalf("GetResult: %v", err)
		}
		if string(got) != "v2" {
			t.Errorf("got %q, want %q", got, "v2")
		}
	})

	t.Run("delete result", func(t *testing.T) {
		if err := s.SaveResult(ctx, "ns1", "task-del", []byte("data")); err != nil {
			t.Fatalf("SaveResult: %v", err)
		}
		if err := s.DeleteResult(ctx, "ns1", "task-del"); err != nil {
			t.Fatalf("DeleteResult: %v", err)
		}

		_, err := s.GetResult(ctx, "ns1", "task-del")
		if err != store.ErrNotFound {
			t.Errorf("got %v, want ErrNotFound after delete", err)
		}
	})

	t.Run("delete nonexistent result is no-op", func(t *testing.T) {
		if err := s.DeleteResult(ctx, "ns1", "never-existed"); err != nil {
			t.Errorf("DeleteResult nonexistent: %v", err)
		}
	})

	t.Run("namespace isolation", func(t *testing.T) {
		if err := s.SaveResult(ctx, "ns-a", "task1", []byte("a-data")); err != nil {
			t.Fatalf("SaveResult ns-a: %v", err)
		}
		if err := s.SaveResult(ctx, "ns-b", "task1", []byte("b-data")); err != nil {
			t.Fatalf("SaveResult ns-b: %v", err)
		}

		got, _ := s.GetResult(ctx, "ns-a", "task1")
		if string(got) != "a-data" {
			t.Errorf("ns-a got %q, want %q", got, "a-data")
		}
		got, _ = s.GetResult(ctx, "ns-b", "task1")
		if string(got) != "b-data" {
			t.Errorf("ns-b got %q, want %q", got, "b-data")
		}
	})
}

func TestSessionStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	t.Run("create and get session", func(t *testing.T) {
		session := &store.SessionRecord{
			Namespace:   "ns1",
			Name:        "session1",
			SessionType: "task",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.CreateSession(ctx, session); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		got, err := s.GetSession(ctx, "ns1", "session1")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.Name != "session1" || got.SessionType != "task" || got.Namespace != "ns1" {
			t.Errorf("unexpected session: %+v", got)
		}
	})

	t.Run("get nonexistent session returns ErrNotFound", func(t *testing.T) {
		_, err := s.GetSession(ctx, "ns1", "nonexistent")
		if err != store.ErrNotFound {
			t.Errorf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("list sessions", func(t *testing.T) {
		// session1 already exists from previous subtest
		session2 := &store.SessionRecord{
			Namespace:   "ns1",
			Name:        "session2",
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.CreateSession(ctx, session2); err != nil {
			t.Fatalf("CreateSession session2: %v", err)
		}

		sessions, err := s.ListSessions(ctx, "ns1")
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if len(sessions) < 2 {
			t.Fatalf("got %d sessions, want at least 2", len(sessions))
		}
	})

	t.Run("list sessions empty namespace", func(t *testing.T) {
		sessions, err := s.ListSessions(ctx, "empty-ns")
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if len(sessions) != 0 {
			t.Errorf("got %d sessions, want 0", len(sessions))
		}
	})

	t.Run("delete session", func(t *testing.T) {
		session := &store.SessionRecord{
			Namespace:   "ns1",
			Name:        "session-del",
			SessionType: "task",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.CreateSession(ctx, session); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := s.DeleteSession(ctx, "ns1", "session-del"); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}

		_, err := s.GetSession(ctx, "ns1", "session-del")
		if err != store.ErrNotFound {
			t.Errorf("got %v, want ErrNotFound after delete", err)
		}
	})
}

func TestSessionLocking(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// Create a session to lock
	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "lock-session",
		SessionType: "task",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	t.Run("acquire lock", func(t *testing.T) {
		if err := s.AcquireLock(ctx, "ns1", "lock-session", "task-a"); err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
	})

	t.Run("double acquire fails", func(t *testing.T) {
		err := s.AcquireLock(ctx, "ns1", "lock-session", "task-b")
		if err == nil {
			t.Fatal("expected error on double acquire, got nil")
		}
	})

	t.Run("is locked by different task", func(t *testing.T) {
		locked, err := s.IsLocked(ctx, "ns1", "lock-session", "task-b")
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if !locked {
			t.Error("expected locked=true for different task")
		}
	})

	t.Run("is not locked for current holder", func(t *testing.T) {
		locked, err := s.IsLocked(ctx, "ns1", "lock-session", "task-a")
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if locked {
			t.Error("expected locked=false for current holder")
		}
	})

	t.Run("release and re-acquire", func(t *testing.T) {
		if err := s.ReleaseLock(ctx, "ns1", "lock-session", "task-a"); err != nil {
			t.Fatalf("ReleaseLock: %v", err)
		}

		locked, err := s.IsLocked(ctx, "ns1", "lock-session", "task-b")
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if locked {
			t.Error("expected locked=false after release")
		}

		if err := s.AcquireLock(ctx, "ns1", "lock-session", "task-b"); err != nil {
			t.Fatalf("re-AcquireLock: %v", err)
		}
	})

	t.Run("is locked for nonexistent session returns ErrNotFound", func(t *testing.T) {
		_, err := s.IsLocked(ctx, "ns1", "nonexistent", "task-a")
		if err != store.ErrNotFound {
			t.Errorf("got %v, want ErrNotFound", err)
		}
	})
}

func TestSessionMessages(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// Create session for message tests
	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "msg-session",
		SessionType: "task",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	t.Run("append and load messages", func(t *testing.T) {
		messages := []store.SessionMessage{
			{Role: roleUser, Content: "hello", Timestamp: now},
			{Role: roleAssistant, Content: "hi there", Timestamp: now},
		}
		if err := s.AppendMessages(ctx, "ns1", "msg-session", messages); err != nil {
			t.Fatalf("AppendMessages: %v", err)
		}

		got, err := s.LoadTranscript(ctx, "ns1", "msg-session", 0)
		if err != nil {
			t.Fatalf("LoadTranscript: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2", len(got))
		}
		if got[0].Role != roleUser || got[0].Content != "hello" {
			t.Errorf("message 0: got %+v", got[0])
		}
		if got[1].Role != roleAssistant || got[1].Content != "hi there" {
			t.Errorf("message 1: got %+v", got[1])
		}
	})

	t.Run("append updates message count", func(t *testing.T) {
		got, err := s.GetSession(ctx, "ns1", "msg-session")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.MessageCount != 2 {
			t.Errorf("got message_count=%d, want 2", got.MessageCount)
		}

		// Append more
		moreMessages := []store.SessionMessage{
			{Role: roleUser, Content: "follow up", Timestamp: now},
		}
		if err := s.AppendMessages(ctx, "ns1", "msg-session", moreMessages); err != nil {
			t.Fatalf("AppendMessages: %v", err)
		}

		got, err = s.GetSession(ctx, "ns1", "msg-session")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.MessageCount != 3 {
			t.Errorf("got message_count=%d, want 3", got.MessageCount)
		}
	})

	t.Run("load transcript with max messages limit", func(t *testing.T) {
		got, err := s.LoadTranscript(ctx, "ns1", "msg-session", 2)
		if err != nil {
			t.Fatalf("LoadTranscript with limit: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2", len(got))
		}
		// Should be first 2 messages (ordered by id)
		if got[0].Content != "hello" {
			t.Errorf("first message: got %q, want %q", got[0].Content, "hello")
		}
	})

	t.Run("messages with structured fields", func(t *testing.T) {
		structSession := &store.SessionRecord{
			Namespace:   "ns1",
			Name:        "struct-session",
			SessionType: "task",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.CreateSession(ctx, structSession); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		messages := []store.SessionMessage{
			{
				Role:       roleAssistant,
				Content:    "",
				Name:       "tool-use",
				Input:      map[string]any{"key": "value", "num": float64(42)},
				ToolCalls:  []map[string]any{{"id": "call1", "type": "function"}},
				ToolCallID: "call1",
				Timestamp:  now,
			},
		}
		if err := s.AppendMessages(ctx, "ns1", "struct-session", messages); err != nil {
			t.Fatalf("AppendMessages: %v", err)
		}

		got, err := s.LoadTranscript(ctx, "ns1", "struct-session", 0)
		if err != nil {
			t.Fatalf("LoadTranscript: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}

		msg := got[0]
		if msg.Name != "tool-use" {
			t.Errorf("Name: got %q, want %q", msg.Name, "tool-use")
		}
		if msg.ToolCallID != "call1" {
			t.Errorf("ToolCallID: got %q, want %q", msg.ToolCallID, "call1")
		}
		if msg.Input["key"] != "value" {
			t.Errorf("Input[key]: got %v, want %q", msg.Input["key"], "value")
		}
		if msg.Input["num"] != float64(42) {
			t.Errorf("Input[num]: got %v, want 42", msg.Input["num"])
		}
		// ToolCalls is deserialized as []any
		toolCalls, ok := msg.ToolCalls.([]any)
		if !ok {
			t.Fatalf("ToolCalls: expected []any, got %T", msg.ToolCalls)
		}
		if len(toolCalls) != 1 {
			t.Fatalf("ToolCalls: got %d items, want 1", len(toolCalls))
		}
	})
}

func TestCascadeDelete(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// Create session with messages
	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "cascade-session",
		SessionType: "task",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	messages := []store.SessionMessage{
		{Role: roleUser, Content: "msg1", Timestamp: now},
		{Role: roleAssistant, Content: "msg2", Timestamp: now},
	}
	if err := s.AppendMessages(ctx, "ns1", "cascade-session", messages); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	// Verify messages exist
	got, err := s.LoadTranscript(ctx, "ns1", "cascade-session", 0)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages before delete, want 2", len(got))
	}

	// Delete session
	if err := s.DeleteSession(ctx, "ns1", "cascade-session"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Verify messages are also deleted (via CASCADE)
	got, err = s.LoadTranscript(ctx, "ns1", "cascade-session", 0)
	if err != nil {
		t.Fatalf("LoadTranscript after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d messages after cascade delete, want 0", len(got))
	}
}

func TestHealthCheck(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Test concurrent writes don't error (SQLite serializes them via MaxOpenConns(1))
	const n = 20
	errs := make(chan error, n)
	for i := range n {
		go func(i int) {
			errs <- s.SaveResult(ctx, "ns1", fmt.Sprintf("task-%d", i), fmt.Appendf(nil, "data-%d", i))
		}(i)
	}

	for range n {
		if err := <-errs; err != nil {
			t.Errorf("concurrent SaveResult: %v", err)
		}
	}

	// Verify all results were saved
	for i := range n {
		got, err := s.GetResult(ctx, "ns1", fmt.Sprintf("task-%d", i))
		if err != nil {
			t.Errorf("GetResult task-%d: %v", i, err)
			continue
		}
		expected := fmt.Sprintf("data-%d", i)
		if string(got) != expected {
			t.Errorf("task-%d: got %q, want %q", i, got, expected)
		}
	}
}

func TestGetSessionIncludesMessages(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "full-session",
		SessionType: "chat",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	messages := []store.SessionMessage{
		{Role: roleUser, Content: "first", Timestamp: now},
		{Role: roleAssistant, Content: "second", Timestamp: now},
	}
	if err := s.AppendMessages(ctx, "ns1", "full-session", messages); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	got, err := s.GetSession(ctx, "ns1", "full-session")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if len(got.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(got.Messages))
	}
	if got.Messages[0].Content != "first" || got.Messages[1].Content != "second" {
		t.Errorf("unexpected messages: %+v", got.Messages)
	}
}

func TestPlanStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	t.Run("save and get plan", func(t *testing.T) {
		plan := &store.PlanState{
			TaskName:     "test-task",
			Namespace:    "default",
			Iteration:    0,
			Summary:      "Initial planning phase",
			ProgressPct:  10,
			GoalComplete: false,
			PlanDocument: "# Plan\n- Step 1\n- Step 2",
		}
		if err := s.SavePlan(ctx, "default", "test-task", plan); err != nil {
			t.Fatalf("SavePlan: %v", err)
		}

		got, err := s.GetPlan(ctx, "default", "test-task")
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if got.Summary != plan.Summary {
			t.Errorf("Summary = %q, want %q", got.Summary, plan.Summary)
		}
		if got.ProgressPct != plan.ProgressPct {
			t.Errorf("ProgressPct = %d, want %d", got.ProgressPct, plan.ProgressPct)
		}
		if got.PlanDocument != plan.PlanDocument {
			t.Errorf("PlanDocument = %q, want %q", got.PlanDocument, plan.PlanDocument)
		}
		if got.GoalComplete {
			t.Error("GoalComplete should be false")
		}
	})

	t.Run("upsert plan", func(t *testing.T) {
		plan := &store.PlanState{
			Iteration:    1,
			Summary:      "Updated progress",
			ProgressPct:  50,
			GoalComplete: false,
			PlanDocument: "# Updated Plan",
		}
		if err := s.SavePlan(ctx, "default", "test-task", plan); err != nil {
			t.Fatalf("SavePlan (update): %v", err)
		}

		got, err := s.GetPlan(ctx, "default", "test-task")
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if got.Summary != "Updated progress" {
			t.Errorf("Summary = %q, want %q", got.Summary, "Updated progress")
		}
		if got.ProgressPct != 50 {
			t.Errorf("ProgressPct = %d, want %d", got.ProgressPct, 50)
		}
	})

	t.Run("get nonexistent plan", func(t *testing.T) {
		_, err := s.GetPlan(ctx, "default", "nonexistent")
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("delete plan", func(t *testing.T) {
		plan := &store.PlanState{
			Summary:      "To be deleted",
			PlanDocument: "# Delete me",
		}
		if err := s.SavePlan(ctx, "default", "delete-task", plan); err != nil {
			t.Fatalf("SavePlan: %v", err)
		}

		if err := s.DeletePlan(ctx, "default", "delete-task"); err != nil {
			t.Fatalf("DeletePlan: %v", err)
		}

		_, err := s.GetPlan(ctx, "default", "delete-task")
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expected ErrNotFound after delete, got %v", err)
		}
	})

	t.Run("goal complete flag", func(t *testing.T) {
		plan := &store.PlanState{
			Summary:      "All done",
			ProgressPct:  100,
			GoalComplete: true,
			PlanDocument: "# Complete",
		}
		if err := s.SavePlan(ctx, "default", "complete-task", plan); err != nil {
			t.Fatalf("SavePlan: %v", err)
		}

		got, err := s.GetPlan(ctx, "default", "complete-task")
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if !got.GoalComplete {
			t.Error("GoalComplete should be true")
		}
	})

	t.Run("namespace isolation", func(t *testing.T) {
		plan := &store.PlanState{
			Summary:      "NS1 plan",
			PlanDocument: "# NS1",
		}
		if err := s.SavePlan(ctx, "ns1", "task-a", plan); err != nil {
			t.Fatalf("SavePlan ns1: %v", err)
		}

		_, err := s.GetPlan(ctx, "ns2", "task-a")
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expected ErrNotFound for different namespace, got %v", err)
		}
	})
}
