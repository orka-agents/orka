package eventjournal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store/storetest"
)

func TestJournalAcceptsLiveUpdatesWithoutPersistingThem(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	journal := Journal{EventStore: eventStore, MapContext: testMapContext()}
	state, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	thought := testUpdateEvent(2, now, harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateAssistantThoughtChunk, AssistantThought: &harnessv2.AssistantMessageChunk{Text: "private reasoning"},
	})
	if appended, isNew, err := state.AppendUpdateIfNew(ctx, thought); err != nil || isNew || appended != nil {
		t.Fatalf("append thought = %#v new=%t err=%v", appended, isNew, err)
	}

	old := "before"
	start := testUpdateEvent(3, now.Add(time.Millisecond), harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCall, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Title: "Edit file", Kind: "edit", Status: harnessv2.ToolCallStatusPending, Name: "edit_file",
		RawInput: json.RawMessage(`{"path":"/w/a"}`), Diffs: []harnessv2.ToolCallDiff{{Path: "a", OldText: &old, NewText: "after"}},
	}})
	if _, isNew, err := state.AppendUpdateIfNew(ctx, start); err != nil || !isNew {
		t.Fatalf("append tool start new=%t err=%v", isNew, err)
	}
	inputOnly := testUpdateEvent(4, now.Add(2*time.Millisecond), harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Status: harnessv2.ToolCallStatusInProgress, RawInput: json.RawMessage(`{"path":"/w/b"}`),
	}})
	if appended, isNew, err := state.AppendUpdateIfNew(ctx, inputOnly); err != nil || isNew || appended != nil {
		t.Fatalf("a live-only update after the start was persisted: %#v new=%t err=%v", appended, isNew, err)
	}
	done := testUpdateEvent(5, now.Add(3*time.Millisecond), harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "tool-1", Status: harnessv2.ToolCallStatusCompleted,
	}})
	if _, isNew, err := state.AppendUpdateIfNew(ctx, done); err != nil || !isNew {
		t.Fatalf("append tool completion new=%t err=%v", isNew, err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 2 {
		t.Fatalf("persisted %d events, want the tool start and completion", len(listed))
	}
	var persisted strings.Builder
	for _, event := range listed {
		persisted.WriteString(event.Summary)
		persisted.Write(event.Content)
		persisted.WriteString(event.ContentText)
	}
	for _, liveOnly := range []string{"private reasoning", "/w/b", "after", "before"} {
		if strings.Contains(persisted.String(), liveOnly) {
			t.Fatalf("persisted events carry live-only %q: %s", liveOnly, persisted.String())
		}
	}
}
