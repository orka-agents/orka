package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
)

func TestMapFrameToExecutionEventCoversFrozenFrameTypes(t *testing.T) {
	ctx := EventMapContext{Namespace: "default", TaskName: "task-a", SessionName: "session-a", AgentName: "agent-a"}
	tests := map[FrameType]string{
		FrameTurnStarted:        events.ExecutionEventTypeAgentRuntimeStarted,
		FrameRuntimeOutput:      events.ExecutionEventTypeModelMessage,
		FrameToolCallRequested:  events.ExecutionEventTypeToolCallStarted,
		FrameToolResultReceived: events.ExecutionEventTypeToolCallCompleted,
		FrameApprovalRequested:  events.ExecutionEventTypeAgentRuntimeCommandStarted,
		FrameTurnCompleted:      events.ExecutionEventTypeAgentRuntimeCompleted,
		FrameTurnFailed:         events.ExecutionEventTypeAgentRuntimeFailed,
		FrameTurnCancelled:      events.ExecutionEventTypeAgentRuntimeCancelled,
		FrameRuntimeLog:         events.ExecutionEventTypeAgentRuntimeCommandStarted,
	}
	for typ, want := range tests {
		t.Run(string(typ), func(t *testing.T) {
			frame := validFrame(typ)
			event, err := MapFrameToExecutionEvent(frame, ctx)
			if err != nil {
				t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
			}
			if event.Type != want {
				t.Fatalf("event.Type = %s, want %s", event.Type, want)
			}
			if event.Namespace != ctx.Namespace || event.TaskName != ctx.TaskName || event.SessionName != ctx.SessionName {
				t.Fatalf("event ownership = %#v, want context ownership", event)
			}
			var content map[string]any
			if err := json.Unmarshal(event.Content, &content); err != nil {
				t.Fatalf("event content unmarshal: %v", err)
			}
			harnessContent, ok := content["harness"].(map[string]any)
			if !ok || harnessContent["turnID"] != string(frame.TurnID) || harnessContent["correlationID"] != frame.CorrelationID {
				t.Fatalf("event content = %#v, want harness metadata", content)
			}
		})
	}
}

func TestMapFrameToExecutionEventDoesNotPromoteRuntimeApprovalFrame(t *testing.T) {
	frame := validFrame(FrameApprovalRequested)
	frame.ApprovalID = "runtime-made-approval"
	frame.ToolName = "dispatch_work_order"
	frame.ToolCallID = "call-1"
	event, err := MapFrameToExecutionEvent(frame, EventMapContext{Namespace: "default", TaskName: "task-a"})
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
	}
	if event.Type == events.ExecutionEventTypeApprovalRequested {
		t.Fatalf("runtime-originated approval frame became canonical approval event")
	}
	if event.Type != events.ExecutionEventTypeAgentRuntimeCommandStarted || event.Severity != events.ExecutionEventSeverityWarning {
		t.Fatalf("event = %#v, want warning runtime diagnostic", event)
	}
}

func TestMapFrameToExecutionEventUnknownFrameIsSafeDiagnostic(t *testing.T) {
	frame := validFrame(FrameType("NotAFrame"))
	event, err := MapFrameToExecutionEvent(frame, EventMapContext{Namespace: "default", TaskName: "task-a"})
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
	}
	if event.Type != events.ExecutionEventTypeAgentRuntimeCommandStarted || event.Severity != events.ExecutionEventSeverityWarning {
		t.Fatalf("event = %#v, want warning diagnostic", event)
	}
	if !strings.Contains(event.Summary, "unknown harness frame") {
		t.Fatalf("event.Summary = %q, want unknown frame diagnostic", event.Summary)
	}
}

func TestMapFrameToExecutionEventRedactsBeforeStoreAppend(t *testing.T) {
	secret := "Authorization: Bearer bearer-value-for-redaction api_key=sk-test12345678901234567890"
	frame := validFrame(FrameRuntimeOutput)
	frame.Summary = secret
	frame.ContentText = secret
	frame.Content = json.RawMessage(`{"message":"` + secret + `"}`)
	event, err := MapFrameToExecutionEvent(frame, EventMapContext{Namespace: "default", TaskName: "task-a"})
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
	}
	encoded := event.Summary + event.ContentText + string(event.Content)
	if strings.Contains(encoded, "sk-test") || strings.Contains(encoded, "bearer-value-for-redaction") {
		t.Fatalf("mapped event leaked secret: %s", encoded)
	}
	if !strings.Contains(encoded, events.ExecutionEventRedactedValue) {
		t.Fatalf("mapped event = %s, want redaction marker", encoded)
	}
}

func validFrame(typ FrameType) HarnessEventFrame {
	frame := HarnessEventFrame{
		Version:          ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Seq:              1,
		CreatedAt:        time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Severity:         events.ExecutionEventSeverityInfo,
		Content:          json.RawMessage(`{"ok":true}`),
	}
	if typ == FrameTurnCompleted {
		frame.Completed = &TurnCompleted{Result: "ok", FinalEventSeq: 1}
	}
	if typ == FrameTurnFailed {
		frame.Failed = &TurnFailed{Reason: "failed", Message: "failed"}
	}
	if typ == FrameToolCallRequested || typ == FrameToolResultReceived {
		frame.ToolName = "echo"
		frame.ToolCallID = "tool-1"
	}
	return frame
}
