/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"strings"
	"testing"
)

func TestNewSSEReader(t *testing.T) {
	r := NewSSEReader(strings.NewReader(""))
	if r == nil {
		t.Fatal("expected non-nil reader")
	}
}

func TestSSEReader_Next(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantEvents []SSEEvent
	}{
		{
			name:  "single event with event type",
			input: "event: message\ndata: {\"content\":\"hello\"}\n\n",
			wantEvents: []SSEEvent{
				{Event: "message", Data: `{"content":"hello"}`},
			},
		},
		{
			name:  "data only event",
			input: "data: plain text\n\n",
			wantEvents: []SSEEvent{
				{Data: "plain text"},
			},
		},
		{
			name:  "multiple events",
			input: "event: message\ndata: first\n\nevent: done\ndata: second\n\n",
			wantEvents: []SSEEvent{
				{Event: "message", Data: "first"},
				{Event: "done", Data: "second"},
			},
		},
		{
			name:       "empty input",
			input:      "",
			wantEvents: nil,
		},
		{
			name:  "blank line resets event type",
			input: "event: status\n\nevent: message\ndata: after reset\n\n",
			wantEvents: []SSEEvent{
				{Event: "message", Data: "after reset"},
			},
		},
		{
			name:       "event without data is not emitted",
			input:      "event: status\n\n",
			wantEvents: nil,
		},
		{
			name:  "data without event type",
			input: "data: no-event\n\n",
			wantEvents: []SSEEvent{
				{Data: "no-event"},
			},
		},
		{
			name:  "non-sse lines are ignored",
			input: "comment line\nid: 123\nretry: 5000\ndata: valid\n\n",
			wantEvents: []SSEEvent{
				{Data: "valid"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewSSEReader(strings.NewReader(tt.input))

			var got []SSEEvent
			for {
				evt, ok := r.Next()
				if !ok {
					break
				}
				got = append(got, evt)
			}

			if len(got) != len(tt.wantEvents) {
				t.Fatalf("got %d events, want %d", len(got), len(tt.wantEvents))
			}
			for i, want := range tt.wantEvents {
				if got[i].Event != want.Event {
					t.Errorf("event[%d].Event = %q, want %q", i, got[i].Event, want.Event)
				}
				if got[i].Data != want.Data {
					t.Errorf("event[%d].Data = %q, want %q", i, got[i].Data, want.Data)
				}
			}
		})
	}
}

func TestSSEReader_Err(t *testing.T) {
	r := NewSSEReader(strings.NewReader("data: hello\n"))
	// Drain events
	for {
		_, ok := r.Next()
		if !ok {
			break
		}
	}
	if err := r.Err(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSSEReader_LargeData(t *testing.T) {
	// Test that the reader can handle large data lines (buffer is configured for 1MB)
	largeData := strings.Repeat("x", 100000)
	input := "data: " + largeData + "\n\n"
	r := NewSSEReader(strings.NewReader(input))

	evt, ok := r.Next()
	if !ok {
		t.Fatal("expected event")
	}
	if evt.Data != largeData {
		t.Errorf("data length = %d, want %d", len(evt.Data), len(largeData))
	}
}

func TestSSEReader_ConsecutiveEvents(t *testing.T) {
	input := "event: a\ndata: 1\nevent: b\ndata: 2\n"
	r := NewSSEReader(strings.NewReader(input))

	evt1, ok := r.Next()
	if !ok {
		t.Fatal("expected first event")
	}
	if evt1.Event != "a" || evt1.Data != "1" {
		t.Errorf("first event = %+v, want {Event:a Data:1}", evt1)
	}

	evt2, ok := r.Next()
	if !ok {
		t.Fatal("expected second event")
	}
	if evt2.Event != "b" || evt2.Data != "2" {
		t.Errorf("second event = %+v, want {Event:b Data:2}", evt2)
	}

	_, ok = r.Next()
	if ok {
		t.Error("expected no more events")
	}
}
