package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

func TestSessionSoulMetadataSurvivesTaskAndSourceChanges(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	session := &store.SessionRecord{Namespace: "team", Name: "s", SessionType: "task", ActiveTask: "first", ActiveTaskUID: "first-uid", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	state, err := s.ReadSessionSoul(ctx, "team", "s", "first", "first-uid")
	if err != nil || state.Established {
		t.Fatalf("new session: %+v %v", state, err)
	}
	digest := agentcontext.Digest("frozen configuration")
	if err := s.AppendMessages(ctx, "team", "s", []store.SessionMessage{{Role: "user", Content: "task", Metadata: map[string]string{store.SessionSoulDigestMetadata: digest}}, {Role: "assistant", Content: "result", Metadata: map[string]string{store.SessionSoulDigestMetadata: digest}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseLock(ctx, "team", "s", "first", "first-uid"); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLock(ctx, "team", "s", "second", "second-uid"); err != nil {
		t.Fatal(err)
	}
	state, err = s.ReadSessionSoul(ctx, "team", "s", "second", "second-uid")
	if err != nil || !state.Established || state.Digest != digest {
		t.Fatalf("continued session: %+v %v", state, err)
	}
	if _, err := s.ReadSessionSoul(ctx, "team", "s", "first", "first-uid"); err == nil {
		t.Fatal("old owner read the binding")
	}
	if err := s.AppendMessages(ctx, "team", "s", []store.SessionMessage{{Role: "assistant", Content: "later", Metadata: map[string]string{store.SessionSoulDigestMetadata: agentcontext.Digest("other")}}}); err != nil {
		t.Fatal(err)
	}
	state, err = s.ReadSessionSoul(ctx, "team", "s", "second", "second-uid")
	if err != nil || state.Digest != digest {
		t.Fatal("later transcript metadata replaced the original binding")
	}
}

func TestLegacyTranscriptEstablishesNoSoul(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "team", Name: "legacy", SessionType: "task", ActiveTask: "t", ActiveTaskUID: "uid", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessages(ctx, "team", "legacy", []store.SessionMessage{{Role: "user", Content: "legacy prompt"}}); err != nil {
		t.Fatal(err)
	}
	state, err := s.ReadSessionSoul(ctx, "team", "legacy", "t", "uid")
	if err != nil || !state.Established || state.Digest != "" {
		t.Fatalf("legacy state: %+v %v", state, err)
	}
}

func TestGatewayRetentionPreservesSoulWithoutRetainingMessageContent(t *testing.T) {
	for _, digest := range []string{agentcontext.Digest("original persona"), ""} {
		t.Run(digest, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			old := now.Add(-2 * time.Hour)
			event := testGatewayEvent(old.Add(-time.Minute), "soul-retention")
			event.BindingUID = testGatewayBindingUID
			if _, _, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 100}); err != nil {
				t.Fatal(err)
			}
			if err := s.ExpireGatewayEvent(ctx, event.Namespace, event.ID, "", "expired", old); err != nil {
				t.Fatal(err)
			}
			attachDeliveredExpiryDelivery(t, s, ctx, event, old)
			metadata, err := json.Marshal(map[string]string{store.SessionSoulDigestMetadata: digest})
			if err != nil {
				t.Fatal(err)
			}
			// Some expiry paths already create the canonical error message. Attach the
			// controller metadata to that message, or create its missing fixture row.
			result, err := s.db.ExecContext(ctx, `UPDATE session_messages SET metadata_json = ?, content = 'expired private response'
   WHERE namespace = ? AND session_name = ? AND message_id = ?`, string(metadata), event.Namespace, event.SessionName, store.GatewayErrorMessageID(event.ID))
			if err != nil {
				t.Fatal(err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				t.Fatal(err)
			}
			if count == 0 {
				_, err = s.db.ExecContext(ctx, `INSERT INTO session_messages(namespace,session_name,message_id,sort_order,role,content,source_type,source_ref,metadata_json,created_at)
    SELECT ?,?,?,COALESCE(MAX(sort_order),0)+1,'assistant','expired private response','gateway-task','',?,?
    FROM session_messages WHERE namespace = ? AND session_name = ?`, event.Namespace, event.SessionName, store.GatewayErrorMessageID(event.ID), string(metadata), old, event.Namespace, event.SessionName)
				if err != nil {
					t.Fatal(err)
				}
			}
			next := testGatewayEvent(now, "soul-retention-next")
			next.ID = "gev-next-soul"
			next.ExternalEventID = "next-soul-input"
			next.SessionName = event.SessionName
			next.BindingUID = event.BindingUID
			next.ContextID = event.ContextID
			if _, _, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: next, AppendUserMessage: true, PendingLimit: 100}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET active_task = 'current', active_task_uid = 'current-uid' WHERE namespace = ? AND name = ?`, event.Namespace, event.SessionName); err != nil {
				t.Fatal(err)
			}
			state, err := s.ReadSessionSoul(ctx, event.Namespace, event.SessionName, "current", "current-uid")
			if err != nil {
				t.Fatal(err)
			}
			if !state.Established || state.Digest != digest {
				t.Fatalf("retention lost the established revision: %+v", state)
			}
			transcript, err := s.LoadTranscript(ctx, event.Namespace, event.SessionName, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range transcript {
				if message.SourceType == store.SessionSoulAnchorSource || message.Content == "expired private response" {
					t.Fatal("retention exposed expired content or internal identity metadata")
				}
			}
			var privateContent int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE namespace = ? AND session_name = ? AND content = 'expired private response'`, event.Namespace, event.SessionName).Scan(&privateContent); err != nil || privateContent != 0 {
				t.Fatal("expired content remains in storage")
			}
		})
	}
}
