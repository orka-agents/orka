package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestTranscriptSessionCleanupCompletionIsIdempotentUntilNameReuse(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	createControlTranscriptSession(t, s, "tenant-a", "chat-delete", now)
	intent := store.SessionCleanupIntent{
		Namespace: "tenant-a", SessionName: "chat-delete",
		OperationID: "delete-chat-session", OperationDigest: controlTestDigest("delete-chat-session"), PreparedAt: now,
	}
	prepared, err := s.PrepareSessionCleanup(ctx, intent)
	if err != nil {
		t.Fatalf("PrepareSessionCleanup(): %v", err)
	}
	listed, err := s.ListSessionCleanupIntents(ctx)
	if err != nil {
		t.Fatalf("ListSessionCleanupIntents(): %v", err)
	}
	if len(listed) != 1 || listed[0].OperationDigest != prepared.OperationDigest {
		t.Fatalf("pending cleanup intents = %#v", listed)
	}
	request := store.CompleteSessionCleanupRequest{
		Namespace: intent.Namespace, SessionName: intent.SessionName,
		OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}
	if err := s.CompleteSessionCleanup(ctx, request); err != nil {
		t.Fatalf("CompleteSessionCleanup(): %v", err)
	}
	if err := s.CompleteSessionCleanup(ctx, request); err != nil {
		t.Fatalf("CompleteSessionCleanup(idempotent retry): %v", err)
	}
	if _, err := s.GetSession(ctx, intent.Namespace, intent.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSession() error = %v, want ErrNotFound", err)
	}
	completion, err := s.GetSessionCleanupCompletion(ctx, intent.Namespace, intent.SessionName)
	if err != nil {
		t.Fatalf("GetSessionCleanupCompletion(): %v", err)
	}
	if completion.OperationID != intent.OperationID || completion.OperationDigest != intent.OperationDigest || completion.CompletedAt.IsZero() {
		t.Fatalf("completion receipt = %#v", completion)
	}
	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: intent.Namespace, Name: intent.SessionName, SessionType: "chat", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession(reused transcript-only name): %v", err)
	}
	if _, err := s.GetSessionCleanupCompletion(ctx, intent.Namespace, intent.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSessionCleanupCompletion(after name reuse) error = %v, want ErrNotFound", err)
	}
}

func TestSessionCleanupIntentBlocksTranscriptAppend(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	createControlTranscriptSession(t, s, "tenant-a", "chat-pending-delete", now)
	if _, err := s.PrepareSessionCleanup(ctx, store.SessionCleanupIntent{
		Namespace: "tenant-a", SessionName: "chat-pending-delete",
		OperationID: "delete-chat-pending", OperationDigest: controlTestDigest("delete-chat-pending"), PreparedAt: now,
	}); err != nil {
		t.Fatalf("PrepareSessionCleanup(): %v", err)
	}
	if err := s.AppendMessages(ctx, "tenant-a", "chat-pending-delete", []store.SessionMessage{{
		Role: "assistant", Content: "late response", Timestamp: now,
	}}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("AppendMessages() error = %v, want ErrConflict", err)
	}
	if err := s.DeleteSession(ctx, "tenant-a", "chat-pending-delete"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteSession(pending cleanup) error = %v, want ErrConflict", err)
	}
	if _, err := s.GetSessionCleanupIntent(ctx, "tenant-a", "chat-pending-delete"); err != nil {
		t.Fatalf("cleanup intent was erased by direct deletion attempt: %v", err)
	}
}

func TestExpiredTransientSessionLockDoesNotWedgeDeletion(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	createControlTranscriptSession(t, s, "tenant-a", "chat-expired-lock", now)
	if err := s.AcquireLockUntil(ctx, "tenant-a", "chat-expired-lock", "chat-request-old", "chat-request-old", now.Add(time.Minute)); err != nil {
		t.Fatalf("AcquireLockUntil(): %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task_expires_at = ? WHERE namespace = ? AND name = ?`,
		now.Add(-time.Minute), "tenant-a", "chat-expired-lock",
	); err != nil {
		t.Fatalf("expire transient lock: %v", err)
	}
	if err := s.DeleteSession(ctx, "tenant-a", "chat-expired-lock"); err != nil {
		t.Fatalf("DeleteSession(after transient lock expiry): %v", err)
	}
}

func TestExpiredTransientSessionOwnerCannotWriteAfterTakeover(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	createControlTranscriptSession(t, s, "tenant-a", "chat-lock-takeover", now)
	if err := s.AcquireLockUntil(ctx, "tenant-a", "chat-lock-takeover", "owner-a", "owner-a", now.Add(time.Minute)); err != nil {
		t.Fatalf("AcquireLockUntil(owner A): %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task_expires_at = ? WHERE namespace = ? AND name = ?`,
		now.Add(-time.Minute), "tenant-a", "chat-lock-takeover",
	); err != nil {
		t.Fatalf("expire owner A: %v", err)
	}
	if err := s.AcquireLockUntil(ctx, "tenant-a", "chat-lock-takeover", "owner-b", "owner-b", now.Add(time.Minute)); err != nil {
		t.Fatalf("AcquireLockUntil(owner B): %v", err)
	}
	late := []store.SessionMessage{{Role: "assistant", Content: "stale", Timestamp: now}}
	if err := s.AppendMessagesWithLock(ctx, "tenant-a", "chat-lock-takeover", "owner-a", "owner-a", late); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("AppendMessagesWithLock(stale owner) error = %v, want ErrConflict", err)
	}
	if err := s.UpdateTokenCountsWithLock(ctx, "tenant-a", "chat-lock-takeover", "owner-a", "owner-a", 1, 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("UpdateTokenCountsWithLock(stale owner) error = %v, want ErrConflict", err)
	}
	if err := s.AppendMessagesWithLock(ctx, "tenant-a", "chat-lock-takeover", "owner-b", "owner-b", []store.SessionMessage{{
		Role: "assistant", Content: "current", Timestamp: now,
	}}); err != nil {
		t.Fatalf("AppendMessagesWithLock(current owner): %v", err)
	}
}
