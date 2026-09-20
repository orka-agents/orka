package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

func TestSessionSoulPinExplicitAbsence(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	now := time.Now().UTC()
	if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "team", Name: "s", SessionType: "task", ActiveTask: "first", ActiveTaskUID: "first-uid", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "first", "first-uid", ""); err != nil {
			t.Fatal(err)
		}
	}
	state, err := s.ReadSessionSoul(ctx, "team", "s", "first", "first-uid")
	if err != nil || !state.Established || state.Digest != "" || state.MessageCount != 0 {
		t.Fatalf("explicit absence was not established: %+v, %v", state, err)
	}
	if transcript, err := s.LoadTranscript(ctx, "team", "s", 50); err != nil || len(transcript) != 0 {
		t.Fatal("absence pin was exposed as transcript content")
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "first", "", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("matching absence bypassed exact Task UID ownership: %v", err)
	}
	if err := s.ReleaseLock(ctx, "team", "s", "first", "first-uid"); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLock(ctx, "team", "s", "second", "second-uid"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "first", "first-uid", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("old owner could pin the no-soul revision: %v", err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "second", "second-uid", agentcontext.Digest("new persona")); !errors.Is(err, store.ErrSessionConfigurationMismatch) {
		t.Fatalf("later Task acquired a first soul: %v", err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "second", "second-uid", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "second", "second-uid", " "); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("nonempty malformed digest was accepted: %v", err)
	}
	var anchors int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE namespace = 'team' AND session_name = 's' AND source_type = ?`, store.SessionSoulAnchorSource).Scan(&anchors); err != nil || anchors != 1 {
		t.Fatal("idempotent no-soul pin created duplicate anchors")
	}
}
