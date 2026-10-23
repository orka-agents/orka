package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestChatTurnRejectsHistoricalSchemaWithoutChangingSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-sessions.db")
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacyDB.Exec(`CREATE TABLE sessions (
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		session_type TEXT NOT NULL DEFAULT 'task',
		owner_type TEXT NOT NULL DEFAULT '',
		owner_ref TEXT NOT NULL DEFAULT '',
		active_task TEXT NOT NULL DEFAULT '',
		active_task_uid TEXT NOT NULL DEFAULT '',
		message_count INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cancelled BOOLEAN NOT NULL DEFAULT FALSE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (namespace, name)
	)`)
	if err != nil {
		_ = legacyDB.Close()
		t.Fatal(err)
	}
	if _, err := legacyDB.Exec(`INSERT INTO sessions
		(namespace, name, session_type, message_count, input_tokens, output_tokens)
		VALUES ('default', 'legacy-chat', 'chat', 3, 11, 7)`); err != nil {
		_ = legacyDB.Close()
		t.Fatal(err)
	}
	before := storedSchemaSQL(t, legacyDB)
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := NewDB(dbPath)
	if db != nil {
		_ = db.Close()
		t.Fatal("NewDB() accepted a historical chat schema")
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported SQLite schema") {
		t.Fatalf("NewDB() error = %v, want unsupported SQLite schema", err)
	}

	legacyDB, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacyDB.Close() })
	if after := storedSchemaSQL(t, legacyDB); after != before {
		t.Fatal("NewDB() rewrote the rejected historical schema")
	}
	var sessionType string
	var messages, inputTokens, outputTokens int
	if err := legacyDB.QueryRow(`SELECT session_type, message_count, input_tokens, output_tokens
		FROM sessions WHERE namespace = 'default' AND name = 'legacy-chat'`).
		Scan(&sessionType, &messages, &inputTokens, &outputTokens); err != nil {
		t.Fatal(err)
	}
	if sessionType != "chat" || messages != 3 || inputTokens != 11 || outputTokens != 7 {
		t.Fatalf("rejected session changed: type=%q messages=%d input=%d output=%d", sessionType, messages, inputTokens, outputTokens)
	}
}

func TestAcquireChatTurnFencesConcurrentAndRecoveredTurns(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	session := &store.SessionRecord{
		Namespace: "default", Name: "chat-session", SessionType: "chat", CreatedAt: now, UpdatedAt: now,
	}
	created, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-a", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("AcquireChatTurn() did not report a newly created session")
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-b", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("concurrent AcquireChatTurn() error = %v, want ErrConflict", err)
	}

	locked, err := s.IsLocked(ctx, "default", "chat-session", "task-a", "task-uid", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("IsLocked() did not report the active chat turn")
	}
	if err := s.AcquireLock(ctx, "default", "chat-session", "task-a", "task-uid", time.Now().UTC()); err == nil {
		t.Fatal("AcquireLock() acquired the same session during an active chat turn")
	}
	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "default", Name: "child-session", SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLock(ctx, "default", "child-session", "child-task", "child-task-uid", time.Now().UTC()); err != nil {
		t.Fatalf("AcquireLock() rejected a Task using a different session: %v", err)
	}

	if _, err := s.db.Exec(`UPDATE sessions SET chat_turn_expires_at = ?
		WHERE namespace = ? AND name = ?`, now.Add(-time.Second), "default", "chat-session"); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLock(ctx, "default", "chat-session", "task-a", "task-uid", time.Now().UTC()); err != nil {
		t.Fatalf("AcquireLock() did not reclaim an expired chat turn: %v", err)
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-b", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("AcquireChatTurn() stole an active Task lock: %v", err)
	}
	if err := s.ReleaseLock(ctx, "default", "chat-session", "task-a", "task-uid"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-b", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatalf("AcquireChatTurn() did not recover expired reservation: %v", err)
	}
	if err := s.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
		Session: session, TurnID: "turn-a", ExpectedMessageCount: 0,
		Messages: []store.SessionMessage{{
			Role: "user", Content: "stale",
		}},
		InputTokens: 1, OutputTokens: 1, Now: time.Now().UTC(),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("recovered owner accepted stale commit: %v", err)
	}

	if err := s.ReleaseChatTurn(ctx, "default", "chat-session", "turn-b", false); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLock(ctx, "default", "chat-session", "task-b", "task-b-uid", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-c", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("AcquireChatTurn() stole active Task lock: %v", err)
	}

	gateway := &store.SessionRecord{
		Namespace: "default", Name: "gateway-session", SessionType: store.SessionTypeGateway,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateSession(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: gateway, TurnID: "turn-gateway", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); !errors.Is(err, store.ErrGatewayOwnedSession) {
		t.Fatalf("AcquireChatTurn() gateway error = %v, want ErrGatewayOwnedSession", err)
	}
}

func TestCommitSessionTurnRejectsActiveTaskFence(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	session := &store.SessionRecord{
		Namespace: "default", Name: "task-fenced-chat", SessionType: store.SessionTypeChat,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-fenced", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET active_task = ?, active_task_uid = ?
		WHERE namespace = ? AND name = ?`, "interleaved-task", "task-uid", "default", "task-fenced-chat"); err != nil {
		t.Fatal(err)
	}

	err := s.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
		Session: session, TurnID: "turn-fenced", ExpectedMessageCount: 0,
		Messages: []store.SessionMessage{
			{Role: "user", Content: "must not commit"},
			{Role: "assistant", Content: "also fenced"},
		},
		InputTokens: 8, OutputTokens: 5, Now: time.Now().UTC(),
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CommitSessionTurn() active Task error = %v, want ErrConflict", err)
	}
	got, err := s.GetSession(ctx, "default", "task-fenced-chat")
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageCount != 0 || got.InputTokens != 0 || got.OutputTokens != 0 || len(got.Messages) != 0 {
		t.Fatalf("active Task fence allowed a partial chat commit: %+v", got)
	}
}

func TestReleaseChatTurnDeletesOnlyCreatedEmptySession(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("new failed turn is removed", func(t *testing.T) {
		session := &store.SessionRecord{
			Namespace: "default", Name: "new-empty-chat", SessionType: store.SessionTypeChat,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-new-empty", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if !created {
			t.Fatal("AcquireChatTurn() did not report a new Session")
		}
		if err := s.ReleaseChatTurn(ctx, "default", "new-empty-chat", "turn-new-empty", created); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetSession(ctx, "default", "new-empty-chat"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetSession() after failed new turn error = %v, want ErrNotFound", err)
		}
	})

	t.Run("preexisting empty session is retained", func(t *testing.T) {
		session := &store.SessionRecord{
			Namespace: "default", Name: "existing-empty-chat", SessionType: store.SessionTypeChat,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		created, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-existing-empty", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Fatal("AcquireChatTurn() reported a preexisting Session as new")
		}
		if err := s.ReleaseChatTurn(ctx, "default", "existing-empty-chat", "turn-existing-empty", created); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetSession(ctx, "default", "existing-empty-chat"); err != nil {
			t.Fatalf("GetSession() after releasing preexisting Session error = %v", err)
		}
	})

	t.Run("committed new session is retained", func(t *testing.T) {
		session := &store.SessionRecord{
			Namespace: "default", Name: "new-committed-chat", SessionType: store.SessionTypeChat,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-new-committed", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
			Session: session, TurnID: "turn-new-committed", ExpectedMessageCount: 0,
			Messages: []store.SessionMessage{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"},
			},
			InputTokens: 4, OutputTokens: 2, Now: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseChatTurn(ctx, "default", "new-committed-chat", "turn-new-committed", created); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSession(ctx, "default", "new-committed-chat")
		if err != nil {
			t.Fatal(err)
		}
		if got.MessageCount != 2 {
			t.Fatalf("committed Session message count = %d, want 2", got.MessageCount)
		}
	})
}

func TestCommitSessionTurnPreservesStableMessageOrderingAndUsage(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	session := &store.SessionRecord{
		Namespace: "default", Name: "atomic-chat", SessionType: "chat", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-one", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
		Session: session, TurnID: "turn-one", ExpectedMessageCount: 0,
		Messages: []store.SessionMessage{
			{Role: "user", Content: "hello", Timestamp: now},
			{Role: "assistant", Content: "hi", Timestamp: now},
		},
		InputTokens: 13, OutputTokens: 8, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseChatTurn(ctx, "default", "atomic-chat", "turn-one", false); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSession(ctx, "default", "atomic-chat")
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageCount != 2 || got.InputTokens != 13 || got.OutputTokens != 8 || len(got.Messages) != 2 {
		t.Fatalf("first atomic turn = %+v", got)
	}
	if got.Messages[0].ID == "" || got.Messages[1].ID == "" || got.Messages[0].ID == got.Messages[1].ID {
		t.Fatalf("stable message IDs = (%q, %q)", got.Messages[0].ID, got.Messages[1].ID)
	}
	if got.Messages[0].Order != 2 || got.Messages[1].Order != 4 {
		t.Fatalf("logical message orders = (%d, %d), want (2, 4)", got.Messages[0].Order, got.Messages[1].Order)
	}

	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-two", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
		Session: session, TurnID: "turn-two", ExpectedMessageCount: 2,
		Messages: []store.SessionMessage{{
			Role: "user", Content: "again", Timestamp: now.Add(time.Second),
		}},
		InputTokens: 5, OutputTokens: 3, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSession(ctx, "default", "atomic-chat")
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageCount != 3 || got.InputTokens != 18 || got.OutputTokens != 11 || got.Messages[2].Order != 6 {
		t.Fatalf("second atomic turn = %+v", got)
	}
}

func TestCommitSessionTurnRollsBackMessagesAndUsageTogether(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	session := &store.SessionRecord{
		Namespace: "default", Name: "rollback-chat", SessionType: "chat", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-rollback", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	err := s.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
		Session: session, TurnID: "turn-rollback", ExpectedMessageCount: 0,
		Messages: []store.SessionMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "invalid", Input: map[string]any{"bad": make(chan int)}},
		},
		InputTokens: 21, OutputTokens: 34, Now: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("CommitSessionTurn() succeeded with an unencodable message")
	}
	got, getErr := s.GetSession(ctx, "default", "rollback-chat")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.MessageCount != 0 || got.InputTokens != 0 || got.OutputTokens != 0 || len(got.Messages) != 0 {
		t.Fatalf("failed turn was partially committed: %+v", got)
	}
}

func TestCommitSessionTurnRejectsInterleavedTranscriptRevision(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	session := &store.SessionRecord{
		Namespace: "default", Name: "interleaved-chat", SessionType: "chat", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{Session: session, TurnID: "turn-interleaved", Now: time.Now().UTC(), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessages(ctx, "default", "interleaved-chat", []store.SessionMessage{{
		ID: "task:result", Role: "assistant", Content: "concurrent task result", Timestamp: now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
		Session: session, TurnID: "turn-interleaved", ExpectedMessageCount: 0,
		Messages: []store.SessionMessage{{
			Role: "user", Content: "chat message", Timestamp: now,
		}},
		InputTokens: 9, OutputTokens: 4, Now: time.Now().UTC(),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CommitSessionTurn() interleaved error = %v, want ErrConflict", err)
	}
	got, err := s.GetSession(ctx, "default", "interleaved-chat")
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageCount != 1 || len(got.Messages) != 1 || got.Messages[0].ID != "task:result" {
		t.Fatalf("interleaved transcript changed unexpectedly: %+v", got)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("interleaved commit updated tokens: (%d, %d)", got.InputTokens, got.OutputTokens)
	}
}
