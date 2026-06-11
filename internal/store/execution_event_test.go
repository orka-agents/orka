package store

import (
	"context"
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
		t.Fatalf("Validate() accepted unsupported session stream type")
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
