package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSessionCheckpointValidation(t *testing.T) {
	valid := func() SessionCheckpoint {
		return SessionCheckpoint{
			ID: "checkpoint-a", Namespace: "tenant-a", SessionName: "session-a",
			Version: SessionCheckpointVersion, LastMessageID: "message-a",
			Note:             "Keep the public API unchanged. The parser passed. Next inspect retries.",
			SourceMessageIDs: []string{"message-a"},
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid checkpoint: %v", err)
	}
	tests := []struct {
		name   string
		change func(*SessionCheckpoint)
	}{
		{"missing identity", func(c *SessionCheckpoint) { c.ID = "" }},
		{"missing Session", func(c *SessionCheckpoint) { c.SessionName = "" }},
		{"unsupported version", func(c *SessionCheckpoint) { c.Version++ }},
		{"empty note", func(c *SessionCheckpoint) { c.Note = " \n" }},
		{"oversized note", func(c *SessionCheckpoint) { c.Note = strings.Repeat("a", MaxSessionCheckpointBytes+1) }},
		{"invalid UTF-8", func(c *SessionCheckpoint) { c.Note = string([]byte{0xff}) }},
		{"credential-shaped note", func(c *SessionCheckpoint) { c.Note = "api_key=" + strings.Repeat("test-fixture", 4) }},
		{"duplicate sources", func(c *SessionCheckpoint) { c.SourceMessageIDs = []string{"message-a", "message-a"} }},
		{"missing source ID", func(c *SessionCheckpoint) { c.SourceMessageIDs = []string{""} }},
		{"negative order", func(c *SessionCheckpoint) { c.LastMessageOrder = -1 }},
		{"too many sources", func(c *SessionCheckpoint) {
			c.SourceMessageIDs = make([]string, MaxSessionCheckpointSources+1)
			for i := range c.SourceMessageIDs {
				c.SourceMessageIDs[i] = fmt.Sprintf("message-%d", i)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := valid()
			test.change(&checkpoint)
			if err := checkpoint.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() error = %v, want ErrValidation", err)
			}
		})
	}
	checkpoint := valid()
	checkpoint.Note = strings.Repeat("a", MaxSessionCheckpointBytes)
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("note at byte limit: %v", err)
	}
}

func TestSessionContextWriteRequiresExactIdentity(t *testing.T) {
	valid := SessionContextWrite{Namespace: "tenant-a", SessionName: "session-a", OwnerName: "task-a", OwnerUID: "uid-a"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SessionContextWrite){
		func(w *SessionContextWrite) { w.Namespace = "" },
		func(w *SessionContextWrite) { w.SessionName = " session-a" },
		func(w *SessionContextWrite) { w.OwnerName = "" },
		func(w *SessionContextWrite) { w.OwnerUID = "" },
		func(w *SessionContextWrite) { w.ThroughMessageID = "message\n" },
	} {
		write := valid
		mutate(&write)
		if err := write.Validate(); !errors.Is(err, ErrValidation) {
			t.Fatalf("Validate() error = %v, want ErrValidation", err)
		}
	}
}

func TestSessionHistoryReadValidation(t *testing.T) {
	valid := SessionHistoryRead{Namespace: "tenant-a", SessionName: "session-a", MessageID: "message-a", Limit: MaxSessionHistoryReadBytes}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SessionHistoryRead){
		func(r *SessionHistoryRead) { r.Namespace = "" },
		func(r *SessionHistoryRead) { r.SessionName = "" },
		func(r *SessionHistoryRead) { r.MessageID = "" },
		func(r *SessionHistoryRead) { r.ThroughMessageID = " " },
		func(r *SessionHistoryRead) { r.Limit = 0 },
		func(r *SessionHistoryRead) { r.Limit++ },
		func(r *SessionHistoryRead) { r.Offset = -1 },
	} {
		read := valid
		mutate(&read)
		if err := read.Validate(); !errors.Is(err, ErrValidation) {
			t.Fatalf("Validate() error = %v, want ErrValidation", err)
		}
	}
}
