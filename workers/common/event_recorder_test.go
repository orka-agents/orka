package common

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
)

func TestEventRecorderFakeCapturesEventsInOrder(t *testing.T) {
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	recorder := NewFakeEventRecorderWithClock(func() time.Time { return now })
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskStarted, WithEventTaskName("task-1"))
	recorder.Record(
		context.Background(),
		events.ExecutionEventTypeToolCallCompleted,
		WithEventToolName("file_read"),
		WithEventToolCallID("call-1"),
	)

	gotTypes := recorder.EventTypes()
	wantTypes := []string{events.ExecutionEventTypeTaskStarted, events.ExecutionEventTypeToolCallCompleted}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("EventTypes() = %#v, want %#v", gotTypes, wantTypes)
	}
	captured := recorder.Events()
	if len(captured) != 2 {
		t.Fatalf("Events() length = %d, want 2", len(captured))
	}
	if captured[0].TaskName != "task-1" || captured[1].ToolName != "file_read" || captured[1].ToolCallID != "call-1" {
		t.Fatalf("captured events = %#v", captured)
	}
	if !captured[0].CreatedAt.Equal(now) || !captured[1].CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt values = %#v", captured)
	}
}

func TestEventRecorderNoopAllowsNilAndEmptyOptions(t *testing.T) {
	recorder := NoopEventRecorder{}
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskCreated)
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskCreated, nil, WithEventSeverity("warning"))
}

func TestEventRecorderFakeSanitizesPayloadOptions(t *testing.T) {
	recorder := NewFakeEventRecorder()
	bearerValue := testDashToken("bearer")
	apiKey := testOpenAIKey()
	recorder.Record(context.Background(), events.ExecutionEventTypeModelMessage,
		WithEventSeverity("ERROR"),
		WithEventSummary("Authorization: Bearer "+bearerValue),
		WithEventContent(mustRawJSON(t, map[string]any{"apiKey": apiKey, "safe": "ok"})),
		WithEventContentText(strings.Repeat("x", events.MaxExecutionEventContentTextChars+10)),
	)
	captured := recorder.Events()
	if len(captured) != 1 {
		t.Fatalf("Events() length = %d, want 1", len(captured))
	}
	event := captured[0]
	if event.Severity != events.ExecutionEventSeverityError {
		t.Fatalf("Severity = %q, want error", event.Severity)
	}
	if strings.Contains(event.Summary, bearerValue) || strings.Contains(string(event.Content), apiKey) {
		t.Fatalf("captured event leaked secret: %#v content=%s", event, event.Content)
	}
	if event.Truncation == nil || !event.Truncation.ContentTextTruncated {
		t.Fatalf("Truncation = %#v, want contentText truncated", event.Truncation)
	}
}

func TestFakeEventRecorderReset(t *testing.T) {
	recorder := NewFakeEventRecorder()
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskStarted)
	recorder.Reset()
	if got := len(recorder.Events()); got != 0 {
		t.Fatalf("Events() length after reset = %d, want 0", got)
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw JSON: %v", err)
	}
	return json.RawMessage(data)
}

func testDashToken(prefix string) string {
	return strings.Join([]string{prefix, "value", "for", "redaction"}, "-")
}

func testOpenAIKey() string {
	return strings.Join([]string{"sk", "test12345678901234567890"}, "-")
}
