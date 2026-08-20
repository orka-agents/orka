package eventjournal

import (
	"context"
	"strings"
	"testing"
	"time"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
)

func TestJournalDeduplicatesWithinPassAndAcrossRecovery(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	journal := Journal{EventStore: eventStore, MapContext: testMapContext()}
	state, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind:  harnessv2.UpdateUsage,
		Usage: &harnessv2.UsageUpdate{InputTokens: 10},
	})
	appended, isNew, err := state.AppendUpdateIfNew(ctx, event)
	if err != nil || !isNew || appended == nil {
		t.Fatalf("first append = %#v, new=%t, err=%v", appended, isNew, err)
	}
	if !state.HasUpdate(event) {
		t.Fatal("journal did not remember appended update")
	}
	if duplicate, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || isNew || duplicate != nil {
		t.Fatalf("same-pass duplicate = %#v, new=%t, err=%v", duplicate, isNew, err)
	}

	recovered, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.HasUpdate(event) {
		t.Fatal("recovered journal did not load persisted update")
	}
	if duplicate, isNew, err := recovered.AppendUpdateIfNew(ctx, event); err != nil || isNew || duplicate != nil {
		t.Fatalf("recovered duplicate = %#v, new=%t, err=%v", duplicate, isNew, err)
	}
	next := testUpdateEvent(3, event.Identity.Timestamp.Add(time.Millisecond), harnessv2.UpdateEvent{
		Kind:  harnessv2.UpdateUsage,
		Usage: &harnessv2.UsageUpdate{OutputTokens: 5},
	})
	if _, isNew, err := recovered.AppendUpdateIfNew(ctx, next); err != nil || !isNew {
		t.Fatalf("next append new=%t, err=%v", isNew, err)
	}

	listed, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-1", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("persisted events = %d, want 2", len(listed))
	}
}

func TestJournalRedactsCredentialsSplitAcrossAssistantChunks(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	journal := Journal{EventStore: eventStore, MapContext: testMapContext()}
	state, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	credential := "sk-" + strings.Repeat("a", 24)
	chunks := []string{"hello", " ", credential[:9], credential[9:] + " world"}
	transcript := strings.Join(chunks, "")
	now := time.Now().UTC()
	for index, chunk := range chunks {
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind:             harnessv2.UpdateAssistantMessageChunk,
			AssistantMessage: &harnessv2.AssistantMessageChunk{Text: chunk},
		})
		if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || isNew || appended != nil {
			t.Fatalf("append assistant chunk %d = %#v new=%t err=%v", index, appended, isNew, err)
		}
	}
	terminal := testTerminalEvent(6, now.Add(5*time.Millisecond))
	if _, isNew, err := state.AppendAssistantTranscriptIfNew(ctx, terminal, transcript); err != nil || !isNew {
		t.Fatalf("append assistant transcript new=%t err=%v", isNew, err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	var persisted strings.Builder
	for _, event := range listed {
		persisted.WriteString(event.Summary)
		persisted.Write(event.Content)
		persisted.WriteString(event.ContentText)
	}
	if strings.Contains(persisted.String(), credential) ||
		strings.Contains(persisted.String(), credential[:9]) ||
		!strings.Contains(persisted.String(), executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("persisted assistant stream was reconstructable: %q", persisted.String())
	}
	if got := listed[len(listed)-1].ContentText; got != "hello "+executionevents.ExecutionEventRedactedValue+" world" {
		t.Fatalf("assistant transcript = %q", got)
	}

	recovered, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, isNew, err := recovered.AppendAssistantTranscriptIfNew(ctx, terminal, transcript); err != nil || isNew || duplicate != nil {
		t.Fatalf("recovered assistant transcript = %#v new=%t err=%v", duplicate, isNew, err)
	}
}

func TestJournalRedactsCredentialsSplitAcrossToolUpdates(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	credential := "sk-" + strings.Repeat("b", 24)
	fragments := []string{"before " + credential[:10], credential[10:] + " after"}
	now := time.Now().UTC()
	for index, fragment := range fragments {
		status := harnessv2.ToolCallStatusInProgress
		if index == len(fragments)-1 {
			status = harnessv2.ToolCallStatusCompleted
		}
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-split", Kind: "shell", Status: status,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: fragment}},
			},
		})
		if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
			t.Fatalf("append tool update %d new=%t err=%v", index, isNew, err)
		}
	}

	listed := listJournalEvents(t, ctx, eventStore)
	var persisted strings.Builder
	for _, event := range listed {
		persisted.WriteString(event.Summary)
		persisted.Write(event.Content)
		persisted.WriteString(event.ContentText)
	}
	if strings.Contains(persisted.String(), credential) ||
		strings.Contains(persisted.String(), credential[:10]) ||
		!strings.Contains(persisted.String(), executionevents.ExecutionEventRedactedValue) {
		t.Fatalf("persisted tool stream was reconstructable: %q", persisted.String())
	}
	if got := listed[len(listed)-1].ContentText; got != "before "+executionevents.ExecutionEventRedactedValue+" after" {
		t.Fatalf("completed tool output = %q", got)
	}
}

func TestJournalOmitsOversizedToolStream(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for index := range 9 {
		status := harnessv2.ToolCallStatusInProgress
		if index == 8 {
			status = harnessv2.ToolCallStatusCompleted
		}
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-large", Kind: "shell", Status: status,
				Content: []harnessv2.ContentBlock{{
					Type: harnessv2.ContentBlockText,
					Text: strings.Repeat("x", harnessv2.MaxProtocolStringBytes),
				}},
			},
		})
		if _, _, err := state.AppendUpdateIfNew(ctx, event); err != nil {
			t.Fatalf("append oversized tool update %d: %v", index, err)
		}
	}

	listed := listJournalEvents(t, ctx, eventStore)
	completed := listed[len(listed)-1]
	if completed.ContentText != "" || completed.Truncation == nil || !completed.Truncation.ContentTextTruncated {
		t.Fatalf("oversized completed tool event = %#v", completed)
	}
}

func listJournalEvents(t *testing.T, ctx context.Context, eventStore store.ExecutionEventStore) []store.ExecutionEvent {
	t.Helper()
	listed, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-1", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return listed
}
