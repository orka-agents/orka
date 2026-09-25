package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestReleaseChatTurnPreservesPendingCleanup(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	session := &store.SessionRecord{
		Namespace: "default", Name: "pending-cleanup-chat", SessionType: store.SessionTypeChat,
	}
	created, err := s.AcquireChatTurn(ctx, session, "old-turn", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("chat session was not created")
	}
	if _, err := s.db.Exec(`UPDATE sessions SET chat_turn_expires_at = ?
		WHERE namespace = ? AND name = ?`, time.Now().UTC().Add(-time.Minute), session.Namespace, session.Name); err != nil {
		t.Fatal(err)
	}
	intent, err := s.PrepareSessionCleanup(ctx, store.SessionCleanupIntent{
		Namespace: session.Namespace, SessionName: session.Name,
		OperationID: "cleanup-chat", OperationDigest: controlTestDigest("cleanup-chat"), PreparedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseChatTurn(ctx, session.Namespace, session.Name, "old-turn", created); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, session.Namespace, session.Name); err != nil {
		t.Fatalf("late release removed session owned by cleanup: %v", err)
	}
	if err := s.CompleteSessionCleanup(ctx, store.CompleteSessionCleanupRequest{
		Namespace: session.Namespace, SessionName: session.Name,
		OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}); err != nil {
		t.Fatalf("CompleteSessionCleanup(): %v", err)
	}
	if _, err := s.GetSessionCleanupIntent(ctx, session.Namespace, session.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cleanup intent remains: %v", err)
	}
	if _, err := s.GetSession(ctx, session.Namespace, session.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cleaned session remains: %v", err)
	}
	if _, err := s.AcquireChatTurn(ctx, session, "new-turn", time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatalf("completed transcript cleanup blocked name reuse: %v", err)
	}
}

func TestDeleteSessionChatTurnLease(t *testing.T) {
	for _, tt := range []struct {
		name      string
		expired   bool
		wantError error
	}{
		{name: "live lease protects session", wantError: store.ErrConflict},
		{name: "expired lease permits deletion", expired: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			session := &store.SessionRecord{
				Namespace: "default", Name: "abandoned-chat", SessionType: store.SessionTypeChat,
			}
			if _, err := s.AcquireChatTurn(ctx, session, "abandoned-turn", time.Now().UTC().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.CommitSessionTurn(ctx, session, "abandoned-turn", 0, []store.SessionMessage{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"},
			}, 3, 2); err != nil {
				t.Fatal(err)
			}
			if tt.expired {
				// Simulate a process exiting before release, without a timing-dependent sleep.
				if _, err := s.db.Exec(`UPDATE sessions SET chat_turn_expires_at = ?
					WHERE namespace = ? AND name = ?`, time.Now().UTC().Add(-time.Minute), session.Namespace, session.Name); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DeleteSession(ctx, session.Namespace, session.Name); !errors.Is(err, tt.wantError) {
				t.Fatalf("DeleteSession() error = %v, want %v", err, tt.wantError)
			}
			if tt.wantError != nil {
				got, err := s.GetSession(ctx, session.Namespace, session.Name)
				if err != nil {
					t.Fatal(err)
				}
				if got.MessageCount != 2 || got.InputTokens != 3 || got.OutputTokens != 2 {
					t.Fatalf("live session changed: %+v", got)
				}
				return
			}
			if _, err := s.GetSession(ctx, session.Namespace, session.Name); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetSession() error = %v, want ErrNotFound", err)
			}
			var messages int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_messages WHERE namespace = ? AND session_name = ?`, session.Namespace, session.Name).Scan(&messages); err != nil {
				t.Fatal(err)
			}
			if messages != 0 {
				t.Fatalf("deleted session left %d messages", messages)
			}
			if err := s.CommitSessionTurn(ctx, session, "abandoned-turn", 2, []store.SessionMessage{
				{Role: "assistant", Content: "late response"},
			}, 0, 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stale CommitSessionTurn() error = %v, want ErrNotFound", err)
			}
		})
	}
}
