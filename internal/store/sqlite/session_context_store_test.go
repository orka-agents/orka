package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/store"
)

func newSessionContextTestStore(t *testing.T) (*Store, store.SessionContextWrite) {
	t.Helper()
	s := setupTestStore(t)
	return s, createSessionContextTestSession(t, s, "tenant-a", "session-a")
}

func createSessionContextTestSession(t *testing.T, s *Store, namespace, name string) store.SessionContextWrite {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	write := store.SessionContextWrite{Namespace: namespace, SessionName: name, OwnerName: "task-a", OwnerUID: "uid-a"}
	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: namespace, Name: name, SessionType: "task", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession(): %v", err)
	}
	if err := s.AcquireLockUntil(ctx, namespace, name, write.OwnerName, write.OwnerUID, now.Add(time.Hour)); err != nil {
		t.Fatalf("AcquireLockUntil(): %v", err)
	}
	return write
}

func appendSessionContextTestMessages(t *testing.T, s *Store, write store.SessionContextWrite, messages ...store.SessionMessage) []store.SessionMessage {
	t.Helper()
	canonical, err := s.AppendContextMessages(context.Background(), write, messages)
	if err != nil {
		t.Fatalf("AppendContextMessages(): %v", err)
	}
	return canonical
}

func readAllSessionContextTestHistory(t *testing.T, s *Store, read store.SessionHistoryRead) []byte {
	t.Helper()
	var data []byte
	for {
		page, err := s.ReadSessionHistory(context.Background(), read)
		if err != nil {
			t.Fatalf("ReadSessionHistory(): %v", err)
		}
		if page.Offset != read.Offset || page.NextOffset-page.Offset != len(page.Data) ||
			len(page.Data) > read.Limit || !utf8.ValidString(page.Data) {
			t.Fatal("history page did not preserve its byte bounds or UTF-8")
		}
		data = append(data, page.Data...)
		if page.NextOffset == page.TotalBytes {
			return data
		}
		if page.NextOffset <= read.Offset {
			t.Fatal("history page did not advance")
		}
		read.Offset = page.NextOffset
	}
}

func TestSessionContextHistoryPreservesSourcesAndPreviews(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	long := store.SessionMessage{
		ID: "tool-output", Role: "tool", Name: "inspect", ToolCallID: "call-a",
		Content:    strings.Repeat("測定結果", 6000) + " final finding",
		Input:      map[string]any{"largeNumber": json.Number("9007199254740993")},
		SourceType: "task", SourceRef: "task-a", Metadata: map[string]string{"path": "api.go"},
	}
	messages := []store.SessionMessage{
		{ID: "request", Role: "user", Content: "Keep the API unchanged."},
		{ID: "call", Role: "assistant", ToolCalls: []any{map[string]any{"id": "call-a", "name": "inspect"}}},
		long,
	}
	canonical := appendSessionContextTestMessages(t, s, write, messages...)
	if len(canonical[2].Content) > store.MaxSessionContextPreviewBytes || !utf8.ValidString(canonical[2].Content) ||
		canonical[2].Metadata[store.SessionContextOutputRefKey] != long.ID || !strings.Contains(canonical[2].Content, "read_session_history") {
		t.Fatal("large output did not produce a bounded, readable preview reference")
	}
	if long.Metadata[store.SessionContextOutputRefKey] != "" || len(long.Content) <= store.MaxSessionContextPreviewBytes {
		t.Fatal("saving output mutated the caller's message")
	}
	data := readAllSessionContextTestHistory(t, s, store.SessionHistoryRead{
		Namespace: write.Namespace, SessionName: write.SessionName, MessageID: long.ID, Limit: store.MaxSessionHistoryReadBytes,
	})
	var recovered store.SessionMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&recovered); err != nil {
		t.Fatalf("decode recovered message: %v", err)
	}
	if recovered.Content != long.Content || recovered.ToolCallID != long.ToolCallID || recovered.Role != long.Role ||
		recovered.Name != long.Name || recovered.SourceRef != long.SourceRef || recovered.SourceType != long.SourceType ||
		recovered.Input["largeNumber"] != json.Number("9007199254740993") || recovered.Metadata["path"] != "api.go" {
		t.Fatal("recovered output changed its content, role, or tool metadata")
	}
	for _, retry := range [][]store.SessionMessage{messages, canonical, {recovered}} {
		if _, err := s.AppendContextMessages(ctx, write, retry); err != nil {
			t.Fatalf("idempotent source or preview retry: %v", err)
		}
	}
	changed := long
	changed.Content = strings.TrimSuffix(long.Content, " final finding") + " different ending"
	if _, err := s.AppendContextMessages(ctx, write, []store.SessionMessage{changed}); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("changed original after the preview prefix: %v, want ErrDuplicateMismatch", err)
	}
	session, err := s.GetSession(ctx, write.Namespace, write.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	if session.MessageCount != 3 || len(session.Messages) != 3 {
		t.Fatalf("canonical message count after retries = %d, want 3", session.MessageCount)
	}
	if session.Messages[2].Content != canonical[2].Content {
		t.Fatal("Session transcript did not retain the canonical preview")
	}
}

func TestSessionCheckpointRejectsMissingSavedOutput(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	if err := s.AppendMessages(ctx, write.Namespace, write.SessionName, []store.SessionMessage{
		{ID: "dangling", Role: "tool", Content: "Only a preview.", Metadata: map[string]string{store.SessionContextOutputRefKey: "dangling"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, sources := range [][]string{nil, {"dangling"}} {
		if err := s.SaveSessionCheckpoint(ctx, write, store.SessionCheckpoint{
			ID: "checkpoint", Version: store.SessionCheckpointVersion, LastMessageID: "dangling", Note: "Unavailable source.", SourceMessageIDs: sources,
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("checkpoint referencing an unsaved original: %v, want ErrNotFound", err)
		}
	}
	if _, err := s.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: write.Namespace, SessionName: write.SessionName, MessageID: "dangling", Limit: 1024,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("history with missing original: %v, want ErrNotFound", err)
	}
}

func TestSessionContextPreservesLegacyMessageIdentity(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	message := store.SessionMessage{ID: "legacy-source", Role: "tool", Content: strings.Repeat("original ", 1500)}
	if err := s.AppendMessages(ctx, write.Namespace, write.SessionName, []store.SessionMessage{message}); err != nil {
		t.Fatal(err)
	}
	canonical := appendSessionContextTestMessages(t, s, write, message)
	if len(canonical) != 1 || canonical[0].ID != message.ID || len(canonical[0].Content) > store.MaxSessionContextPreviewBytes {
		t.Fatal("adding output recovery changed the legacy message identity or omitted its preview")
	}
	appendSessionContextTestMessages(t, s, write, message)
	session, err := s.GetSession(ctx, write.Namespace, write.SessionName)
	if err != nil || session.MessageCount != 1 || len(session.Messages) != 1 {
		t.Fatalf("adding output recovery duplicated the legacy message: %v", err)
	}
	data := readAllSessionContextTestHistory(t, s, store.SessionHistoryRead{
		Namespace: write.Namespace, SessionName: write.SessionName, MessageID: message.ID, Limit: 1024,
	})
	if !bytes.Contains(data, []byte(message.Content)) {
		t.Fatal("legacy message recovery lost the full output")
	}
}

func TestSessionContextHistoryBoundsAndIsolation(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	appendSessionContextTestMessages(t, s, write,
		store.SessionMessage{ID: "early", Role: "user", Content: "Keep constraints."},
		store.SessionMessage{ID: "later", Role: "assistant", Content: "測定"},
	)
	otherSession := createSessionContextTestSession(t, s, write.Namespace, "other-session")
	otherNamespace := createSessionContextTestSession(t, s, "other-tenant", write.SessionName)
	for _, other := range []store.SessionContextWrite{otherSession, otherNamespace} {
		appendSessionContextTestMessages(t, s, other, store.SessionMessage{ID: "foreign", Role: "tool", Content: "Other Session data."})
	}
	valid := store.SessionHistoryRead{Namespace: write.Namespace, SessionName: write.SessionName, MessageID: "early", Limit: 1024}
	for _, test := range []struct {
		name   string
		change func(*store.SessionHistoryRead)
	}{
		{"after boundary", func(r *store.SessionHistoryRead) { r.MessageID, r.ThroughMessageID = "later", "early" }},
		{"foreign message", func(r *store.SessionHistoryRead) { r.MessageID = "foreign" }},
		{"foreign boundary", func(r *store.SessionHistoryRead) { r.ThroughMessageID = "foreign" }},
		{"other namespace", func(r *store.SessionHistoryRead) { r.Namespace = otherNamespace.Namespace }},
		{"other Session", func(r *store.SessionHistoryRead) { r.SessionName = otherSession.SessionName }},
		{"missing Session", func(r *store.SessionHistoryRead) { r.SessionName = "absent" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := valid
			test.change(&read)
			if _, err := s.ReadSessionHistory(ctx, read); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("ReadSessionHistory() error = %v, want ErrNotFound", err)
			}
		})
	}
	valid.MessageID, valid.ThroughMessageID = "later", "later"
	data := readAllSessionContextTestHistory(t, s, valid)
	runeOffset := bytes.Index(data, []byte("測"))
	if runeOffset < 0 {
		t.Fatal("missing UTF-8 fixture in saved JSON")
	}
	for _, test := range []struct{ offset, limit int }{
		{-1, 1024}, {len(data) + 1, 1024}, {int(^uint(0) >> 1), 1024},
		{runeOffset + 1, 1024}, {runeOffset, 1}, {0, 0}, {0, store.MaxSessionHistoryReadBytes + 1},
	} {
		read := valid
		read.Offset, read.Limit = test.offset, test.limit
		if _, err := s.ReadSessionHistory(ctx, read); !errors.Is(err, store.ErrValidation) {
			t.Fatalf("invalid byte range (%d, %d): %v, want ErrValidation", test.offset, test.limit, err)
		}
	}
	valid.Offset = len(data)
	last, err := s.ReadSessionHistory(ctx, valid)
	if err != nil || last.Data != "" || last.NextOffset != len(data) {
		t.Fatalf("end-of-message read did not return an empty final page: %v", err)
	}
}

func TestSessionCheckpointSourcesBoundariesAndRetries(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	appendSessionContextTestMessages(t, s, write,
		store.SessionMessage{ID: "first", Role: "user", Content: "Keep the API unchanged."},
		store.SessionMessage{ID: "second", Role: "assistant", Content: "The parser passed."},
		store.SessionMessage{ID: "third", Role: "assistant", Content: "Retry handling is next."},
	)
	first := store.SessionCheckpoint{
		ID: "first-checkpoint", Version: store.SessionCheckpointVersion, LastMessageID: "first",
		Note: "Keep the API unchanged. Source: first.", SourceMessageIDs: []string{"first"},
	}
	if err := s.SaveSessionCheckpoint(ctx, write, first); err != nil {
		t.Fatal(err)
	}
	saved, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, "first")
	if err != nil || saved.Namespace != write.Namespace || saved.SessionName != write.SessionName || saved.LastMessageOrder != 2 || saved.CreatedAt.IsZero() {
		t.Fatalf("checkpoint did not derive Session identity, order, and time: %v", err)
	}
	for _, retry := range []store.SessionCheckpoint{first, *saved} {
		if err := s.SaveSessionCheckpoint(ctx, write, retry); err != nil {
			t.Fatalf("checkpoint retry: %v", err)
		}
	}
	changed := first
	changed.Note = "A different note using the same checkpoint ID."
	if err := s.SaveSessionCheckpoint(ctx, write, changed); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("changed checkpoint retry: %v, want ErrDuplicateMismatch", err)
	}
	latest := first
	latest.ID, latest.LastMessageID = "latest-checkpoint", "third"
	latest.SourceMessageIDs = []string{"first", "second", "third"}
	latest.Note = "Keep the API unchanged. The parser passed. Inspect retries next. Sources: first, second, third."
	if err := s.SaveSessionCheckpoint(ctx, write, latest); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ through, want string }{
		{"", latest.ID}, {"third", latest.ID}, {"second", first.ID}, {"first", first.ID},
	} {
		loaded, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, test.through)
		if err != nil || loaded.ID != test.want {
			t.Fatalf("checkpoint through %q: %v, want %s", test.through, err, test.want)
		}
	}
	other := createSessionContextTestSession(t, s, "other-tenant", write.SessionName)
	appendSessionContextTestMessages(t, s, other, store.SessionMessage{ID: "foreign", Role: "assistant", Content: "Different tenant."})
	for _, test := range []struct {
		name   string
		change func(*store.SessionContextWrite, *store.SessionCheckpoint)
		want   error
	}{
		{"missing source", func(_ *store.SessionContextWrite, c *store.SessionCheckpoint) {
			c.SourceMessageIDs = []string{"missing"}
		}, store.ErrNotFound},
		{"foreign source", func(_ *store.SessionContextWrite, c *store.SessionCheckpoint) {
			c.SourceMessageIDs = []string{"foreign"}
		}, store.ErrNotFound},
		{"after last message", func(_ *store.SessionContextWrite, c *store.SessionCheckpoint) {
			c.SourceMessageIDs = []string{"second"}
		}, store.ErrNotFound},
		{"after reader boundary", func(w *store.SessionContextWrite, c *store.SessionCheckpoint) {
			w.ThroughMessageID, c.LastMessageID = "first", "third"
		}, store.ErrNotFound},
		{"forged order", func(_ *store.SessionContextWrite, c *store.SessionCheckpoint) { c.LastMessageOrder = 1 }, store.ErrValidation},
		{"foreign namespace", func(_ *store.SessionContextWrite, c *store.SessionCheckpoint) { c.Namespace = other.Namespace }, store.ErrValidation},
		{"foreign Session", func(_ *store.SessionContextWrite, c *store.SessionCheckpoint) { c.SessionName = "other-session" }, store.ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, target := first, write
			candidate.ID = "invalid-checkpoint"
			test.change(&target, &candidate)
			if err := s.SaveSessionCheckpoint(ctx, target, candidate); !errors.Is(err, test.want) {
				t.Fatalf("SaveSessionCheckpoint() error = %v, want %v", err, test.want)
			}
		})
	}
	bounded := write
	bounded.ThroughMessageID = "second"
	first.ID = "bounded-checkpoint"
	if err := s.SaveSessionCheckpoint(ctx, bounded, first); err != nil {
		t.Fatalf("checkpoint confined to the reader boundary: %v", err)
	}
	for _, boundary := range []string{"missing", "foreign"} {
		if _, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, boundary); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign or missing load boundary: %v, want ErrNotFound", err)
		}
	}
}

func TestSessionCheckpointRetentionAndSessionDeletion(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	for i := 1; i <= store.MaxSessionCheckpoints+2; i++ {
		id := fmt.Sprintf("message-%d", i)
		appendSessionContextTestMessages(t, s, write, store.SessionMessage{ID: id, Role: "tool", Content: strings.Repeat("result ", 1500)})
		if err := s.SaveSessionCheckpoint(ctx, write, store.SessionCheckpoint{
			ID: fmt.Sprintf("checkpoint-%d", i), Version: store.SessionCheckpointVersion, LastMessageID: id,
			Note: "A saved finding references " + id + ".", SourceMessageIDs: []string{id},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, "message-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old checkpoint was not evicted: %v", err)
	}
	if checkpoint, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, "message-3"); err != nil || checkpoint.ID != "checkpoint-3" {
		t.Fatalf("first retained checkpoint missing: %v", err)
	}
	if err := s.ReleaseLock(ctx, write.Namespace, write.SessionName, write.OwnerName, write.OwnerUID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, write.Namespace, write.SessionName); err != nil {
		t.Fatal(err)
	}
	assertNoSessionContextRows(t, s, write)
	if _, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("checkpoint recovery after deletion: %v, want ErrNotFound", err)
	}
	if _, err := s.AppendContextMessages(ctx, write, []store.SessionMessage{{ID: "late", Role: "user", Content: "late"}}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("append recreated deleted Session: %v", err)
	}
	if err := s.SaveSessionCheckpoint(ctx, write, store.SessionCheckpoint{
		ID: "late", Version: store.SessionCheckpointVersion, LastMessageID: "message-6", Note: "Late checkpoint.",
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("checkpoint recreated deleted Session: %v", err)
	}
	recreated := createSessionContextTestSession(t, s, write.Namespace, write.SessionName)
	appendSessionContextTestMessages(t, s, recreated, store.SessionMessage{ID: "message-6", Role: "user", Content: "A new Session incarnation."})
	if _, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, "message-6"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Session recreation exposed an old checkpoint: %v", err)
	}
}

func assertNoSessionContextRows(t *testing.T, s *Store, write store.SessionContextWrite) {
	t.Helper()
	for _, table := range []string{"session_context_outputs", "session_checkpoints"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE namespace = ? AND session_name = ?`, write.Namespace, write.SessionName).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d rows after Session deletion", table, count)
		}
	}
}

func TestSessionContextRejectsExpiredOwnerAndGatewayWrites(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	appendSessionContextTestMessages(t, s, write, store.SessionMessage{ID: "first", Role: "user", Content: "First request."})
	checkpoint := store.SessionCheckpoint{ID: "checkpoint", Version: store.SessionCheckpointVersion, LastMessageID: "first", Note: "First request is pending."}
	if _, err := s.db.Exec(`UPDATE sessions SET active_task_expires_at = ? WHERE namespace = ? AND name = ?`,
		time.Now().UTC().Add(-time.Minute), write.Namespace, write.SessionName); err != nil {
		t.Fatal(err)
	}
	assertRejected := func(want error) {
		t.Helper()
		if _, err := s.AppendContextMessages(ctx, write, []store.SessionMessage{{ID: "late", Role: "assistant", Content: "Late result."}}); !errors.Is(err, want) {
			t.Fatalf("late append error = %v, want %v", err, want)
		}
		if err := s.SaveSessionCheckpoint(ctx, write, checkpoint); !errors.Is(err, want) {
			t.Fatalf("late checkpoint error = %v, want %v", err, want)
		}
	}
	assertRejected(store.ErrConflict)
	if err := s.AcquireLockUntil(ctx, write.Namespace, write.SessionName, write.OwnerName, "new-uid", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertRejected(store.ErrConflict)
	write.OwnerUID = "new-uid"
	if err := s.SaveSessionCheckpoint(ctx, write, checkpoint); err != nil {
		t.Fatalf("current owner checkpoint: %v", err)
	}
	for _, field := range []string{"session_type", "owner_type"} {
		if _, err := s.db.Exec(`UPDATE sessions SET ` + field + ` = 'gateway'`); err != nil {
			t.Fatal(err)
		}
		assertRejected(store.ErrGatewayOwnedSession)
		if _, err := s.db.Exec(`UPDATE sessions SET ` + field + ` = ''`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionContextAppendRollsBackPartialPersistence(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
	}{
		{"output storage failure", `CREATE TRIGGER fail_context_output BEFORE INSERT ON session_context_outputs
		 BEGIN SELECT RAISE(ABORT, 'context output unavailable'); END`},
		{"owner expires before commit", `CREATE TRIGGER expire_context_owner AFTER INSERT ON session_messages
		 BEGIN UPDATE sessions SET active_task_expires_at = '2000-01-01 00:00:00'; END`},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, write := newSessionContextTestStore(t)
			if _, err := s.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			_, err := s.AppendContextMessages(context.Background(), write, []store.SessionMessage{
				{ID: "small", Role: "user", Content: "Keep the current request."},
				{ID: "large", Role: "tool", Content: strings.Repeat("x", store.MaxSessionContextPreviewBytes+1)},
			})
			if err == nil {
				t.Fatal("append unexpectedly succeeded after an in-transaction failure")
			}
			session, err := s.GetSession(context.Background(), write.Namespace, write.SessionName)
			if err != nil || session.MessageCount != 0 || len(session.Messages) != 0 {
				t.Fatalf("failed append retained canonical messages: %v", err)
			}
			assertNoSessionContextRows(t, s, write)
		})
	}
}

func TestSessionCheckpointFailureAndCancellationKeepCommittedHistory(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	appendSessionContextTestMessages(t, s, write, store.SessionMessage{ID: "source", Role: "tool", Content: strings.Repeat("x", 9000)})
	checkpoint := store.SessionCheckpoint{ID: "checkpoint", Version: store.SessionCheckpointVersion, LastMessageID: "source", Note: "One finding is saved.", SourceMessageIDs: []string{"source"}}
	if err := s.SaveSessionCheckpoint(ctx, write, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER expire_checkpoint_owner AFTER INSERT ON session_checkpoints
		BEGIN UPDATE sessions SET active_task_expires_at = '2000-01-01 00:00:00'; END`); err != nil {
		t.Fatal(err)
	}
	checkpoint.ID = "failed-checkpoint"
	if err := s.SaveSessionCheckpoint(ctx, write, checkpoint); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("checkpoint commit after owner expiry: %v, want ErrConflict", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.AppendContextMessages(cancelled, write, []store.SessionMessage{{ID: "cancelled", Role: "user", Content: "Cancelled request."}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled append: %v", err)
	}
	if err := s.SaveSessionCheckpoint(cancelled, write, checkpoint); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled checkpoint save: %v", err)
	}
	loaded, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, "")
	if err != nil || loaded.ID != "checkpoint" {
		t.Fatalf("failed checkpoint replaced the committed checkpoint: %v", err)
	}
	data := readAllSessionContextTestHistory(t, s, store.SessionHistoryRead{
		Namespace: write.Namespace, SessionName: write.SessionName, MessageID: "source", Limit: 4096,
	})
	if !bytes.Contains(data, []byte(strings.Repeat("x", 9000))) {
		t.Fatal("failed checkpoint lost committed source data")
	}
}

func TestSessionContextCleanupIntentRemovesData(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	appendSessionContextTestMessages(t, s, write, store.SessionMessage{ID: "source", Role: "tool", Content: strings.Repeat("x", 9000)})
	checkpoint := store.SessionCheckpoint{ID: "checkpoint", Version: store.SessionCheckpointVersion, LastMessageID: "source", Note: "A saved finding.", SourceMessageIDs: []string{"source"}}
	if err := s.SaveSessionCheckpoint(ctx, write, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseLock(ctx, write.Namespace, write.SessionName, write.OwnerName, write.OwnerUID); err != nil {
		t.Fatal(err)
	}
	intent := store.SessionCleanupIntent{
		Namespace: write.Namespace, SessionName: write.SessionName, OperationID: "delete-context",
		OperationDigest: controlTestDigest("delete-context"), PreparedAt: time.Now().UTC(),
	}
	if _, err := s.PrepareSessionCleanup(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSessionCheckpoint(ctx, write, checkpoint); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("write during Session cleanup: %v", err)
	}
	if _, err := s.LoadSessionCheckpoint(ctx, write.Namespace, write.SessionName, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("checkpoint load during Session cleanup: %v", err)
	}
	if _, err := s.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: write.Namespace, SessionName: write.SessionName, MessageID: "source", Limit: 1024,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("history read during Session cleanup: %v", err)
	}
	if err := s.CompleteSessionCleanup(ctx, store.CompleteSessionCleanupRequest{
		Namespace: intent.Namespace, SessionName: intent.SessionName, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}); err != nil {
		t.Fatal(err)
	}
	assertNoSessionContextRows(t, s, write)
}

func TestSessionContextRedactsSourceDataAndRejectsUnsupportedWrites(t *testing.T) {
	s, write := newSessionContextTestStore(t)
	ctx := context.Background()
	fixture := strings.Repeat("synthetic-test-value", 4)
	message := store.SessionMessage{
		ID: "source", Role: "tool", Content: "api_key=" + fixture + "\n" + strings.Repeat("x", 9000),
		Input:     map[string]any{"nested": map[string]any{"authorization": fixture, "plain": "Findings."}},
		ToolCalls: []any{map[string]any{"id": "call-a", "arguments": "password=" + fixture}},
		Metadata:  map[string]string{"credential": fixture},
	}
	appendSessionContextTestMessages(t, s, write, message)
	data := readAllSessionContextTestHistory(t, s, store.SessionHistoryRead{
		Namespace: write.Namespace, SessionName: write.SessionName, MessageID: "source", Limit: 4096,
	})
	if bytes.Contains(data, []byte(fixture)) || !bytes.Contains(data, []byte("[REDACTED]")) {
		t.Fatal("stored source JSON did not remove credential-shaped values")
	}
	if err := s.SaveSessionCheckpoint(ctx, write, store.SessionCheckpoint{
		ID: "unsafe", Version: store.SessionCheckpointVersion, LastMessageID: "source", Note: "api_key=" + fixture,
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("credential-shaped checkpoint note: %v, want ErrValidation", err)
	}
	for _, invalid := range []store.SessionMessage{
		{Role: "user", Content: "Missing stable ID."},
		{ID: "too-large", Role: "tool", Content: strings.Repeat("x", store.MaxSessionContextMessageBytes)},
		{ID: "invalid-text", Role: "tool", Content: string([]byte{0xff})},
		{ID: "forged-reference", Role: "tool", Metadata: map[string]string{store.SessionContextOutputRefKey: "source"}},
		{ID: "old-order", Role: "user", Content: "Insert before saved history.", Order: 1},
	} {
		if _, err := s.AppendContextMessages(ctx, write, []store.SessionMessage{invalid}); !errors.Is(err, store.ErrValidation) {
			t.Fatalf("invalid context message: %v, want ErrValidation", err)
		}
	}
	write.ThroughMessageID = "source"
	if _, err := s.AppendContextMessages(ctx, write, []store.SessionMessage{{ID: "new", Role: "user", Content: "New request."}}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("append through pinned reader boundary: %v, want ErrValidation", err)
	}
}

func TestSessionContextSurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, path)
	write := createSessionContextTestSession(t, s, "tenant-a", "session-a")
	appendSessionContextTestMessages(t, s, write, store.SessionMessage{ID: "source", Role: "tool", Content: strings.Repeat("saved ", 2000)})
	if err := s.SaveSessionCheckpoint(context.Background(), write, store.SessionCheckpoint{
		ID: "checkpoint", Version: store.SessionCheckpointVersion, LastMessageID: "source", Note: "A verified parser finding.", SourceMessageIDs: []string{"source"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, err := NewDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedDB.Close() })
	reopened := NewStore(reopenedDB, path)
	checkpoint, err := reopened.LoadSessionCheckpoint(context.Background(), write.Namespace, write.SessionName, "source")
	if err != nil || checkpoint.ID != "checkpoint" {
		t.Fatalf("reopened checkpoint: %v", err)
	}
	data := readAllSessionContextTestHistory(t, reopened, store.SessionHistoryRead{
		Namespace: write.Namespace, SessionName: write.SessionName, MessageID: "source", Limit: 4096,
	})
	if !bytes.Contains(data, []byte(strings.Repeat("saved ", 2000))) {
		t.Fatal("reopening the store lost saved output")
	}
}
