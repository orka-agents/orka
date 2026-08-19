package store

import (
	"testing"

	"github.com/orka-agents/orka/internal/events"
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
