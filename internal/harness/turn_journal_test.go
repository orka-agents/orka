/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

func TestTurnJournalOpenIndexesStoredHarnessIdentity(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	_, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		Type:       events.ExecutionEventTypeAgentRuntimeStarted,
		Content:    []byte(`{"harness":{"runtimeSessionID":"runtime-1","turnID":"turn-1","correlationID":"corr-1","seq":7}}`),
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
	state, err := TurnJournal{EventStore: eventStore, MapContext: EventMapContext{Namespace: "default", TaskName: "task-a"}}.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	keys := state.keys
	key := strings.Join([]string{"runtime-1", "turn-1", "corr-1", "7"}, "\x00")
	if _, ok := keys[key]; !ok {
		t.Fatalf("existing frame key missing from %#v", keys)
	}
}

func TestTurnJournalHasPersistedFrames(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		Type:       events.ExecutionEventTypeAgentRuntimeStarted,
		Content:    []byte(`{"harness":{"runtimeSessionID":"runtime-1","turnID":"turn-abc","correlationID":"corr-1","seq":1}}`),
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
	journal := TurnJournal{EventStore: eventStore, MapContext: EventMapContext{Namespace: "default", TaskName: "task-a"}}

	has, err := journal.HasPersistedFrames(context.Background(), "turn-abc")
	if err != nil {
		t.Fatalf("HasPersistedFrames: %v", err)
	}
	if !has {
		t.Fatal("expected persisted frames for turn-abc to be detected")
	}

	has, err = journal.HasPersistedFrames(context.Background(), "turn-other")
	if err != nil {
		t.Fatalf("HasPersistedFrames(other): %v", err)
	}
	if has {
		t.Fatal("unexpected match for a different turn ID")
	}

	emptyJournal := TurnJournal{EventStore: store.NewFakeExecutionEventStore(), MapContext: EventMapContext{Namespace: "default", TaskName: "task-a"}}
	has, err = emptyJournal.HasPersistedFrames(context.Background(), "turn-abc")
	if err != nil {
		t.Fatalf("HasPersistedFrames(empty): %v", err)
	}
	if has {
		t.Fatal("unexpected match against an empty store")
	}
}

func TestTurnJournalPagesPastNonHarnessEvents(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	for i := range store.MaxExecutionEventLimit {
		if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace:  "default",
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   "task-a",
			TaskName:   "task-a",
			Type:       events.ExecutionEventTypeModelMessage,
			Summary:    fmt.Sprintf("non-harness event %d", i),
		}); err != nil {
			t.Fatalf("AppendExecutionEvent(non-harness %d): %v", i, err)
		}
	}
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		TaskName:   "task-a",
		Type:       events.ExecutionEventTypeAgentRuntimeCompleted,
		Content:    []byte(`{"harness":{"runtimeSessionID":"runtime-page","turnID":"turn-page","correlationID":"corr-page","seq":1001}}`),
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(harness): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	keys, err := (TurnJournal{EventStore: eventStore, MapContext: EventMapContext{Namespace: "default", TaskName: "task-a"}}).ExistingFrameKeys(ctx)
	if err != nil {
		t.Fatalf("ExistingFrameKeys: %v", err)
	}
	key := strings.Join([]string{"runtime-page", "turn-page", "corr-page", "1001"}, "\x00")
	if _, ok := keys[key]; !ok {
		t.Fatalf("paged harness frame key missing from %#v", keys)
	}
}

func TestTurnJournalAppendFrameIfNewDeduplicatesMappedFrame(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	journal := TurnJournal{EventStore: eventStore, MapContext: EventMapContext{Namespace: "default", TaskName: "task-a", AgentName: "agent-a"}}
	state, err := journal.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	frame := HarnessEventFrame{
		Version:          ProtocolVersion,
		Type:             FrameTurnStarted,
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Seq:              1,
		CreatedAt:        time.Now().UTC(),
	}

	appended, appendedNew, err := state.AppendFrameIfNew(context.Background(), frame)
	if err != nil {
		t.Fatalf("AppendFrameIfNew first: %v", err)
	}
	if !appendedNew || appended == nil {
		t.Fatalf("first append = (%#v, %v), want appended event", appended, appendedNew)
	}

	appended, appendedNew, err = state.AppendFrameIfNew(context.Background(), frame)
	if err != nil {
		t.Fatalf("AppendFrameIfNew duplicate: %v", err)
	}
	if appendedNew || appended != nil {
		t.Fatalf("duplicate append = (%#v, %v), want skipped", appended, appendedNew)
	}

	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("events = %#v, want one appended event", listed)
	}
}

func TestTurnJournalAppendFrameIfNewPropagatesAppendFailure(t *testing.T) {
	journal := TurnJournal{
		EventStore: failingAppendExecutionEventStore{err: fmt.Errorf("store unavailable")},
		MapContext: EventMapContext{Namespace: "default", TaskName: "task-a"},
	}
	state := &TurnJournalState{journal: journal, keys: map[string]struct{}{}}
	_, _, err := state.AppendFrameIfNew(context.Background(), HarnessEventFrame{
		Version:          ProtocolVersion,
		Type:             FrameTurnStarted,
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Seq:              1,
		CreatedAt:        time.Now().UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "append mapped harness event: store unavailable") {
		t.Fatalf("AppendFrameIfNew error = %v, want append failure", err)
	}
}

type failingAppendExecutionEventStore struct {
	store.ExecutionEventStore
	err error
}

func (s failingAppendExecutionEventStore) AppendExecutionEvent(context.Context, *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	return nil, s.err
}
