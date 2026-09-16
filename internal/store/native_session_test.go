package store

import (
	"testing"
	"time"
)

func TestNativeSessionHistoryDigestBindsMessageIdentityAndContent(t *testing.T) {
	base := SessionMessage{
		ID: "message", Role: "assistant", Content: "response", Name: "tool", ToolCallID: "call",
		Input: map[string]any{"argument": "value"}, ToolCalls: []any{map[string]any{"id": "call"}},
		SourceType: "gateway-event", SourceRef: "event", Metadata: map[string]string{"senderId": "sender"},
	}
	want, err := NativeSessionHistoryDigest([]SessionMessage{base})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*SessionMessage)
	}{
		{name: "ID", mutate: func(m *SessionMessage) { m.ID = "replacement" }},
		{name: "role", mutate: func(m *SessionMessage) { m.Role = "user" }},
		{name: "content", mutate: func(m *SessionMessage) { m.Content = "changed" }},
		{name: "name", mutate: func(m *SessionMessage) { m.Name = "other" }},
		{name: "tool call ID", mutate: func(m *SessionMessage) { m.ToolCallID = "other" }},
		{name: "tool calls", mutate: func(m *SessionMessage) { m.ToolCalls = nil }},
		{name: "input", mutate: func(m *SessionMessage) { m.Input = nil }},
		{name: "provenance", mutate: func(m *SessionMessage) { m.Metadata = nil }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := base
			change.mutate(&changed)
			got, err := NativeSessionHistoryDigest([]SessionMessage{changed})
			if err != nil || got == want {
				t.Fatalf("digest did not bind changed %s: %v", change.name, err)
			}
		})
	}
	base.Timestamp, base.Order = time.Now(), 42
	got, err := NativeSessionHistoryDigest([]SessionMessage{base})
	if err != nil || got != want {
		t.Fatalf("timestamp or database order changed the digest: %v", err)
	}
	empty, err := NativeSessionHistoryDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	explicitEmpty, err := NativeSessionHistoryDigest([]SessionMessage{})
	if err != nil || empty != explicitEmpty {
		t.Fatalf("empty transcripts have different digests: %v", err)
	}
	if _, err := NativeSessionHistoryDigest([]SessionMessage{{ToolCalls: make(chan int)}}); err == nil {
		t.Fatal("unencodable transcript content was accepted")
	}
}
