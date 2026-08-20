package eventjournal

import (
	"context"
	"testing"
	"time"

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
		Kind:             harnessv2.UpdateAssistantMessageChunk,
		AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "next"},
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
