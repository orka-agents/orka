package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
)

func TestExecutionEventFilterNormalizationDefaultsLimit(t *testing.T) {
	filter := ExecutionEventFilter{
		Namespace:  " default ",
		StreamID:   " task-1 ",
		TaskName:   " task-1 ",
		EventTypes: []string{" " + events.ExecutionEventTypeTaskCreated + " ", ""},
		AfterSeq:   -10,
	}.Normalized()
	if filter.Namespace != "default" {
		t.Fatalf("Namespace = %q, want default", filter.Namespace)
	}
	if filter.StreamType != ExecutionEventStreamTypeTask {
		t.Fatalf("StreamType = %q, want task", filter.StreamType)
	}
	if filter.StreamID != "task-1" || filter.TaskName != "task-1" {
		t.Fatalf("filter names not trimmed: %#v", filter)
	}
	if filter.Limit != DefaultExecutionEventLimit {
		t.Fatalf("Limit = %d, want %d", filter.Limit, DefaultExecutionEventLimit)
	}
	if filter.AfterSeq != 0 {
		t.Fatalf("AfterSeq = %d, want 0", filter.AfterSeq)
	}
	if len(filter.EventTypes) != 1 || filter.EventTypes[0] != events.ExecutionEventTypeTaskCreated {
		t.Fatalf("EventTypes = %#v", filter.EventTypes)
	}

	filter = ExecutionEventFilter{Limit: MaxExecutionEventLimit + 1}.Normalized()
	if filter.Limit != MaxExecutionEventLimit {
		t.Fatalf("Limit = %d, want capped to %d", filter.Limit, MaxExecutionEventLimit)
	}
}

func TestExecutionEventFilterValidateRejectsUnsupportedValues(t *testing.T) {
	if err := (ExecutionEventFilter{StreamType: "session"}).Validate(); err == nil {
		t.Fatalf("Validate() accepted unsupported direct session stream type")
	}
	if err := (ExecutionEventFilter{EventTypes: []string{"Nope"}}).Validate(); err == nil {
		t.Fatalf("Validate() accepted unsupported event type")
	}
}

func TestExecutionEventStoreFakeAppendsMonotonicSeqPerStream(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	store := NewFakeExecutionEventStoreWithClock(func() time.Time { return now })

	first, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{
		Namespace:  "default",
		StreamType: ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		TaskName:   "task-a",
		Type:       events.ExecutionEventTypeTaskCreated,
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent(first) error = %v", err)
	}
	second, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{
		Namespace:  "default",
		StreamType: ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		TaskName:   "task-a",
		Type:       events.ExecutionEventTypeTaskSucceeded,
		Severity:   "WARNING",
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent(second) error = %v", err)
	}
	other, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{
		Namespace:  "default",
		StreamType: ExecutionEventStreamTypeTask,
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
	latest, err := store.GetLatestExecutionEventSeq(ctx, "default", ExecutionEventStreamTypeTask, "task-a")
	if err != nil {
		t.Fatalf("GetLatestExecutionEventSeq() error = %v", err)
	}
	if latest != 2 {
		t.Fatalf("latest = %d, want 2", latest)
	}

	listed, err := store.ListExecutionEvents(ctx, ExecutionEventFilter{
		Namespace:  "default",
		StreamType: ExecutionEventStreamTypeTask,
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
	store := NewFakeExecutionEventStoreWithClock(clock)
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
		{"task-a", events.ExecutionEventTypeTaskSucceeded},
	} {
		if _, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{
			Namespace:   "default",
			StreamType:  ExecutionEventStreamTypeTask,
			StreamID:    item.task,
			TaskName:    item.task,
			SessionName: "session-1",
			Type:        item.typ,
		}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}

	listed, latest, err := store.ListSessionExecutionEvents(ctx, SessionExecutionEventFilter{Namespace: "default", SessionName: "session-1", AfterSeq: 1})
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
	store := NewFakeExecutionEventStore()
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
	} {
		if _, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{
			Namespace: "default", StreamType: ExecutionEventStreamTypeTask, StreamID: item.task,
			TaskName: item.task, SessionName: "session-1", Type: item.typ,
		}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}
	listed, latest, err := store.ListSessionExecutionEvents(ctx, SessionExecutionEventFilter{
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
	store := NewFakeExecutionEventStore()
	for _, item := range []struct {
		task string
		typ  string
	}{
		{"task-a", events.ExecutionEventTypeTaskStarted},
		{"task-b", events.ExecutionEventTypeWorkerStarted},
	} {
		if _, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{
			Namespace:   "default",
			StreamType:  ExecutionEventStreamTypeTask,
			StreamID:    item.task,
			TaskName:    item.task,
			SessionName: "session-1",
			Type:        item.typ,
		}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}
	if err := store.DeleteExecutionEvents(ctx, "default", ExecutionEventStreamTypeTask, "task-a"); err != nil {
		t.Fatalf("DeleteExecutionEvents: %v", err)
	}
	if _, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{
		Namespace:   "default",
		StreamType:  ExecutionEventStreamTypeTask,
		StreamID:    "task-c",
		TaskName:    "task-c",
		SessionName: "session-1",
		Type:        events.ExecutionEventTypeTaskSucceeded,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}

	listed, latest, err := store.ListSessionExecutionEvents(ctx, SessionExecutionEventFilter{
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
			store := NewFakeExecutionEventStore()
			approved := ExecutionEvent{
				Namespace:  "default",
				StreamType: ExecutionEventStreamTypeTask,
				StreamID:   "task",
				TaskName:   "task",
				Type:       events.ExecutionEventTypeApprovalApproved,
				ToolCallID: tc.toolCallID,
				Content:    tc.content,
			}
			if _, err := store.AppendExecutionEvent(ctx, &approved); err != nil {
				t.Fatalf("AppendExecutionEvent(approved) error = %v", err)
			}
			declined := approved
			declined.Type = events.ExecutionEventTypeApprovalDeclined
			if _, err := store.AppendExecutionEvent(ctx, &declined); !errors.Is(err, ErrConflict) {
				t.Fatalf("AppendExecutionEvent(declined) error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestExecutionEventStoreFakeValidationAndDelete(t *testing.T) {
	ctx := context.Background()
	store := NewFakeExecutionEventStore()
	if _, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{Namespace: "default", StreamID: "task", Type: "Nope"}); err == nil {
		t.Fatalf("AppendExecutionEvent() accepted invalid type")
	} else if !errors.Is(err, ErrValidation) {
		t.Fatalf("AppendExecutionEvent() error = %v, want ErrValidation", err)
	}
	if _, err := store.AppendExecutionEvent(ctx, &ExecutionEvent{Namespace: "default", StreamID: "task", Type: events.ExecutionEventTypeTaskCreated}); err != nil {
		t.Fatalf("AppendExecutionEvent(valid) error = %v", err)
	}
	if err := store.DeleteExecutionEvents(ctx, "default", ExecutionEventStreamTypeTask, "task"); err != nil {
		t.Fatalf("DeleteExecutionEvents() error = %v", err)
	}
	latest, err := store.GetLatestExecutionEventSeq(ctx, "default", ExecutionEventStreamTypeTask, "task")
	if err != nil {
		t.Fatalf("GetLatestExecutionEventSeq() error = %v", err)
	}
	if latest != 0 {
		t.Fatalf("latest after delete = %d, want 0", latest)
	}
}
