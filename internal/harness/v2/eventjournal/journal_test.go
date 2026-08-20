package eventjournal

import (
	"context"
	"errors"
	"fmt"
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

func TestJournalRecoveryScansOnlyCurrentPromptTail(t *testing.T) {
	ctx := context.Background()
	base := storetest.NewFakeExecutionEventStore()
	var oldEvent harnessv2.Event
	for index := range store.MaxExecutionEventLimit + 5 {
		oldEvent = testUpdateEvent(uint64(index+2), time.Now().UTC(), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: 1},
		})
		oldEvent.Identity.PromptID = "prompt-old"
		mapped, err := MapUpdate(oldEvent, testMapContext())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := base.AppendExecutionEvent(ctx, mapped); err != nil {
			t.Fatal(err)
		}
	}
	current := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{OutputTokens: 2},
	})
	mapped, err := MapUpdate(current, testMapContext())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.AppendExecutionEvent(ctx, mapped); err != nil {
		t.Fatal(err)
	}

	recording := &recordingExecutionEventStore{ExecutionEventStore: base}
	recoveryIdentity := mappedUpdateIdentity(current)
	recoveryIdentity.Sequence = 0
	state, err := (Journal{
		EventStore: recording, MapContext: testMapContext(), RecoveryIdentity: recoveryIdentity,
	}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasUpdate(current) || state.HasUpdate(oldEvent) {
		t.Fatalf("recovered current=%t old=%t", state.HasUpdate(current), state.HasUpdate(oldEvent))
	}
	if len(recording.filters) != 1 || recording.filters[0].AfterSeq == 0 {
		t.Fatalf("recovery filters = %#v", recording.filters)
	}
}

func TestJournalRetriesAppendOnlyAfterConfirmedAbsence(t *testing.T) {
	ctx := context.Background()
	base := storetest.NewFakeExecutionEventStore()
	eventStore := &faultingAppendEventStore{
		ExecutionEventStore: base,
		faults:              []appendFault{{err: errors.New("transient append failure")}},
	}
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: 10},
	})
	appended, isNew, err := state.AppendUpdateIfNew(ctx, event)
	if err != nil || !isNew || appended == nil {
		t.Fatalf("recovered append = %#v new=%t err=%v", appended, isNew, err)
	}
	if eventStore.appendCalls != 2 {
		t.Fatalf("append calls = %d, want 2", eventStore.appendCalls)
	}
	if listed := listJournalEvents(t, ctx, base); len(listed) != 1 {
		t.Fatalf("persisted events = %d, want 1", len(listed))
	}
}

func TestJournalReconcilesAmbiguousCommittedAppendWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	base := storetest.NewFakeExecutionEventStore()
	eventStore := &faultingAppendEventStore{
		ExecutionEventStore: base,
		faults: []appendFault{{
			persistBeforeError: true,
			err:                errors.New("append response lost"),
		}},
	}
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: 10},
	})
	appended, isNew, err := state.AppendUpdateIfNew(ctx, event)
	if err != nil || isNew || appended != nil {
		t.Fatalf("ambiguous append = %#v new=%t err=%v", appended, isNew, err)
	}
	if eventStore.appendCalls != 1 {
		t.Fatalf("append calls = %d, want 1", eventStore.appendCalls)
	}
	if !state.HasUpdate(event) {
		t.Fatal("journal did not remember reconciled update")
	}
	if listed := listJournalEvents(t, ctx, base); len(listed) != 1 {
		t.Fatalf("persisted events = %d, want 1", len(listed))
	}
}

func TestJournalReconcilesAmbiguousCommittedRetryWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	base := storetest.NewFakeExecutionEventStore()
	eventStore := &faultingAppendEventStore{
		ExecutionEventStore: base,
		faults: []appendFault{
			{err: errors.New("transient append failure")},
			{persistBeforeError: true, err: errors.New("retry response lost")},
		},
	}
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: 10},
	})
	appended, isNew, err := state.AppendUpdateIfNew(ctx, event)
	if err != nil || isNew || appended != nil {
		t.Fatalf("ambiguous retry = %#v new=%t err=%v", appended, isNew, err)
	}
	if eventStore.appendCalls != 2 {
		t.Fatalf("append calls = %d, want 2", eventStore.appendCalls)
	}
	if listed := listJournalEvents(t, ctx, base); len(listed) != 1 {
		t.Fatalf("persisted events = %d, want 1", len(listed))
	}
}

func TestJournalReturnsPersistentAppendFailureAfterOneRetry(t *testing.T) {
	ctx := context.Background()
	base := storetest.NewFakeExecutionEventStore()
	eventStore := &faultingAppendEventStore{
		ExecutionEventStore: base,
		faults: []appendFault{
			{err: errors.New("first append failure")},
			{err: errors.New("retry append failure")},
		},
	}
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: 10},
	})
	if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err == nil || isNew || appended != nil {
		t.Fatalf("persistent append = %#v new=%t err=%v", appended, isNew, err)
	}
	if eventStore.appendCalls != 2 {
		t.Fatalf("append calls = %d, want 2", eventStore.appendCalls)
	}
	if state.HasUpdate(event) {
		t.Fatal("journal remembered an update that was never persisted")
	}
}

func TestJournalDoesNotRetryWhenAppendAbsenceCannotBeConfirmed(t *testing.T) {
	ctx := context.Background()
	base := storetest.NewFakeExecutionEventStore()
	eventStore := &faultingAppendEventStore{
		ExecutionEventStore: base,
		faults:              []appendFault{{err: errors.New("append failure")}},
		listFaultAt:         2,
		listErr:             errors.New("readback failure"),
	}
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: 10},
	})
	if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err == nil || isNew || appended != nil {
		t.Fatalf("unreconciled append = %#v new=%t err=%v", appended, isNew, err)
	}
	if eventStore.appendCalls != 1 {
		t.Fatalf("append calls = %d, want 1", eventStore.appendCalls)
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

func TestJournalPersistsAssistantTextOnNonTerminalStreamClosure(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	journal := Journal{EventStore: eventStore, MapContext: testMapContext()}
	state, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	credential := "sk-" + strings.Repeat("d", 24)
	fragments := []string{"before " + credential[:10], credential[10:] + " after"}
	now := time.Now().UTC()
	var last harnessv2.Event
	for index, fragment := range fragments {
		last = testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind:             harnessv2.UpdateAssistantMessageChunk,
			AssistantMessage: &harnessv2.AssistantMessageChunk{Text: fragment},
		})
		if appended, isNew, err := state.AppendUpdateIfNew(ctx, last); err != nil || isNew || appended != nil {
			t.Fatalf("assistant chunk %d = %#v new=%t err=%v", index, appended, isNew, err)
		}
	}
	transcript := strings.Join(fragments, "")
	if appended, isNew, err := state.AppendAssistantStreamClosureIfNew(ctx, last, transcript); err != nil || !isNew || appended == nil {
		t.Fatalf("assistant stream closure = %#v new=%t err=%v", appended, isNew, err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 1 || listed[0].ContentText != "before "+executionevents.ExecutionEventRedactedValue+" after" {
		t.Fatalf("persisted assistant stream closure = %#v", listed)
	}
	recovered, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, isNew, err := recovered.AppendAssistantStreamClosureIfNew(ctx, last, transcript); err != nil || isNew || duplicate != nil {
		t.Fatalf("recovered assistant stream closure = %#v new=%t err=%v", duplicate, isNew, err)
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
		if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || isNew != (index == len(fragments)-1) {
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

func TestJournalOmitsToolContentSplitAcrossBlocks(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	prefix := "sk-aaaaaaaa"
	suffix := "aaaaaaaaaaaaaaaa"
	event := testUpdateEvent(2, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-split-blocks", Kind: "shell", Status: harnessv2.ToolCallStatusCompleted,
			Content: []harnessv2.ContentBlock{
				{Type: harnessv2.ContentBlockText, Text: prefix},
				{Type: harnessv2.ContentBlockText, Text: suffix},
			},
		},
	})
	if _, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew {
		t.Fatalf("append multi-block tool update new=%t err=%v", isNew, err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 1 {
		t.Fatalf("persisted tool events = %d, want 1", len(listed))
	}
	persisted := listed[0]
	encoded := persisted.Summary + persisted.ContentText + string(persisted.Content)
	if persisted.ContentText != "" || strings.Contains(encoded, prefix) || strings.Contains(encoded, suffix) ||
		!strings.Contains(string(persisted.Content), toolContentMultipleBlocksOmittedReason) || persisted.Truncation != nil {
		t.Fatalf("multi-block tool event = %#v", persisted)
	}
}

func TestJournalAggregatesContentOnlyToolFragmentsIntoTerminalEvent(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for index, fragment := range []string{"streamed ", "output"} {
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-content-only", Status: harnessv2.ToolCallStatusInProgress,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: fragment}},
			},
		})
		if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || isNew || appended != nil {
			t.Fatalf("content fragment %d = %#v new=%t err=%v", index, appended, isNew, err)
		}
	}
	completed := testUpdateEvent(4, now.Add(2*time.Millisecond), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-content-only", Status: harnessv2.ToolCallStatusCompleted,
		},
	})
	if appended, isNew, err := state.AppendUpdateIfNew(ctx, completed); err != nil || !isNew || appended == nil {
		t.Fatalf("completed tool = %#v new=%t err=%v", appended, isNew, err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 1 || listed[0].Type != executionevents.ExecutionEventTypeToolCallCompleted ||
		listed[0].ContentText != "streamed output" {
		t.Fatalf("persisted tool events = %#v", listed)
	}
}

func TestJournalPersistsBufferedToolOutputOnStreamClosure(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	credential := "sk-" + strings.Repeat("z", 24)
	now := time.Now().UTC()
	fragments := []string{"before " + credential[:10], credential[10:] + " after"}
	for index, fragment := range fragments {
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-open", Title: "Inspect repository", Kind: "shell",
				Status:  harnessv2.ToolCallStatusInProgress,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: fragment}},
			},
		})
		if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || isNew || appended != nil {
			t.Fatalf("append open tool fragment %d = %#v new=%t err=%v", index, appended, isNew, err)
		}
	}
	if listed := listJournalEvents(t, ctx, eventStore); len(listed) != 0 {
		t.Fatalf("tool output persisted before closure: %#v", listed)
	}
	if err := state.AppendToolStreamClosuresIfNew(ctx); err != nil {
		t.Fatal(err)
	}
	if err := state.AppendToolStreamClosuresIfNew(ctx); err != nil {
		t.Fatal(err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 1 || listed[0].Type != executionevents.ExecutionEventTypeToolCallFailed ||
		listed[0].Severity != executionevents.ExecutionEventSeverityError ||
		listed[0].ContentText != "before "+executionevents.ExecutionEventRedactedValue+" after" ||
		listed[0].ToolName != "shell" || listed[0].Summary != "Inspect repository" {
		t.Fatalf("persisted tool stream closure = %#v", listed)
	}
}

func TestJournalPersistsBufferedToolClosuresInProtocolOrder(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for index, tool := range []struct {
		id      string
		content string
	}{{id: "call-z", content: "first"}, {id: "call-a", content: "second"}} {
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: tool.id, Status: harnessv2.ToolCallStatusInProgress,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: tool.content}},
			},
		})
		if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || isNew || appended != nil {
			t.Fatalf("append open tool %q = %#v new=%t err=%v", tool.id, appended, isNew, err)
		}
	}
	if err := state.AppendToolStreamClosuresIfNew(ctx); err != nil {
		t.Fatal(err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 2 ||
		listed[0].Type != executionevents.ExecutionEventTypeToolCallFailed ||
		listed[1].Type != executionevents.ExecutionEventTypeToolCallFailed ||
		listed[0].ContentText != "first" || listed[1].ContentText != "second" {
		t.Fatalf("persisted tool closure order = %#v", listed)
	}
}

func TestJournalReplacesACPToolContentSnapshots(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for index, snapshot := range []string{"a", "ab"} {
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-snapshot", Status: harnessv2.ToolCallStatusInProgress,
				Content:        []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: snapshot}},
				ContentReplace: true,
			},
		})
		if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || isNew || appended != nil {
			t.Fatalf("content snapshot %d = %#v new=%t err=%v", index, appended, isNew, err)
		}
	}
	completed := testUpdateEvent(4, now.Add(2*time.Millisecond), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-snapshot", Status: harnessv2.ToolCallStatusCompleted,
		},
	})
	if appended, isNew, err := state.AppendUpdateIfNew(ctx, completed); err != nil || !isNew || appended == nil {
		t.Fatalf("completed tool = %#v new=%t err=%v", appended, isNew, err)
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 1 || listed[0].ContentText != "ab" {
		t.Fatalf("persisted tool snapshot events = %#v", listed)
	}
}

func TestJournalBuffersToolMetadataUntilTerminalEvent(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	credential := "sk-" + strings.Repeat("c", 24)
	now := time.Now().UTC()
	updates := []harnessv2.UpdateEvent{
		{Kind: harnessv2.UpdateToolCall, ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-metadata", Title: credential[:10], Kind: "shell", Status: harnessv2.ToolCallStatusPending,
		}},
		{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-metadata", Title: credential[10:], Status: harnessv2.ToolCallStatusInProgress,
		}},
		{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-metadata", Title: "Finished safely", Status: harnessv2.ToolCallStatusCompleted,
		}},
	}
	for index, update := range updates {
		if _, isNew, err := state.AppendUpdateIfNew(
			ctx,
			testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), update),
		); err != nil || !isNew {
			t.Fatalf("append metadata update %d new=%t err=%v", index, isNew, err)
		}
	}

	listed := listJournalEvents(t, ctx, eventStore)
	if len(listed) != 3 {
		t.Fatalf("persisted tool events = %d, want 3", len(listed))
	}
	var persisted strings.Builder
	for _, event := range listed {
		persisted.WriteString(event.Summary)
		persisted.WriteString(event.ToolName)
		persisted.Write(event.Content)
	}
	if strings.Contains(persisted.String(), credential) || strings.Contains(persisted.String(), credential[:10]) ||
		strings.Contains(persisted.String(), credential[10:]) {
		t.Fatalf("streamed tool metadata remained reconstructable: %q", persisted.String())
	}
	for _, event := range listed[:2] {
		if event.ToolName != "" || !strings.Contains(string(event.Content), "metadataOmitted") {
			t.Fatalf("nonterminal tool metadata was persisted: %#v", event)
		}
	}
	terminal := listed[2]
	if terminal.ToolName != "shell" || terminal.Summary != "Finished safely" {
		t.Fatalf("terminal tool metadata = %#v", terminal)
	}
}

func TestJournalOmitsOversizedToolStreamContent(t *testing.T) {
	ctx := context.Background()
	eventStore := storetest.NewFakeExecutionEventStore()
	state, err := (Journal{EventStore: eventStore, MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	credential := "sk-" + strings.Repeat("c", 24)
	now := time.Now().UTC()
	for index := range 9 {
		status := harnessv2.ToolCallStatusInProgress
		if index == 8 {
			status = harnessv2.ToolCallStatusCompleted
		}
		content := strings.Repeat("x", harnessv2.MaxProtocolStringBytes)
		switch index {
		case 7:
			content = strings.Repeat("x", harnessv2.MaxProtocolStringBytes-8) + credential[:8]
		case 8:
			content = credential[8:]
		}
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "call-large", Kind: "shell", Status: status,
				Content: []harnessv2.ContentBlock{{
					Type: harnessv2.ContentBlockText,
					Text: content,
				}},
			},
		})
		if _, _, err := state.AppendUpdateIfNew(ctx, event); err != nil {
			t.Fatalf("append oversized tool update %d: %v", index, err)
		}
	}

	listed := listJournalEvents(t, ctx, eventStore)
	completed := listed[len(listed)-1]
	encoded := completed.Summary + completed.ContentText + string(completed.Content)
	if completed.ContentText != "" || strings.Contains(encoded, credential[:8]) ||
		completed.Truncation == nil || !completed.Truncation.ContentTextTruncated ||
		!strings.Contains(string(completed.Content), "streamed_text_exceeded_journal_limit") {
		t.Fatalf("oversized completed tool event = %#v", completed)
	}
}

func TestJournalRejectsTooManyOpenToolAccumulators(t *testing.T) {
	ctx := context.Background()
	state, err := (Journal{EventStore: storetest.NewFakeExecutionEventStore(), MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for index := range maxOpenToolAccumulators {
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCall,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: fmt.Sprintf("call-open-%d", index), Status: harnessv2.ToolCallStatusPending,
			},
		})
		if _, _, err := state.AppendUpdateIfNew(ctx, event); err != nil {
			t.Fatalf("append open tool %d: %v", index, err)
		}
	}
	overflow := testUpdateEvent(uint64(maxOpenToolAccumulators+2), now.Add(time.Second), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCall,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-open-overflow", Status: harnessv2.ToolCallStatusPending,
		},
	})
	for attempt := range 2 {
		if _, _, err := state.AppendUpdateIfNew(ctx, overflow); !errors.Is(err, ErrToolBufferLimitExceeded) {
			t.Fatalf("open-tool overflow attempt %d error = %v", attempt, err)
		}
	}
}

func TestJournalRejectsAggregateToolContentOverflow(t *testing.T) {
	ctx := context.Background()
	state, err := (Journal{EventStore: storetest.NewFakeExecutionEventStore(), MapContext: testMapContext()}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("🙂", executionevents.MaxExecutionEventContentTextChars)
	contentBytes := len(content)
	now := time.Now().UTC()
	for index := 0; index < maxBufferedToolContentBytes/contentBytes; index++ {
		event := testUpdateEvent(uint64(index+2), now.Add(time.Duration(index)*time.Millisecond), harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: fmt.Sprintf("call-buffer-%d", index), Status: harnessv2.ToolCallStatusInProgress,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: content}}, ContentReplace: true,
			},
		})
		if _, _, err := state.AppendUpdateIfNew(ctx, event); err != nil {
			t.Fatalf("append buffered tool %d: %v", index, err)
		}
	}
	overflow := testUpdateEvent(100, now.Add(time.Second), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "call-buffer-overflow", Status: harnessv2.ToolCallStatusInProgress,
			Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: content}}, ContentReplace: true,
		},
	})
	if _, _, err := state.AppendUpdateIfNew(ctx, overflow); !errors.Is(err, ErrToolBufferLimitExceeded) {
		t.Fatalf("aggregate tool overflow error = %v", err)
	}
}

func TestToolContentFragmentPreservesURIOnlyResourceLink(t *testing.T) {
	got, multipleBlocks := toolContentFragment([]harnessv2.ContentBlock{{
		Type: harnessv2.ContentBlockResourceLink,
		URI:  "https://example.com/output.txt?X-Amz-Credential=secret&X-Amz-Signature=value#download",
	}})
	if got != "resource: https://example.com/output.txt" || multipleBlocks {
		t.Fatalf("resource-link fragment = %q multipleBlocks=%t", got, multipleBlocks)
	}
}

func TestToolContentFragmentSanitizesURLShapedResourceName(t *testing.T) {
	got, multipleBlocks := toolContentFragment([]harnessv2.ContentBlock{{
		Type: harnessv2.ContentBlockResourceLink,
		Name: "https://account.blob.core.windows.net/output.txt?sp=r&sig=usable-secret#download",
		URI:  "https://fallback.example.com/output.txt",
	}})
	if got != "resource: https://account.blob.core.windows.net/output.txt" || multipleBlocks ||
		strings.Contains(got, "sig=") || strings.Contains(got, "usable-secret") {
		t.Fatalf("resource-link name fragment = %q multipleBlocks=%t", got, multipleBlocks)
	}
}

func TestToolContentFragmentSanitizesRelativeResourceNames(t *testing.T) {
	for _, test := range []struct {
		name string
		want string
	}{
		{name: "//account.blob.core.windows.net/output.txt?sig=usable-secret#download", want: "resource: //account.blob.core.windows.net/output.txt"},
		{name: "output.txt?sig=usable-secret#download", want: "resource: output.txt"},
	} {
		got, multipleBlocks := toolContentFragment([]harnessv2.ContentBlock{{
			Type: harnessv2.ContentBlockResourceLink,
			Name: test.name,
			URI:  "https://fallback.example.com/output.txt",
		}})
		if got != test.want || multipleBlocks || strings.Contains(got, "sig=") || strings.Contains(got, "usable-secret") {
			t.Fatalf("resource-link name %q fragment = %q multipleBlocks=%t", test.name, got, multipleBlocks)
		}
	}
}

func TestToolContentFragmentOmitsOpaqueResourceURI(t *testing.T) {
	got, multipleBlocks := toolContentFragment([]harnessv2.ContentBlock{{
		Type: harnessv2.ContentBlockResourceLink,
		URI:  "data:application/json,%7B%22private%22%3A%22sensitive-payload%22%7D",
	}})
	if got != "" || multipleBlocks {
		t.Fatalf("opaque resource-link fragment = %q multipleBlocks=%t", got, multipleBlocks)
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

type appendFault struct {
	persistBeforeError bool
	err                error
}

type faultingAppendEventStore struct {
	store.ExecutionEventStore
	faults      []appendFault
	appendCalls int
	listFaultAt int
	listErr     error
	listCalls   int
}

type recordingExecutionEventStore struct {
	store.ExecutionEventStore
	filters []store.ExecutionEventFilter
}

func (s *recordingExecutionEventStore) ListExecutionEvents(
	ctx context.Context,
	filter store.ExecutionEventFilter,
) ([]store.ExecutionEvent, error) {
	s.filters = append(s.filters, filter)
	return s.ExecutionEventStore.ListExecutionEvents(ctx, filter)
}

func (s *faultingAppendEventStore) AppendExecutionEvent(
	ctx context.Context,
	event *store.ExecutionEvent,
) (*store.ExecutionEvent, error) {
	s.appendCalls++
	if s.appendCalls <= len(s.faults) {
		fault := s.faults[s.appendCalls-1]
		if fault.persistBeforeError {
			if _, err := s.ExecutionEventStore.AppendExecutionEvent(ctx, event); err != nil {
				return nil, err
			}
		}
		return nil, fault.err
	}
	return s.ExecutionEventStore.AppendExecutionEvent(ctx, event)
}

func (s *faultingAppendEventStore) ListExecutionEvents(
	ctx context.Context,
	filter store.ExecutionEventFilter,
) ([]store.ExecutionEvent, error) {
	s.listCalls++
	if s.listCalls == s.listFaultAt {
		return nil, s.listErr
	}
	return s.ExecutionEventStore.ListExecutionEvents(ctx, filter)
}
