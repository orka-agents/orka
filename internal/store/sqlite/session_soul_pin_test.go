package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

func TestSessionSoulPinIsFencedHiddenAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	now := time.Now().UTC()
	if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "team", Name: "s", SessionType: "task", ActiveTask: "first", ActiveTaskUID: "first-uid", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	digest := agentcontext.Digest("frozen revision")
	for range 2 {
		if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "first", "first-uid", digest); err != nil {
			t.Fatal(err)
		}
	}
	state, err := s.ReadSessionSoul(ctx, "team", "s", "first", "first-uid")
	if err != nil || !state.Established || state.Digest != digest || state.MessageCount != 0 || state.FirstMessageID != "" {
		t.Fatalf("pin did not establish hidden identity: %+v, %v", state, err)
	}
	transcript, err := s.LoadTranscript(ctx, "team", "s", 50)
	if err != nil || len(transcript) != 0 {
		t.Fatal("identity pin leaked into the transcript")
	}
	var anchors int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE namespace = 'team' AND session_name = 's' AND source_type = ?`, store.SessionSoulAnchorSource).Scan(&anchors); err != nil || anchors != 1 {
		t.Fatalf("idempotent pin count = %d, %v", anchors, err)
	}
	if err := s.ReleaseLock(ctx, "team", "s", "first", "first-uid"); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLock(ctx, "team", "s", "second", "second-uid"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "first", "first-uid", digest); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("old owner pin error = %v", err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "second", "second-uid", agentcontext.Digest("different revision")); !errors.Is(err, store.ErrSessionConfigurationMismatch) {
		t.Fatalf("different revision pin error = %v", err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "second", "second-uid", digest); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseLock(ctx, "team", "s", "second", "second-uid"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "team", "s"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE namespace = 'team' AND session_name = 's'`).Scan(&anchors); err != nil || anchors != 0 {
		t.Fatal("Session deletion retained its identity pin")
	}
}

func TestSessionSoulPinRejectsLegacyAndUnownedSessions(t *testing.T) {
	for _, scenario := range []string{"legacy transcript", "wrong name", "wrong UID", "empty UID", "expired lock", "cleanup pending", "gateway", "missing Session"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := setupTestStore(t)
			now := time.Now().UTC()
			if scenario != "missing Session" {
				if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "team", Name: "s", SessionType: "task", ActiveTask: "owner", ActiveTaskUID: "uid", CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			owner, uid := "owner", "uid"
			switch scenario {
			case "legacy transcript":
				if err := s.AppendMessagesWithLock(ctx, "team", "s", owner, uid, []store.SessionMessage{{Role: "user", Content: "legacy turn"}}); err != nil {
					t.Fatal(err)
				}
			case "wrong name":
				owner = "other"
			case "wrong UID":
				uid = "other-uid"
			case "empty UID":
				uid = ""
			case "expired lock":
				if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET active_task_expires_at = ? WHERE namespace = 'team' AND name = 's'`, now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			case "cleanup pending":
				if _, err := s.db.ExecContext(ctx, `INSERT INTO session_cleanup_intents
					(namespace, session_name, operation_id, operation_digest, plan, created_at)
					VALUES ('team', 's', 'cleanup', ?, '{}', ?)`, agentcontext.Digest("cleanup"), now); err != nil {
					t.Fatal(err)
				}
			case "gateway":
				if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET session_type = ? WHERE namespace = 'team' AND name = 's'`, store.SessionTypeGateway); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", owner, uid, agentcontext.Digest("persona")); err == nil {
				t.Fatal("invalid pin was accepted")
			}
			var anchors int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE source_type = ?`, store.SessionSoulAnchorSource).Scan(&anchors); err != nil || anchors != 0 {
				t.Fatal("rejected pin wrote control metadata")
			}
		})
	}
}

func TestSessionSoulPinHonorsExistingCanonicalRevision(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	now := time.Now().UTC()
	if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "team", Name: "s", SessionType: "task", ActiveTask: "owner", ActiveTaskUID: "uid", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	digest := agentcontext.Digest("existing revision")
	if err := s.AppendMessagesWithLock(ctx, "team", "s", "owner", "uid", []store.SessionMessage{{
		ID: "existing-turn", Role: "user", Content: "existing prompt", Metadata: map[string]string{store.SessionSoulDigestMetadata: digest},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "owner", "uid", digest); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSessionSoulWithLock(ctx, "team", "s", "owner", "uid", agentcontext.Digest("replacement")); !errors.Is(err, store.ErrSessionConfigurationMismatch) {
		t.Fatalf("canonical revision changed: %v", err)
	}
	var anchors int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE source_type = ?`, store.SessionSoulAnchorSource).Scan(&anchors); err != nil || anchors != 0 {
		t.Fatal("a new anchor masked existing canonical history")
	}
}
