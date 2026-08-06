package harness

import (
	"encoding/json"
	"testing"
)

func TestMappedFrameIdentityRoundTripsMappedEventContent(t *testing.T) {
	frame := validFrame(FrameRuntimeOutput)
	frame.RuntimeSessionID = "runtime-1"
	frame.TurnID = "turn-1"
	frame.CorrelationID = "corr-1"
	frame.Seq = 42

	event, err := MapFrameToExecutionEvent(frame, EventMapContext{Namespace: "default", TaskName: "task-a"})
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
	}
	identity, ok := MappedFrameIdentityFromEvent(*event)
	if !ok {
		t.Fatalf("MappedFrameIdentityFromEvent() did not find identity in %s", event.Content)
	}
	if identity.Version != ProtocolVersion || identity.FrameType != frame.Type {
		t.Fatalf("identity protocol fields = %#v, want version %q frame type %q", identity, ProtocolVersion, frame.Type)
	}
	if identity.RuntimeSessionID != frame.RuntimeSessionID || identity.TurnID != frame.TurnID || identity.CorrelationID != frame.CorrelationID || identity.Seq != frame.Seq {
		t.Fatalf("identity = %#v, want frame identity", identity)
	}
	if got, want := identity.Key(), MappedFrameKey(frame); got != want {
		t.Fatalf("identity key = %q, want %q", got, want)
	}
}

func TestMappedFrameIdentityFromContentAcceptsLegacyIdentity(t *testing.T) {
	raw := json.RawMessage(`{"harness":{"runtimeSessionID":"runtime-legacy","turnID":"turn-legacy","correlationID":"corr-legacy","seq":7}}`)
	identity, ok := MappedFrameIdentityFromContent(raw)
	if !ok {
		t.Fatalf("MappedFrameIdentityFromContent() did not accept legacy identity")
	}
	if identity.FrameType != "" || identity.Version != "" {
		t.Fatalf("legacy identity protocol fields = %#v, want empty", identity)
	}
	if got, want := identity.Key(), "runtime-legacy\x00turn-legacy\x00corr-legacy\x007"; got != want {
		t.Fatalf("legacy identity key = %q, want %q", got, want)
	}
	if !identity.HasTurnID(" turn-legacy ") {
		t.Fatalf("HasTurnID did not trim turn IDs")
	}
}

func TestMappedFrameIdentityFromContentIgnoresNonHarnessContent(t *testing.T) {
	tests := map[string]json.RawMessage{
		"empty":       nil,
		"malformed":   json.RawMessage(`{"harness":`),
		"missing":     json.RawMessage(`{"message":"not a harness event"}`),
		"emptyObject": json.RawMessage(`{"harness":{}}`),
		"zeroSeq":     json.RawMessage(`{"harness":{"runtimeSessionID":"runtime-a","turnID":"turn-a","correlationID":"corr-a","seq":0}}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if identity, ok := MappedFrameIdentityFromContent(raw); ok {
				t.Fatalf("MappedFrameIdentityFromContent() = %#v, true; want false", identity)
			}
		})
	}
}
