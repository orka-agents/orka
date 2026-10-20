package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestReleaseLockWithTokenUsageCountsOnce(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	now := time.Now().UTC()
	create := func(name string) {
		t.Helper()
		if err := s.CreateSession(ctx, &store.SessionRecord{
			Namespace: "default", Name: name, SessionType: "task",
			ActiveTask: "owner", ActiveTaskUID: "owner-uid", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", name, err)
		}
	}
	tokens := func(name string) (int, int, string) {
		t.Helper()
		session, err := s.GetSession(ctx, "default", name)
		if err != nil {
			t.Fatalf("GetSession(%s): %v", name, err)
		}
		return session.InputTokens, session.OutputTokens, session.ActiveTask
	}

	create("counted")
	if err := s.ReleaseLockWithTokenUsage(ctx, "default", "counted", "owner", "owner-uid", 150, 25); err != nil {
		t.Fatalf("ReleaseLockWithTokenUsage: %v", err)
	}
	// A replay after the release matches no owner and adds nothing.
	if err := s.ReleaseLockWithTokenUsage(ctx, "default", "counted", "owner", "owner-uid", 150, 25); err != nil {
		t.Fatalf("replayed ReleaseLockWithTokenUsage: %v", err)
	}
	if in, out, owner := tokens("counted"); in != 150 || out != 25 || owner != "" {
		t.Fatalf("tokens/owner = %d/%d/%q, want 150/25 and released", in, out, owner)
	}

	// Another owner's release changes nothing.
	create("other-owner")
	if err := s.ReleaseLockWithTokenUsage(ctx, "default", "other-owner", "intruder", "intruder-uid", 10, 10); err != nil {
		t.Fatalf("ReleaseLockWithTokenUsage(intruder): %v", err)
	}
	if in, out, owner := tokens("other-owner"); in != 0 || out != 0 || owner != "owner" {
		t.Fatalf("tokens/owner = %d/%d/%q, want 0/0 and still held by owner", in, out, owner)
	}

	// An expired lock is released without counting its usage.
	create("expired")
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET active_task_expires_at = ? WHERE namespace = ? AND name = ?`,
		now.Add(-time.Minute), "default", "expired"); err != nil {
		t.Fatalf("expire lock: %v", err)
	}
	if err := s.ReleaseLockWithTokenUsage(ctx, "default", "expired", "owner", "owner-uid", 10, 10); err != nil {
		t.Fatalf("ReleaseLockWithTokenUsage(expired): %v", err)
	}
	if in, out, owner := tokens("expired"); in != 0 || out != 0 || owner != "" {
		t.Fatalf("tokens/owner = %d/%d/%q, want 0/0 and released", in, out, owner)
	}
}
