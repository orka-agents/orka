package api

import (
	"context"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestTaskTimelineReaderListMatchingReturnsEmptySlice(t *testing.T) {
	reader := newTaskTimelineReader(store.NewFakeExecutionEventStore(), "default", "task-a")
	listed, err := reader.listMatching(context.Background(), []string{events.ExecutionEventTypeApprovalRequested})
	if err != nil {
		t.Fatalf("listMatching error = %v", err)
	}
	if listed == nil || len(listed) != 0 {
		t.Fatalf("listMatching = %#v, want non-nil empty slice", listed)
	}
}

func TestTaskTimelineReaderListThroughAllowsExactLimitAndRejectsNext(t *testing.T) {
	ctx := context.Background()
	eventStore := store.NewFakeExecutionEventStore()
	appendReaderEvents(t, eventStore, events.ExecutionEventTypeModelMessage, 3)
	reader := newTaskTimelineReader(eventStore, "default", "task-a")

	listed, err := reader.listThrough(ctx, 3, 3)
	if err != nil {
		t.Fatalf("listThrough exact limit error = %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("listThrough exact limit returned %d events, want 3", len(listed))
	}
	_, err = reader.listThrough(ctx, 3, 2)
	if !errors.Is(err, errTaskTimelineReadLimitExceeded) {
		t.Fatalf("listThrough over limit error = %v, want limit exceeded", err)
	}
}

func TestTaskTimelineReaderListRecentThroughPreservesForkOverflowEvent(t *testing.T) {
	ctx := context.Background()
	eventStore := store.NewFakeExecutionEventStore()
	appendReaderEvents(t, eventStore, events.ExecutionEventTypeModelMessage, 5)
	reader := newTaskTimelineReader(eventStore, "default", "task-a")

	listed, err := reader.listRecentThrough(ctx, 5, 3)
	if err != nil {
		t.Fatalf("listRecentThrough error = %v", err)
	}
	if len(listed) != 3 || listed[0].Seq != 3 || listed[2].Seq != 5 {
		t.Fatalf("listRecentThrough seqs = %v, want 3..5", eventSeqs(listed))
	}
}

func TestTaskTimelineReaderSeqExistsValidatesCheckpointRanges(t *testing.T) {
	ctx := context.Background()
	eventStore := store.NewFakeExecutionEventStore()
	appendReaderEvents(t, eventStore, events.ExecutionEventTypeModelMessage, 2)
	reader := newTaskTimelineReader(eventStore, "default", "task-a")

	for _, seq := range []int64{0, 2} {
		ok, err := reader.seqExists(ctx, seq, 2)
		if err != nil || !ok {
			t.Fatalf("seqExists(%d, latest=2) = %t, %v; want true nil", seq, ok, err)
		}
	}
	for _, seq := range []int64{-1, 3} {
		ok, err := reader.seqExists(ctx, seq, 2)
		if err != nil || ok {
			t.Fatalf("seqExists(%d, latest=2) = %t, %v; want false nil", seq, ok, err)
		}
	}
	ok, err := reader.seqExists(ctx, 1, 2)
	if err != nil || !ok {
		t.Fatalf("seqExists(existing seq) = %t, %v; want true nil", ok, err)
	}
}

func TestTaskTimelineReaderTerminalForCompletionScansPastFilteredCursor(t *testing.T) {
	ctx := context.Background()
	eventStore := store.NewFakeExecutionEventStore()
	appendReaderEvents(t, eventStore, events.ExecutionEventTypeToolCallCompleted, 1)
	appendReaderEvents(t, eventStore, events.ExecutionEventTypeTaskSucceeded, 1)
	appendReaderEvents(t, eventStore, events.ExecutionEventTypeToolCallCompleted, 1)
	reader := newTaskTimelineReader(eventStore, "default", "task-a")

	terminal, found, scannedThrough, err := reader.terminalForCompletion(ctx, 1)
	if err != nil {
		t.Fatalf("terminalForCompletion error = %v", err)
	}
	if !found || terminal.Type != events.ExecutionEventTypeTaskSucceeded || terminal.Seq != 2 {
		t.Fatalf("terminalForCompletion terminal = %#v found=%t, want TaskSucceeded seq 2", terminal, found)
	}
	if scannedThrough != 2 {
		t.Fatalf("scannedThrough = %d, want 2", scannedThrough)
	}
}

func appendReaderEvents(t *testing.T, eventStore store.ExecutionEventStore, eventType string, count int) {
	t.Helper()
	for range count {
		if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a", Type: eventType}); err != nil {
			t.Fatalf("AppendExecutionEvent: %v", err)
		}
	}
}

func eventSeqs(values []store.ExecutionEvent) []int64 {
	out := make([]int64, 0, len(values))
	for _, value := range values {
		out = append(out, value.Seq)
	}
	return out
}
