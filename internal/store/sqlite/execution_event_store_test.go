package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

//nolint:gocyclo // Keeps append/list/latest/delete coverage together for store lifecycle readability.
func TestExecutionEventStoreAppendListLatestDelete(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()

	first, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   "default",
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    "task-1",
		TaskName:    "task-1",
		Type:        events.ExecutionEventTypeTaskStarted,
		Severity:    "WARNING",
		Summary:     "started",
		Content:     json.RawMessage(`{"safe":"ok"}`),
		ContentText: "hello",
		Truncation: &events.ExecutionEventTruncation{
			ContentTextTruncated:     true,
			ContentTextOriginalChars: 100,
		},
		CreatedAt: time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent first: %v", err)
	}
	second, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
		TaskName:   "task-1",
		Type:       events.ExecutionEventTypeToolCallCompleted,
		Severity:   events.ExecutionEventSeverityInfo,
		Summary:    "tool done",
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent second: %v", err)
	}
	other, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-2",
		TaskName:   "task-2",
		Type:       events.ExecutionEventTypeTaskFailed,
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent other: %v", err)
	}

	if first.Seq != 1 || second.Seq != 2 || other.Seq != 1 {
		t.Fatalf("seqs = first %d second %d other %d, want 1 2 1", first.Seq, second.Seq, other.Seq)
	}
	if first.ID == "" || second.ID == "" || first.ID == second.ID {
		t.Fatalf("assigned IDs invalid: first=%q second=%q", first.ID, second.ID)
	}
	if first.Severity != events.ExecutionEventSeverityWarning {
		t.Fatalf("severity = %q, want warning", first.Severity)
	}

	latest, err := s.GetLatestExecutionEventSeq(ctx, "default", store.ExecutionEventStreamTypeTask, "task-1")
	if err != nil {
		t.Fatalf("GetLatestExecutionEventSeq: %v", err)
	}
	if latest != 2 {
		t.Fatalf("latest = %d, want 2", latest)
	}

	listed, err := s.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 2 || listed[0].Seq != 1 || listed[1].Seq != 2 {
		t.Fatalf("listed = %#v, want seqs 1,2", listed)
	}
	if string(listed[0].Content) != `{"safe":"ok"}` || listed[0].Truncation == nil || !listed[0].Truncation.ContentTextTruncated {
		t.Fatalf("listed first payload not preserved: %#v content=%s", listed[0], listed[0].Content)
	}

	filtered, err := s.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
		AfterSeq:   1,
		EventTypes: []string{events.ExecutionEventTypeToolCallCompleted},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents filtered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Seq != 2 || filtered[0].Type != events.ExecutionEventTypeToolCallCompleted {
		t.Fatalf("filtered = %#v, want only seq 2 tool event", filtered)
	}

	if err := s.DeleteExecutionEvents(ctx, "default", store.ExecutionEventStreamTypeTask, "task-1"); err != nil {
		t.Fatalf("DeleteExecutionEvents: %v", err)
	}
	remainingTask1, err := s.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-1"})
	if err != nil {
		t.Fatalf("ListExecutionEvents after delete: %v", err)
	}
	remainingTask2, err := s.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-2"})
	if err != nil {
		t.Fatalf("ListExecutionEvents other after delete: %v", err)
	}
	if len(remainingTask1) != 0 || len(remainingTask2) != 1 {
		t.Fatalf("remaining task1=%d task2=%d, want 0 and 1", len(remainingTask1), len(remainingTask2))
	}
	latest, err = s.GetLatestExecutionEventSeq(ctx, "default", store.ExecutionEventStreamTypeTask, "task-1")
	if err != nil {
		t.Fatalf("GetLatestExecutionEventSeq after delete: %v", err)
	}
	if latest != 0 {
		t.Fatalf("latest after delete = %d, want 0", latest)
	}
}

func TestExecutionEventStoreListDefaultAndMaxLimit(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	for i := range store.MaxExecutionEventLimit + 5 {
		if _, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace:  "default",
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   "task-limit",
			TaskName:   "task-limit",
			Type:       events.ExecutionEventTypeModelMessage,
		}); err != nil {
			t.Fatalf("AppendExecutionEvent %d: %v", i, err)
		}
	}
	defaulted, err := s.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-limit"})
	if err != nil {
		t.Fatalf("ListExecutionEvents default limit: %v", err)
	}
	if len(defaulted) != store.DefaultExecutionEventLimit {
		t.Fatalf("defaulted len = %d, want %d", len(defaulted), store.DefaultExecutionEventLimit)
	}
	capped, err := s.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-limit", Limit: store.MaxExecutionEventLimit + 50})
	if err != nil {
		t.Fatalf("ListExecutionEvents capped limit: %v", err)
	}
	if len(capped) != store.MaxExecutionEventLimit {
		t.Fatalf("capped len = %d, want %d", len(capped), store.MaxExecutionEventLimit)
	}
}

func TestEventSecretRedactionPersistedRows(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	secrets := executionEventAuditSecrets()
	content := map[string]any{
		"authorization":     "Bearer " + secrets["bearer"],
		"jwt":               secrets["jwt"],
		"apiKey":            secrets["openai"],
		"cookie":            "session=" + secrets["cookie"],
		"transaction-token": secrets["txn"],
		"githubToken":       secrets["github"],
		"anthropic_api_key": secrets["anthropic"],
		"safe":              "preserved",
	}
	contentBytes, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if _, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   "default",
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    "task-redact-store",
		TaskName:    "task-redact-store",
		Type:        events.ExecutionEventTypeModelMessage,
		Summary:     "Authorization: Bearer " + secrets["bearer"] + "\nTransaction-Token: " + secrets["txn"],
		Content:     json.RawMessage(contentBytes),
		ContentText: "Cookie: session=" + secrets["cookie"] + "\napi_key=" + secrets["openai"],
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}

	var persisted string
	if err := s.db.QueryRowContext(ctx, `SELECT summary || COALESCE(content_json, '') || content_text FROM execution_events WHERE stream_id = ?`, "task-redact-store").Scan(&persisted); err != nil {
		t.Fatalf("query persisted event: %v", err)
	}
	for name, secret := range secrets {
		if strings.Contains(persisted, secret) {
			t.Fatalf("persisted event leaked %s secret %q in %q", name, secret, persisted)
		}
	}
	if !strings.Contains(persisted, events.ExecutionEventRedactedValue) {
		t.Fatalf("persisted event = %q, want redaction marker", persisted)
	}
	listed, err := s.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-redact-store",
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 1 || !strings.Contains(string(listed[0].Content), events.ExecutionEventRedactedValue) {
		t.Fatalf("listed redacted event = %#v content=%s", listed, listed[0].Content)
	}
}

func TestExecutionEventStoreConcurrentSameStreamAppends(t *testing.T) {
	s := setupDiskStore(t)
	assertConcurrentExecutionEventAppends(t, s)
}

func TestExecutionEventStoreConcurrentSameStreamAppendsMultiConnection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "multi-conn.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("set WAL mode: %v", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatalf("set busy timeout: %v", err)
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		t.Fatalf("set synchronous mode: %v", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatalf("set foreign keys: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertConcurrentExecutionEventAppends(t, NewStore(db, dbPath))
}

func assertConcurrentExecutionEventAppends(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	const count = 50
	seqs := make([]int64, count)
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range count {

		wg.Go(func() {
			appended, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
				Namespace:  "default",
				StreamType: store.ExecutionEventStreamTypeTask,
				StreamID:   "task-concurrent",
				TaskName:   "task-concurrent",
				Type:       events.ExecutionEventTypeToolCallStarted,
			})
			if err != nil {
				errs <- err
				return
			}
			seqs[i] = appended.Seq
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AppendExecutionEvent concurrent: %v", err)
		}
	}
	slices.Sort(seqs)
	for i, seq := range seqs {
		want := int64(i + 1)
		if seq != want {
			t.Fatalf("sorted seqs[%d] = %d, want %d; all=%v", i, seq, want, seqs)
		}
	}
	latest, err := s.GetLatestExecutionEventSeq(ctx, "default", store.ExecutionEventStreamTypeTask, "task-concurrent")
	if err != nil {
		t.Fatalf("GetLatestExecutionEventSeq: %v", err)
	}
	if latest != count {
		t.Fatalf("latest = %d, want %d", latest, count)
	}
}

func executionEventAuditSecrets() map[string]string {
	return map[string]string{
		"bearer": strings.Join([]string{"bearer", "value", "for", "redaction"}, "-"),
		"jwt": strings.Join([]string{
			"ey" + "JhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9",
			"ey" + "JzdWIiOiJ0YXNrIiwiYXVkIjoib3JrYSJ9",
			"signature" + strings.Repeat("a", 24),
		}, "."),
		"openai":    strings.Join([]string{"sk", strings.Repeat("a", 24)}, "-"),
		"cookie":    strings.Join([]string{"cookie", "value", "for", "redaction"}, "-"),
		"txn":       strings.Join([]string{"txn", "value", "for", "redaction"}, "-"),
		"github":    "github" + "_pat_" + strings.Repeat("a", 32),
		"anthropic": strings.Join([]string{"sk", "ant", "api03", strings.Repeat("a", 32)}, "-"),
	}
}

func TestMigrateBackfillsExecutionEventSessionCursors(t *testing.T) {
	const taskC = "task-c"
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE execution_events (
		id              TEXT PRIMARY KEY,
		namespace       TEXT NOT NULL,
		stream_type     TEXT NOT NULL,
		stream_id       TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		type            TEXT NOT NULL,
		severity        TEXT NOT NULL DEFAULT 'info',
		task_name       TEXT NOT NULL DEFAULT '',
		session_name    TEXT NOT NULL DEFAULT '',
		agent_name      TEXT NOT NULL DEFAULT '',
		tool_name       TEXT NOT NULL DEFAULT '',
		tool_call_id    TEXT NOT NULL DEFAULT '',
		summary         TEXT NOT NULL DEFAULT '',
		content_json    TEXT,
		content_text    TEXT NOT NULL DEFAULT '',
		truncation_json TEXT,
		created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(namespace, stream_type, stream_id, seq)
	)`)
	if err != nil {
		t.Fatalf("create old execution_events: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO execution_events(id, namespace, stream_type, stream_id, seq, type, severity, task_name, session_name)
		 VALUES ('default/task/task-a/1', 'default', 'task', 'task-a', 1, 'TaskStarted', 'info', 'task-a', 'session-1')`,
		`INSERT INTO execution_events(id, namespace, stream_type, stream_id, seq, type, severity, task_name, session_name)
		 VALUES ('default/task/task-b/1', 'default', 'task', 'task-b', 1, 'WorkerStarted', 'info', 'task-b', 'session-1')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("insert old event: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	migratedDB, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB(migrated) error = %v", err)
	}
	defer migratedDB.Close() //nolint:errcheck
	s := NewStore(migratedDB, path)
	ctx := context.Background()
	if err := s.DeleteExecutionEvents(ctx, "default", store.ExecutionEventStreamTypeTask, "task-a"); err != nil {
		t.Fatalf("DeleteExecutionEvents(task-a): %v", err)
	}
	if err := s.DeleteExecutionEvents(ctx, "default", store.ExecutionEventStreamTypeTask, "task-b"); err != nil {
		t.Fatalf("DeleteExecutionEvents(task-b): %v", err)
	}
	if _, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   "default",
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    taskC,
		TaskName:    taskC,
		SessionName: "session-1",
		Type:        events.ExecutionEventTypeTaskSucceeded,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(task-c): %v", err)
	}
	listed, latest, err := s.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
		Namespace: "default", SessionName: "session-1", AfterSeq: 2,
	})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 3 || len(listed) != 1 || listed[0].SessionSeq != 3 || listed[0].TaskName != taskC {
		t.Fatalf("latest=%d listed=%#v, want migrated cursor to continue at 3", latest, listed)
	}
}

func TestExecutionEventStoreRejectsDuplicateTerminalApproval(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    json.RawMessage
		toolCallID string
	}{
		{name: "approvalID", content: json.RawMessage(`{"approvalID":"approval-1"}`)},
		{name: "id", content: json.RawMessage(`{"id":"approval-1"}`)},
		{name: "toolCallID", content: json.RawMessage(`{}`), toolCallID: "approval-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupDiskStore(t)
			ctx := context.Background()
			approved := store.ExecutionEvent{
				Namespace:  "default",
				StreamType: store.ExecutionEventStreamTypeTask,
				StreamID:   "task",
				TaskName:   "task",
				Type:       events.ExecutionEventTypeApprovalApproved,
				ToolCallID: tc.toolCallID,
				Content:    tc.content,
			}
			if _, err := s.AppendExecutionEvent(ctx, &approved); err != nil {
				t.Fatalf("AppendExecutionEvent(approved) error = %v", err)
			}
			declined := approved
			declined.Type = events.ExecutionEventTypeApprovalDeclined
			if _, err := s.AppendExecutionEvent(ctx, &declined); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("AppendExecutionEvent(declined) error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestExecutionEventStoreListSessionExecutionEvents(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	for i, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
		{"task-a", events.ExecutionEventTypeTaskSucceeded},
	} {
		if _, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace:   "default",
			StreamType:  store.ExecutionEventStreamTypeTask,
			StreamID:    item.task,
			TaskName:    item.task,
			SessionName: "session-1",
			Type:        item.typ,
			CreatedAt:   now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}
	listed, latest, err := s.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{Namespace: "default", SessionName: "session-1", AfterSeq: 1, Limit: 10})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 3 || len(listed) != 2 {
		t.Fatalf("latest=%d listed=%#v, want latest 3 and two events", latest, listed)
	}
	if listed[0].SessionSeq != 2 || listed[0].TaskName != "task-b" || listed[0].TaskSeq != 1 {
		t.Fatalf("first = %#v, want task-b session seq 2 task seq 1", listed[0])
	}
}

func TestExecutionEventStoreListSessionCursorSurvivesTaskDeletion(t *testing.T) {
	const taskC = "task-c"
	s := setupDiskStore(t)
	ctx := context.Background()
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
	} {
		if _, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace:   "default",
			StreamType:  store.ExecutionEventStreamTypeTask,
			StreamID:    item.task,
			TaskName:    item.task,
			SessionName: "session-1",
			Type:        item.typ,
		}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}
	if err := s.DeleteExecutionEvents(ctx, "default", store.ExecutionEventStreamTypeTask, "task-a"); err != nil {
		t.Fatalf("DeleteExecutionEvents: %v", err)
	}
	if _, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   "default",
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    taskC,
		TaskName:    taskC,
		SessionName: "session-1",
		Type:        events.ExecutionEventTypeTaskSucceeded,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}

	listed, latest, err := s.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
		Namespace: "default", SessionName: "session-1", AfterSeq: 2,
	})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 3 || len(listed) != 1 || listed[0].SessionSeq != 3 || listed[0].TaskName != taskC {
		t.Fatalf("latest=%d listed=%#v, want stable cursor 3 for task-c", latest, listed)
	}
}

func TestExecutionEventStoreListSessionTypeFilterPreservesCursor(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
	} {
		if _, err := s.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: item.task,
			TaskName: item.task, SessionName: "session-1", Type: item.typ,
		}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}
	listed, latest, err := s.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
		Namespace: "default", SessionName: "session-1", EventTypes: []string{events.ExecutionEventTypeWorkerStarted},
	})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 2 || len(listed) != 1 || listed[0].SessionSeq != 2 {
		t.Fatalf("latest=%d listed=%#v, want latest 2 and preserved session seq 2", latest, listed)
	}
}
