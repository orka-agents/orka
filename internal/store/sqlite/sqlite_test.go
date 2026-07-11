package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const contentHello = "hello"

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

func TestChatTurnMigrationPreservesLegacySessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-sessions.db")
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacyDB.Exec(`CREATE TABLE sessions (
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		session_type TEXT NOT NULL DEFAULT 'task',
		active_task TEXT NOT NULL DEFAULT '',
		message_count INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cancelled BOOLEAN NOT NULL DEFAULT FALSE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (namespace, name)
	)`); err != nil {
		_ = legacyDB.Close()
		t.Fatalf("create legacy sessions table: %v", err)
	}
	if _, err := legacyDB.Exec(`INSERT INTO sessions (namespace, name, session_type) VALUES ('ns1', 'legacy-chat', 'chat')`); err != nil {
		_ = legacyDB.Close()
		t.Fatalf("insert legacy session: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, dbPath)
	now := time.Now()
	if err := s.AcquireChatTurn(context.Background(), &store.SessionRecord{
		Namespace: "ns1", Name: "legacy-chat", SessionType: "chat", CreatedAt: now, UpdatedAt: now,
	}, "chat-turn-migrated", now.Add(time.Hour)); err != nil {
		t.Fatalf("AcquireChatTurn on migrated session: %v", err)
	}
	if err := s.ReleaseChatTurn(context.Background(), "ns1", "legacy-chat", "chat-turn-migrated"); err != nil {
		t.Fatalf("ReleaseChatTurn on migrated session: %v", err)
	}
	session, err := s.GetSession(context.Background(), "ns1", "legacy-chat")
	if err != nil {
		t.Fatalf("GetSession after migration: %v", err)
	}
	if session.SessionType != "chat" || session.MessageCount != 0 || session.InputTokens != 0 || session.OutputTokens != 0 {
		t.Fatalf("legacy session changed during migration: %+v", session)
	}
}

func TestAcquireChatTurn(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	session := &store.SessionRecord{Namespace: "ns1", Name: "chat-lock", SessionType: "chat", CreatedAt: now, UpdatedAt: now}

	if err := s.AcquireChatTurn(ctx, session, "chat-turn-a", now.Add(time.Minute)); err != nil {
		t.Fatalf("AcquireChatTurn new session: %v", err)
	}
	got, err := s.GetSession(ctx, "ns1", "chat-lock")
	if err != nil {
		t.Fatalf("GetSession locked chat: %v", err)
	}
	if got.ActiveTask != "" {
		t.Fatalf("chat reservation occupied task lock: %q", got.ActiveTask)
	}
	var activeChatTurn string
	if err := s.db.QueryRow(`SELECT chat_turn_id FROM sessions WHERE namespace = 'ns1' AND name = 'chat-lock'`).Scan(&activeChatTurn); err != nil {
		t.Fatalf("read chat turn owner: %v", err)
	}
	if activeChatTurn != "chat-turn-a" {
		t.Fatalf("chat turn owner = %q, want chat-turn-a", activeChatTurn)
	}
	locked, err := s.IsLocked(ctx, "ns1", "chat-lock", "task-a")
	if err != nil {
		t.Fatalf("IsLocked during chat turn: %v", err)
	}
	if locked {
		t.Fatal("chat reservation incorrectly occupied the task lock domain")
	}
	if err := s.AcquireLock(ctx, "ns1", "chat-lock", "task-a"); err != nil {
		t.Fatalf("AcquireLock during chat turn: %v", err)
	}
	if err := s.CommitSessionTurn(ctx, session, "chat-turn-a", 0, []store.SessionMessage{{Role: roleUser, Content: "checkpoint while task runs"}}, 1, 1); err != nil {
		t.Fatalf("CommitSessionTurn while task lock active: %v", err)
	}
	if err := s.ReleaseLock(ctx, "ns1", "chat-lock", "task-a"); err != nil {
		t.Fatalf("ReleaseLock during chat turn: %v", err)
	}

	if err := s.AcquireChatTurn(ctx, session, "chat-turn-b", now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("concurrent AcquireChatTurn error = %v, want ErrConflict", err)
	}
	if err := s.ReleaseChatTurn(ctx, "ns1", "chat-lock", "chat-turn-a"); err != nil {
		t.Fatalf("ReleaseChatTurn chat turn: %v", err)
	}
	if err := s.AcquireChatTurn(ctx, session, "chat-turn-b", now.Add(time.Minute)); err != nil {
		t.Fatalf("AcquireChatTurn after release: %v", err)
	}

	if _, err := s.db.Exec(`UPDATE sessions SET chat_turn_id = 'chat-turn-stale', chat_turn_expires_at = ? WHERE namespace = 'ns1' AND name = 'chat-lock'`, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("age chat reservation: %v", err)
	}
	if err := s.AcquireChatTurn(ctx, session, "chat-turn-c", now.Add(time.Hour)); err != nil {
		t.Fatalf("AcquireChatTurn stale takeover: %v", err)
	}
	if err := s.CommitSessionTurn(ctx, session, "chat-turn-b", 0, []store.SessionMessage{{Role: roleUser, Content: "stale owner"}}, 1, 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale owner CommitSessionTurn error = %v, want ErrConflict", err)
	}
	if err := s.CommitSessionTurn(ctx, session, "chat-turn-c", 1, []store.SessionMessage{{Role: roleUser, Content: "current owner"}}, 1, 1); err != nil {
		t.Fatalf("current owner CommitSessionTurn: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET chat_turn_expires_at = ? WHERE namespace = 'ns1' AND name = 'chat-lock'`, now.Add(-time.Minute)); err != nil {
		t.Fatalf("expire chat reservation: %v", err)
	}
	if err := s.CommitSessionTurn(ctx, session, "chat-turn-c", 2, []store.SessionMessage{{Role: roleAssistant, Content: "too late"}}, 2, 3); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expired owner CommitSessionTurn error = %v, want ErrConflict", err)
	}
	if err := s.AcquireChatTurn(ctx, session, "chat-turn-after-expiry", now.Add(time.Hour)); err != nil {
		t.Fatalf("AcquireChatTurn did not reclaim expired reservation: %v", err)
	}
	if err := s.ReleaseChatTurn(ctx, "ns1", "chat-lock", "chat-turn-after-expiry"); err != nil {
		t.Fatalf("ReleaseChatTurn after expiration: %v", err)
	}

	taskSession := &store.SessionRecord{Namespace: "ns1", Name: "task-lock", SessionType: "task", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateSession(ctx, taskSession); err != nil {
		t.Fatalf("CreateSession task lock: %v", err)
	}
	if err := s.AcquireLock(ctx, "ns1", "task-lock", "chat-turn-task-owner"); err != nil {
		t.Fatalf("AcquireLock task owner: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE namespace = 'ns1' AND name = 'task-lock'`, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("age task lock: %v", err)
	}
	if err := s.AcquireChatTurn(ctx, taskSession, "chat-turn-d", now.Add(time.Hour)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("AcquireChatTurn stole task lock: %v", err)
	}
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
			{Role: roleUser, Content: contentHello, Timestamp: now},
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
		if got[0].Role != roleUser || got[0].Content != contentHello {
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
		if got[0].Content != contentHello {
			t.Errorf("first message: got %q, want %q", got[0].Content, contentHello)
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

func TestUpdateTokenCounts(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "token-sess",
		SessionType: "chat",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	t.Run("increment tokens", func(t *testing.T) {
		if err := s.UpdateTokenCounts(ctx, "ns1", "token-sess", 100, 200); err != nil {
			t.Fatalf("UpdateTokenCounts: %v", err)
		}
		got, err := s.GetSession(ctx, "ns1", "token-sess")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.InputTokens != 100 || got.OutputTokens != 200 {
			t.Errorf("tokens = (%d, %d), want (100, 200)", got.InputTokens, got.OutputTokens)
		}
	})

	t.Run("accumulate tokens", func(t *testing.T) {
		if err := s.UpdateTokenCounts(ctx, "ns1", "token-sess", 50, 75); err != nil {
			t.Fatalf("UpdateTokenCounts: %v", err)
		}
		got, err := s.GetSession(ctx, "ns1", "token-sess")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.InputTokens != 150 || got.OutputTokens != 275 {
			t.Errorf("tokens = (%d, %d), want (150, 275)", got.InputTokens, got.OutputTokens)
		}
	})

	t.Run("zero increment is no-op", func(t *testing.T) {
		if err := s.UpdateTokenCounts(ctx, "ns1", "token-sess", 0, 0); err != nil {
			t.Fatalf("UpdateTokenCounts zero: %v", err)
		}
		got, err := s.GetSession(ctx, "ns1", "token-sess")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.InputTokens != 150 || got.OutputTokens != 275 {
			t.Errorf("tokens changed unexpectedly: (%d, %d)", got.InputTokens, got.OutputTokens)
		}
	})

	t.Run("nonexistent session is silent no-op", func(t *testing.T) {
		if err := s.UpdateTokenCounts(ctx, "ns1", "nonexistent", 10, 20); err != nil {
			t.Errorf("UpdateTokenCounts nonexistent: %v", err)
		}
	})
}

func TestCommitSessionTurn(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "atomic-turn",
		SessionType: "chat",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.CommitSessionTurn(ctx, session, "", 0, nil, 1, 0); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("token-only CommitSessionTurn error = %v, want ErrValidation", err)
	}
	if _, err := s.GetSession(ctx, "ns1", "atomic-turn"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token-only CommitSessionTurn created session: %v", err)
	}

	firstTurn := []store.SessionMessage{
		{Role: roleUser, Content: "hello", Timestamp: now},
		{Role: roleAssistant, Content: "hi", Timestamp: now},
	}
	if err := s.CommitSessionTurn(ctx, session, "", 0, firstTurn, 10, 20); err != nil {
		t.Fatalf("CommitSessionTurn first turn: %v", err)
	}

	got, err := s.GetSession(ctx, "ns1", "atomic-turn")
	if err != nil {
		t.Fatalf("GetSession after first turn: %v", err)
	}
	if got.MessageCount != 2 || got.InputTokens != 10 || got.OutputTokens != 20 {
		t.Fatalf("first turn metadata = messages:%d input:%d output:%d, want 2/10/20", got.MessageCount, got.InputTokens, got.OutputTokens)
	}

	secondTurn := []store.SessionMessage{
		{Role: roleUser, Content: "again", Timestamp: now},
		{Role: roleAssistant, Content: "welcome back", Timestamp: now},
	}
	if err := s.CommitSessionTurn(ctx, session, "", 2, secondTurn, 5, 7); err != nil {
		t.Fatalf("CommitSessionTurn second turn: %v", err)
	}

	got, err = s.GetSession(ctx, "ns1", "atomic-turn")
	if err != nil {
		t.Fatalf("GetSession after second turn: %v", err)
	}
	if got.MessageCount != 4 || got.InputTokens != 15 || got.OutputTokens != 27 {
		t.Fatalf("second turn metadata = messages:%d input:%d output:%d, want 4/15/27", got.MessageCount, got.InputTokens, got.OutputTokens)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("transcript length = %d, want 4", len(got.Messages))
	}

	err = s.CommitSessionTurn(ctx, session, "", 2, []store.SessionMessage{{Role: roleUser, Content: "stale"}}, 100, 200)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale CommitSessionTurn error = %v, want ErrConflict", err)
	}
	got, err = s.GetSession(ctx, "ns1", "atomic-turn")
	if err != nil {
		t.Fatalf("GetSession after conflict: %v", err)
	}
	if got.MessageCount != 4 || got.InputTokens != 15 || got.OutputTokens != 27 || len(got.Messages) != 4 {
		t.Fatalf("conflicting turn changed session: %+v", got)
	}

}

func TestCommitSessionTurnRollsBackOnFailure(t *testing.T) {
	t.Run("existing session", func(t *testing.T) {
		s := setupTestStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)
		session := &store.SessionRecord{Namespace: "ns1", Name: "atomic-fail", SessionType: "chat", CreatedAt: now, UpdatedAt: now}
		if err := s.CommitSessionTurn(ctx, session, "", 0, []store.SessionMessage{{Role: roleAssistant, Content: "prior", Timestamp: now}}, 3, 4); err != nil {
			t.Fatalf("seed CommitSessionTurn: %v", err)
		}
		if _, err := s.db.Exec(`CREATE TRIGGER fail_atomic_turn_update
			BEFORE UPDATE ON sessions
			WHEN NEW.namespace = 'ns1' AND NEW.name = 'atomic-fail'
			BEGIN SELECT RAISE(ABORT, 'injected turn commit failure'); END`); err != nil {
			t.Fatalf("create failure trigger: %v", err)
		}

		err := s.CommitSessionTurn(ctx, session, "", 1, []store.SessionMessage{
			{Role: roleUser, Content: "new user", Timestamp: now},
			{Role: roleAssistant, Content: "new assistant", Timestamp: now},
		}, 10, 20)
		if err == nil {
			t.Fatal("CommitSessionTurn succeeded despite injected metadata failure")
		}

		got, getErr := s.GetSession(ctx, "ns1", "atomic-fail")
		if getErr != nil {
			t.Fatalf("GetSession after rollback: %v", getErr)
		}
		if got.MessageCount != 1 || got.InputTokens != 3 || got.OutputTokens != 4 || len(got.Messages) != 1 {
			t.Fatalf("failed turn was partially committed: %+v", got)
		}
	})

	t.Run("new session", func(t *testing.T) {
		s := setupTestStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)
		if _, err := s.db.Exec(`CREATE TRIGGER fail_new_atomic_turn_update
			BEFORE UPDATE ON sessions
			WHEN NEW.namespace = 'ns1' AND NEW.name = 'new-atomic-fail'
			BEGIN SELECT RAISE(ABORT, 'injected new turn commit failure'); END`); err != nil {
			t.Fatalf("create failure trigger: %v", err)
		}
		session := &store.SessionRecord{Namespace: "ns1", Name: "new-atomic-fail", SessionType: "chat", CreatedAt: now, UpdatedAt: now}
		err := s.CommitSessionTurn(ctx, session, "", 0, []store.SessionMessage{
			{Role: roleUser, Content: "hello", Timestamp: now},
			{Role: roleAssistant, Content: "hi", Timestamp: now},
		}, 10, 20)
		if err == nil {
			t.Fatal("CommitSessionTurn succeeded despite injected metadata failure")
		}
		if _, getErr := s.GetSession(ctx, "ns1", "new-atomic-fail"); !errors.Is(getErr, store.ErrNotFound) {
			t.Fatalf("new session survived failed atomic turn: %v", getErr)
		}
	})
}

func TestAppendMessagesZeroTimestamp(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "zero-ts", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Message with zero timestamp should auto-fill
	msgs := []store.SessionMessage{
		{Role: "user", Content: "auto-timestamp"},
	}
	if err := s.AppendMessages(ctx, "ns1", "zero-ts", msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	got, err := s.LoadTranscript(ctx, "ns1", "zero-ts", 0)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if got[0].Timestamp.IsZero() {
		t.Error("expected non-zero timestamp for auto-filled message")
	}
	if got[0].Content != "auto-timestamp" {
		t.Errorf("content = %q, want %q", got[0].Content, "auto-timestamp")
	}
}

func TestAppendMessagesNilFields(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "nil-fields", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Message with nil Input, nil ToolCalls, empty Name, empty ToolCallID
	msgs := []store.SessionMessage{
		{Role: "user", Content: "bare message", Timestamp: now},
	}
	if err := s.AppendMessages(ctx, "ns1", "nil-fields", msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	got, err := s.LoadTranscript(ctx, "ns1", "nil-fields", 0)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if got[0].Input != nil {
		t.Errorf("expected nil Input, got %v", got[0].Input)
	}
	if got[0].ToolCalls != nil {
		t.Errorf("expected nil ToolCalls, got %v", got[0].ToolCalls)
	}
	if got[0].Name != "" {
		t.Errorf("expected empty Name, got %q", got[0].Name)
	}
	if got[0].ToolCallID != "" {
		t.Errorf("expected empty ToolCallID, got %q", got[0].ToolCallID)
	}
}

func TestNewDBInvalidPath(t *testing.T) {
	// Opening a DB at an invalid/unwritable path should fail
	_, err := NewDB("/nonexistent-dir/sub/sub/test.db")
	if err == nil {
		t.Fatal("expected error for invalid DB path, got nil")
	}
}

func TestAppendMessagesUnmarshalableInput(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "bad-input", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Input with a channel value cannot be JSON marshaled
	msgs := []store.SessionMessage{
		{Role: "user", Content: "bad", Input: map[string]any{"ch": make(chan int)}, Timestamp: now},
	}
	err := s.AppendMessages(ctx, "ns1", "bad-input", msgs)
	if err == nil {
		t.Fatal("expected marshal error for channel in Input, got nil")
	}
}

func TestAppendMessagesUnmarshalableToolCalls(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "bad-tc", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// ToolCalls with a func value cannot be JSON marshaled
	msgs := []store.SessionMessage{
		{Role: "assistant", Content: "", ToolCalls: func() {}, Timestamp: now},
	}
	err := s.AppendMessages(ctx, "ns1", "bad-tc", msgs)
	if err == nil {
		t.Fatal("expected marshal error for func in ToolCalls, got nil")
	}
}

func TestLoadTranscriptCorruptedInput(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "corrupt-input", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Insert a message with invalid JSON in the input column directly via SQL
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_messages (namespace, session_name, role, content, input, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"ns1", "corrupt-input", "user", "test", "{invalid json", now,
	)
	if err != nil {
		t.Fatalf("direct insert: %v", err)
	}

	_, err = s.LoadTranscript(ctx, "ns1", "corrupt-input", 0)
	if err == nil {
		t.Fatal("expected unmarshal error for corrupted input JSON")
	}
}

func TestLoadTranscriptCorruptedToolCalls(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "corrupt-tc", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Insert a message with invalid JSON in the tool_calls column directly via SQL
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_messages (namespace, session_name, role, content, tool_calls, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"ns1", "corrupt-tc", "assistant", "test", "not-valid-json!", now,
	)
	if err != nil {
		t.Fatalf("direct insert: %v", err)
	}

	_, err = s.LoadTranscript(ctx, "ns1", "corrupt-tc", 0)
	if err == nil {
		t.Fatal("expected unmarshal error for corrupted tool_calls JSON")
	}
}

func TestGetSessionWithCorruptedMessages(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "corrupt-sess", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Insert a message with invalid JSON so LoadTranscript fails during GetSession
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_messages (namespace, session_name, role, content, input, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"ns1", "corrupt-sess", "user", "test", "{bad json", now,
	)
	if err != nil {
		t.Fatalf("direct insert: %v", err)
	}

	_, err = s.GetSession(ctx, "ns1", "corrupt-sess")
	if err == nil {
		t.Fatal("expected error from GetSession with corrupted messages")
	}
}

func TestClosedDBErrors(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	s := NewStore(db, ":memory:")
	db.Close() //nolint:errcheck

	ctx := context.Background()

	// All operations on a closed DB should return errors
	if _, err := s.GetResult(ctx, "ns", "t"); err == nil {
		t.Error("expected error from GetResult on closed DB")
	}
	if err := s.SaveResult(ctx, "ns", "t", []byte("d")); err == nil {
		t.Error("expected error from SaveResult on closed DB")
	}
	if err := s.DeleteResult(ctx, "ns", "t"); err == nil {
		t.Error("expected error from DeleteResult on closed DB")
	}
	if _, err := s.GetSession(ctx, "ns", "s"); err == nil {
		t.Error("expected error from GetSession on closed DB")
	}
	if _, err := s.ListSessions(ctx, "ns"); err == nil {
		t.Error("expected error from ListSessions on closed DB")
	}
	if err := s.AcquireLock(ctx, "ns", "s", "t"); err == nil {
		t.Error("expected error from AcquireLock on closed DB")
	}
	if err := s.AcquireChatTurn(ctx, &store.SessionRecord{Namespace: "ns", Name: "s", SessionType: "chat"}, "chat-turn-test", time.Now().Add(time.Hour)); err == nil {
		t.Error("expected error from AcquireChatTurn on closed DB")
	}
	if err := s.ReleaseChatTurn(ctx, "ns", "s", "chat-turn-test"); err == nil {
		t.Error("expected error from ReleaseChatTurn on closed DB")
	}
	if _, err := s.IsLocked(ctx, "ns", "s", "t"); err == nil {
		t.Error("expected error from IsLocked on closed DB")
	}
	if err := s.AppendMessages(ctx, "ns", "s", []store.SessionMessage{{Role: "user"}}); err == nil {
		t.Error("expected error from AppendMessages on closed DB")
	}
	if _, err := s.LoadTranscript(ctx, "ns", "s", 0); err == nil {
		t.Error("expected error from LoadTranscript on closed DB")
	}
	if _, err := s.GetPlan(ctx, "ns", "t"); err == nil {
		t.Error("expected error from GetPlan on closed DB")
	}
	if err := s.SavePlan(ctx, "ns", "t", &store.PlanState{}); err == nil {
		t.Error("expected error from SavePlan on closed DB")
	}
	if err := s.HealthCheck(ctx); err == nil {
		t.Error("expected error from HealthCheck on closed DB")
	}
	if _, err := s.GetMessages(ctx, "ns", "t", "p", false); err == nil {
		t.Error("expected error from GetMessages on closed DB")
	}
	if err := s.SendMessage(ctx, &store.Message{Namespace: "ns", FromTask: "a", ToTask: "b", ParentTask: "p", Content: "c"}); err == nil {
		t.Error("expected error from SendMessage on closed DB")
	}
}

func TestUpdateDBSizeMetricEdgeCases(t *testing.T) {
	t.Run("empty path is no-op", func(t *testing.T) {
		db, err := NewDB(":memory:")
		if err != nil {
			t.Fatalf("NewDB: %v", err)
		}
		defer db.Close() //nolint:errcheck
		s := NewStore(db, "")
		// Should not panic
		s.updateDBSizeMetric()
	})

	t.Run("nonexistent file path is no-op", func(t *testing.T) {
		db, err := NewDB(":memory:")
		if err != nil {
			t.Fatalf("NewDB: %v", err)
		}
		defer db.Close() //nolint:errcheck
		s := NewStore(db, "/tmp/nonexistent-orka-test-file.db")
		// Should not panic even if file doesn't exist
		s.updateDBSizeMetric()
	})
}

func TestAcquireLockNonexistentSession(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// AcquireLock on a session that doesn't exist should fail (rows affected = 0)
	err := s.AcquireLock(ctx, "ns1", "no-such-session", "task-x")
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
}

func TestDeletePlanNonexistent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Deleting a plan that doesn't exist should be a no-op
	if err := s.DeletePlan(ctx, "ns1", "never-existed"); err != nil {
		t.Errorf("DeletePlan nonexistent: %v", err)
	}
}

func TestDeleteSessionNonexistent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Deleting a session that doesn't exist should be a no-op
	if err := s.DeleteSession(ctx, "ns1", "never-existed"); err != nil {
		t.Errorf("DeleteSession nonexistent: %v", err)
	}
}

func TestSessionCancelledField(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "cancelled-sess",
		SessionType: "task",
		Cancelled:   true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession(ctx, "ns1", "cancelled-sess")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !got.Cancelled {
		t.Error("expected Cancelled=true")
	}
}

func TestListSessionsMetadata(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// Create session with active task
	session := &store.SessionRecord{
		Namespace:   "ns1",
		Name:        "meta-sess",
		SessionType: "chat",
		ActiveTask:  "",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Acquire lock so active_task is set
	if err := s.AcquireLock(ctx, "ns1", "meta-sess", "active-task"); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	sessions, err := s.ListSessions(ctx, "ns1")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].ActiveTask != "active-task" {
		t.Errorf("ActiveTask = %q, want %q", sessions[0].ActiveTask, "active-task")
	}
	if sessions[0].SessionType != "chat" {
		t.Errorf("SessionType = %q, want %q", sessions[0].SessionType, "chat")
	}
}

func TestLoadTranscriptEmptySession(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Loading transcript for nonexistent session returns empty slice, not error
	msgs, err := s.LoadTranscript(ctx, "ns1", "nonexistent", 0)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("got %d messages, want 0", len(msgs))
	}
}

func TestNilIfEmpty(t *testing.T) {
	if got := nilIfEmpty(""); got != nil {
		t.Errorf("nilIfEmpty(\"\") = %v, want nil", got)
	}
	if got := nilIfEmpty(contentHello); got == nil || *got != contentHello {
		t.Errorf("nilIfEmpty(\"hello\") = %v, want pointer to \"hello\"", got)
	}
}

func TestContextCancellation(t *testing.T) {
	s := setupTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Operations on cancelled context should return errors
	if err := s.SaveResult(ctx, "ns1", "task1", []byte("data")); err == nil {
		t.Error("expected error on cancelled context for SaveResult")
	}

	if _, err := s.GetResult(ctx, "ns1", "task1"); err == nil {
		t.Error("expected error on cancelled context for GetResult")
	}

	if err := s.SavePlan(ctx, "ns1", "task1", &store.PlanState{}); err == nil {
		t.Error("expected error on cancelled context for SavePlan")
	}

	if _, err := s.GetPlan(ctx, "ns1", "task1"); err == nil {
		t.Error("expected error on cancelled context for GetPlan")
	}

	now := time.Now()
	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns1", Name: "s", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err == nil {
		t.Error("expected error on cancelled context for CreateSession")
	}

	if _, err := s.GetSession(ctx, "ns1", "s"); err == nil {
		t.Error("expected error on cancelled context for GetSession")
	}

	if _, err := s.ListSessions(ctx, "ns1"); err == nil {
		t.Error("expected error on cancelled context for ListSessions")
	}

	if err := s.AppendMessages(ctx, "ns1", "s", []store.SessionMessage{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("expected error on cancelled context for AppendMessages")
	}

	if _, err := s.LoadTranscript(ctx, "ns1", "s", 0); err == nil {
		t.Error("expected error on cancelled context for LoadTranscript")
	}

	if err := s.UpdateTokenCounts(ctx, "ns1", "s", 10, 20); err == nil {
		t.Error("expected error on cancelled context for UpdateTokenCounts")
	}

	if _, err := s.IsLocked(ctx, "ns1", "s", "t"); err == nil {
		t.Error("expected error on cancelled context for IsLocked")
	}

	if err := s.AcquireLock(ctx, "ns1", "s", "t"); err == nil {
		t.Error("expected error on cancelled context for AcquireLock")
	}
}

func TestSpecialCharacters(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	t.Run("special chars in result keys", func(t *testing.T) {
		keys := []string{
			"task/with/slashes",
			"task with spaces",
			"task'with\"quotes",
			"task;with;semicolons",
			"émojis-🎉-✓",
		}
		for _, key := range keys {
			data := []byte("data-for-" + key)
			if err := s.SaveResult(ctx, "ns1", key, data); err != nil {
				t.Fatalf("SaveResult(%q): %v", key, err)
			}
			got, err := s.GetResult(ctx, "ns1", key)
			if err != nil {
				t.Fatalf("GetResult(%q): %v", key, err)
			}
			if string(got) != string(data) {
				t.Errorf("key %q: got %q, want %q", key, got, data)
			}
		}
	})

	t.Run("special chars in plan", func(t *testing.T) {
		plan := &store.PlanState{
			Summary:      "Plan with 'quotes' and \"double quotes\" and\nnewlines",
			PlanDocument: "# Plan\n\n- Step 1: `code block`\n- Step 2: <html>tags</html>\n- Step 3: ${variable}",
		}
		if err := s.SavePlan(ctx, "ns1", "special-plan", plan); err != nil {
			t.Fatalf("SavePlan: %v", err)
		}
		got, err := s.GetPlan(ctx, "ns1", "special-plan")
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if got.Summary != plan.Summary {
			t.Errorf("Summary mismatch")
		}
		if got.PlanDocument != plan.PlanDocument {
			t.Errorf("PlanDocument mismatch")
		}
	})
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
