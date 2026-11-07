package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

const fakeDedupeTaskName = "task-dedupe"

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

func TestExecutionEventStoreFakeAppendIfAbsent(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeExecutionEventStore()
	event := &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   fakeDedupeTaskName,
		TaskName:   fakeDedupeTaskName,
		Type:       events.ExecutionEventTypeTaskStarted,
	}

	first, appended, err := fake.AppendExecutionEventIfAbsent(ctx, event, "event-key")
	if err != nil || !appended || first == nil {
		t.Fatalf("first append = %#v appended=%t err=%v", first, appended, err)
	}
	duplicate, appended, err := fake.AppendExecutionEventIfAbsent(ctx, event, "event-key")
	if err != nil || appended || duplicate == nil || duplicate.ID != first.ID || duplicate.Seq != first.Seq {
		t.Fatalf("duplicate append = %#v appended=%t err=%v, want existing %#v", duplicate, appended, err, first)
	}

	listed, err := fake.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: fakeDedupeTaskName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("deduplicated events = %#v, want one event", listed)
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
