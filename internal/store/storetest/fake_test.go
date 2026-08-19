package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventStoreFakeAppendsMonotonicSeqPerStream(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	fake := NewFakeExecutionEventStoreWithClock(func() time.Time { return now })

	first, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		TaskName:   "task-a",
		Type:       events.ExecutionEventTypeTaskCreated,
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent(first) error = %v", err)
	}
	second, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		TaskName:   "task-a",
		Type:       events.ExecutionEventTypeTaskSucceeded,
		Severity:   "WARNING",
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent(second) error = %v", err)
	}
	other, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-b",
		TaskName:   "task-b",
		Type:       events.ExecutionEventTypeTaskCreated,
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent(other) error = %v", err)
	}

	if first.Seq != 1 || second.Seq != 2 || other.Seq != 1 {
		t.Fatalf("seqs = %d, %d, %d; want 1, 2, 1", first.Seq, second.Seq, other.Seq)
	}
	if second.Severity != events.ExecutionEventSeverityWarning {
		t.Fatalf("Severity = %q, want warning", second.Severity)
	}
	latest, err := fake.GetLatestExecutionEventSeq(ctx, "default", store.ExecutionEventStreamTypeTask, "task-a")
	if err != nil {
		t.Fatalf("GetLatestExecutionEventSeq() error = %v", err)
	}
	if latest != 2 {
		t.Fatalf("latest = %d, want 2", latest)
	}

	listed, err := fake.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		AfterSeq:   1,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Seq != 2 || listed[0].Type != events.ExecutionEventTypeTaskSucceeded {
		t.Fatalf("listed = %#v, want only seq 2 succeeded event", listed)
	}
}

func TestExecutionEventStoreFakeAggregatesSessionEvents(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	fake := NewFakeExecutionEventStoreWithClock(clock)
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
		{"task-a", events.ExecutionEventTypeTaskSucceeded},
	} {
		if _, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
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

	listed, latest, err := fake.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{Namespace: "default", SessionName: "session-1", AfterSeq: 1})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 3 || len(listed) != 2 {
		t.Fatalf("latest=%d listed=%#v, want latest 3 and two events", latest, listed)
	}
	if listed[0].SessionSeq != 2 || listed[0].TaskName != "task-b" || listed[0].TaskSeq != 1 {
		t.Fatalf("first session event = %#v, want task-b session seq 2 task seq 1", listed[0])
	}
}

func TestExecutionEventStoreFakeSessionTypeFilterPreservesCursor(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeExecutionEventStore()
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
	} {
		if _, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: item.task,
			TaskName: item.task, SessionName: "session-1", Type: item.typ,
		}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}
	listed, latest, err := fake.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
		Namespace: "default", SessionName: "session-1", EventTypes: []string{events.ExecutionEventTypeWorkerStarted},
	})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 2 || len(listed) != 1 || listed[0].SessionSeq != 2 {
		t.Fatalf("latest=%d listed=%#v, want latest 2 and preserved session seq 2", latest, listed)
	}
}

func TestExecutionEventStoreFakeSessionCursorSurvivesTaskDeletion(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeExecutionEventStore()
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
	} {
		if _, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
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
	if err := fake.DeleteExecutionEvents(ctx, "default", store.ExecutionEventStreamTypeTask, "task-a"); err != nil {
		t.Fatalf("DeleteExecutionEvents: %v", err)
	}
	if _, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   "default",
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    "task-c",
		TaskName:    "task-c",
		SessionName: "session-1",
		Type:        events.ExecutionEventTypeTaskSucceeded,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}

	listed, latest, err := fake.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
		Namespace: "default", SessionName: "session-1", AfterSeq: 2,
	})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 3 || len(listed) != 1 || listed[0].SessionSeq != 3 || listed[0].TaskName != "task-c" {
		t.Fatalf("latest=%d listed=%#v, want stable cursor 3 for task-c", latest, listed)
	}
}

func TestExecutionEventStoreFakeRejectsDuplicateTerminalApproval(t *testing.T) {
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
			ctx := context.Background()
			fake := NewFakeExecutionEventStore()
			approved := store.ExecutionEvent{
				Namespace:  "default",
				StreamType: store.ExecutionEventStreamTypeTask,
				StreamID:   "task",
				TaskName:   "task",
				Type:       events.ExecutionEventTypeApprovalApproved,
				ToolCallID: tc.toolCallID,
				Content:    tc.content,
			}
			if _, err := fake.AppendExecutionEvent(ctx, &approved); err != nil {
				t.Fatalf("AppendExecutionEvent(approved) error = %v", err)
			}
			declined := approved
			declined.Type = events.ExecutionEventTypeApprovalDeclined
			if _, err := fake.AppendExecutionEvent(ctx, &declined); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("AppendExecutionEvent(declined) error = %v, want store.ErrConflict", err)
			}
		})
	}
}

func TestExecutionEventStoreFakeValidationAndDelete(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeExecutionEventStore()
	if _, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{Namespace: "default", StreamID: "task", Type: "Nope"}); err == nil {
		t.Fatalf("AppendExecutionEvent() accepted invalid type")
	} else if !errors.Is(err, store.ErrValidation) {
		t.Fatalf("AppendExecutionEvent() error = %v, want store.ErrValidation", err)
	}
	if _, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{Namespace: "default", StreamID: "task", Type: events.ExecutionEventTypeTaskCreated}); err != nil {
		t.Fatalf("AppendExecutionEvent(valid) error = %v", err)
	}
	if err := fake.DeleteExecutionEvents(ctx, "default", store.ExecutionEventStreamTypeTask, "task"); err != nil {
		t.Fatalf("DeleteExecutionEvents() error = %v", err)
	}
	latest, err := fake.GetLatestExecutionEventSeq(ctx, "default", store.ExecutionEventStreamTypeTask, "task")
	if err != nil {
		t.Fatalf("GetLatestExecutionEventSeq() error = %v", err)
	}
	if latest != 0 {
		t.Fatalf("latest after delete = %d, want 0", latest)
	}
}

func TestAppendExecutionEventRecordsRedactionMetricExactlyOnce(t *testing.T) {
	ctx := context.Background()

	// Worker/API path: the payload is pre-sanitized in a different process before
	// submission, so by the time the store sanitizes again the content already
	// carries the redaction marker and does not change. The metric must still
	// count this persisted event once (regression: it previously recorded 0).
	t.Run("worker pre-redacted counts once", func(t *testing.T) {
		metrics.ExecutionEventRedactionsTotal.Reset()
		fake := NewFakeExecutionEventStore()
		// Simulate the worker having already redacted: content contains the marker
		// and the API DTO sanitize would not change it further.
		_, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace:   "default",
			StreamType:  store.ExecutionEventStreamTypeTask,
			StreamID:    "task",
			Type:        events.ExecutionEventTypeModelMessage,
			ContentText: "api key " + events.ExecutionEventRedactedValue,
		})
		if err != nil {
			t.Fatalf("AppendExecutionEvent() error = %v", err)
		}
		if got := metrics.CounterVecValue(metrics.ExecutionEventRedactionsTotal, store.ExecutionEventStreamTypeTask, events.ExecutionEventTypeModelMessage); got != 1 {
			t.Fatalf("redactions counter = %v, want 1 (pre-redacted worker payload must count once)", got)
		}
	})

	// Harness path: store.SanitizeExecutionEventPayloadFields runs in the mapper AND
	// again in the store. The metric must count the persisted event once, not
	// twice (regression: it previously recorded 2).
	t.Run("double sanitize counts once", func(t *testing.T) {
		metrics.ExecutionEventRedactionsTotal.Reset()
		fake := NewFakeExecutionEventStore()
		event := &store.ExecutionEvent{
			Namespace:   "default",
			StreamType:  store.ExecutionEventStreamTypeTask,
			StreamID:    "task",
			Type:        events.ExecutionEventTypeModelMessage,
			ContentText: "authorization: Bearer sk-secret-value-1234567890",
		}
		// Mapper-style first pass before the store appends (store sanitizes again).
		if err := store.SanitizeExecutionEventPayloadFields(event); err != nil {
			t.Fatalf("store.SanitizeExecutionEventPayloadFields() error = %v", err)
		}
		if _, err := fake.AppendExecutionEvent(ctx, event); err != nil {
			t.Fatalf("AppendExecutionEvent() error = %v", err)
		}
		if got := metrics.CounterVecValue(metrics.ExecutionEventRedactionsTotal, store.ExecutionEventStreamTypeTask, events.ExecutionEventTypeModelMessage); got != 1 {
			t.Fatalf("redactions counter = %v, want 1 (double sanitize must not double-count)", got)
		}
	})

	// Clean payload: no redaction marker -> no count.
	t.Run("clean payload counts zero", func(t *testing.T) {
		metrics.ExecutionEventRedactionsTotal.Reset()
		fake := NewFakeExecutionEventStore()
		if _, err := fake.AppendExecutionEvent(ctx, &store.ExecutionEvent{
			Namespace:   "default",
			StreamType:  store.ExecutionEventStreamTypeTask,
			StreamID:    "task",
			Type:        events.ExecutionEventTypeModelMessage,
			ContentText: "nothing sensitive here",
		}); err != nil {
			t.Fatalf("AppendExecutionEvent() error = %v", err)
		}
		if got := metrics.CounterVecValue(metrics.ExecutionEventRedactionsTotal, store.ExecutionEventStreamTypeTask, events.ExecutionEventTypeModelMessage); got != 0 {
			t.Fatalf("redactions counter = %v, want 0", got)
		}
	})
}
