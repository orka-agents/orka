package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
)

const markerWhitespaceHistorySize = 128

func TestExecutionEventMarkerWhitespaceAfterLargeHistory(t *testing.T) {
	for _, test := range []struct {
		name        string
		parts       []string
		title       string
		benign      bool
		reopen      bool
		toolHistory bool
	}{
		{name: "space", parts: []string{"api key", " is fixture-value"}},
		{name: "tab", parts: []string{"api\tkey", " is fixture-value"}, toolHistory: true},
		{name: "form-feed", parts: []string{"api\fkey", " is fixture-value"}},
		{name: "carriage-return", parts: []string{"api\rkey", " is fixture-value"}},
		{name: "line-feed", parts: []string{"api\nkey", " is fixture-value"}},
		{name: "mixed-whitespace", parts: []string{"api \t\f\r\n key", " is fixture-value"}},
		{name: "split-whitespace", parts: []string{"api\t", "\nkey", " is fixture-value"}},
		{name: "whitespace-only-fields", parts: []string{"api", "\t", "\f\r\n", "key", " is fixture-value"}},
		{name: "split-marker-and-whitespace", parts: []string{"a", "pi\t", "\r\n", "ke", "y", " is fixture-value"}},
		{name: "tab-after-reopen", parts: []string{"api\tkey", " is fixture-value"}, reopen: true},
		{name: "nbsp", parts: []string{"api\u00a0key", " is fixture-value"}, benign: true},
		{name: "nbsp-at-boundary", parts: []string{"api\t", "\u00a0key", " is fixture-value"}, benign: true},
		{name: "vertical-tab", parts: []string{"api\vkey", " is fixture-value"}, benign: true},
		{
			name: "ordinary-command", title: "cd /tmp/workspace && pwd && ls -la", benign: true,
			parts: []string{"/bin/bash: line 1: cd: /tmp/workspace: No such file or directory", "README.md\ncmd\ninternal\n"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			joined := strings.Join(test.parts, "")
			if protected := events.RedactExecutionEventText(joined) != joined; protected == test.benign {
				t.Fatal("fixture does not have the expected shared-redactor classification")
			}
			for _, part := range test.parts {
				if events.RedactExecutionEventText(part) != part {
					t.Fatal("fixture fragments must be harmless in isolation")
				}
			}
			journal, state := newModelHistoryJournal(t, "")
			historyRows, planDocument := seedMarkerWhitespaceHistory(t, state, test.toolHistory)
			title := test.title
			if title == "" {
				title = "Read output"
			}
			updates := make([]harnessv2.Event, 0, len(test.parts))
			for i, part := range test.parts {
				if test.reopen && i == 1 {
					var err error
					state, err = journal.Open(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					appendMarkerWhitespaceUpdate(t, state, updates[0], false)
				}
				event := markerWhitespaceToolEvent(uint64(historyRows+i+1), title, part)
				appendMarkerWhitespaceUpdate(t, state, event, true)
				appendMarkerWhitespaceUpdate(t, state, event, false)
				updates = append(updates, event)
			}
			rows := markerWhitespaceRows(t, journal, historyRows+len(test.parts))
			for i, row := range rows[:historyRows] {
				dto := markerWhitespacePublicDTO(t, row)
				if !test.toolHistory {
					if dto.Summary != "Plan updated (0/128 steps complete)" || dto.ContentText != planDocument {
						t.Fatal("benign plan history was suppressed or changed")
					}
					continue
				}
				wantTitle := markerWhitespaceHistoryTitle(i + 1)
				if dto.Summary != wantTitle || dto.Content.Title != wantTitle || dto.ContentText != "" {
					t.Fatalf("benign history event %d was suppressed or changed", i+1)
				}
			}
			var publicOutput strings.Builder
			for i, row := range rows[historyRows:] {
				dto := markerWhitespacePublicDTO(t, row)
				for _, field := range []struct {
					name  string
					value string
					want  string
				}{
					{name: "summary", value: dto.Summary, want: title},
					{name: "content.title", value: dto.Content.Title, want: title},
					{name: "contentText", value: dto.ContentText, want: test.parts[i]},
				} {
					if field.value != field.want && (test.benign || field.value != events.ExecutionEventRedactedValue) {
						t.Errorf("event %d %s changed unexpectedly", row.Seq, field.name)
					}
				}
				// ContentText is the only public copy of titled tool output.
				// Count persisted DTO locations once, regardless of replay reads.
				publicOutput.WriteString(dto.ContentText)
			}
			if output := publicOutput.String(); events.RedactExecutionEventText(output) != output {
				t.Error("persisted DTO output fields reconstruct a protected value")
			}

			reopened, err := journal.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range updates {
				appendMarkerWhitespaceUpdate(t, reopened, event, false)
			}
			afterReplay := markerWhitespaceRows(t, journal, len(rows))
			for i, row := range afterReplay {
				if markerWhitespacePublicDTO(t, row) != markerWhitespacePublicDTO(t, rows[i]) {
					t.Fatalf("reopening and replay changed public event %d", row.Seq)
				}
			}
		})
	}
}

func seedMarkerWhitespaceHistory(t *testing.T, state *eventjournal.State, toolHistory bool) (int, string) {
	t.Helper()
	// Titles and pending plan entries each contribute one logical field with
	// two public copies. Retain the full tool sequence for the original tab
	// regression; one bulk plan seeds the same candidate pressure elsewhere.
	entries := make([]harnessv2.PlanEntry, 0, markerWhitespaceHistorySize)
	lines := []string{"# Plan"}
	for i := 1; i <= markerWhitespaceHistorySize; i++ {
		title := markerWhitespaceHistoryTitle(i)
		if toolHistory {
			appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(uint64(i), title, ""), true)
			continue
		}
		entries = append(entries, harnessv2.PlanEntry{Content: title, Status: harnessv2.PlanEntryPending})
		lines = append(lines, "- [ ] "+title)
	}
	if toolHistory {
		return markerWhitespaceHistorySize, ""
	}
	event := modelHistoryEvent(1)
	event.Type = harnessv2.EventUpdate
	event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: entries}}
	appendMarkerWhitespaceUpdate(t, state, event, true)
	return 1, strings.Join(lines, "\n")
}

func markerWhitespaceHistoryTitle(index int) string {
	return fmt.Sprintf("Completed step %03d.", index)
}

func markerWhitespaceToolEvent(sequence uint64, title, output string) harnessv2.Event {
	event := modelHistoryEvent(sequence)
	event.Type = harnessv2.EventUpdate
	tool := &harnessv2.ToolCallUpdate{
		ToolCallID: fmt.Sprintf("whitespace-tool-%d", sequence), Title: title, Status: harnessv2.ToolCallStatusCompleted,
	}
	if output != "" {
		tool.Content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: output}}
	}
	event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateToolCallUpdate, ToolCall: tool}
	return event
}

func appendMarkerWhitespaceUpdate(t *testing.T, state *eventjournal.State, event harnessv2.Event, wantNew bool) {
	t.Helper()
	if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
		t.Fatal(err)
	}
	appended, isNew, err := state.AppendUpdateIfNew(context.Background(), event)
	if err != nil || isNew != wantNew || (appended != nil) != wantNew {
		t.Fatalf("append update %d: new=%t want=%t err=%v", event.Identity.Sequence, isNew, wantNew, err)
	}
}

func markerWhitespaceRows(t *testing.T, journal eventjournal.Journal, wantCount int) []store.ExecutionEvent {
	t.Helper()
	rows, err := journal.EventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: defaultNamespace, StreamType: store.ExecutionEventStreamTypeTask,
		StreamID: modelHistoryTaskName, Limit: store.MaxExecutionEventLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != wantCount {
		t.Fatalf("persisted row count = %d, want %d", len(rows), wantCount)
	}
	return rows
}

type markerWhitespaceDTO struct {
	Summary     string `json:"summary"`
	ContentText string `json:"contentText"`
	Content     struct {
		Title string `json:"title"`
	} `json:"content"`
}

func markerWhitespacePublicDTO(t *testing.T, row store.ExecutionEvent) markerWhitespaceDTO {
	t.Helper()
	encoded, err := json.Marshal(NewExecutionEventResponse(row))
	if err != nil {
		t.Fatal(err)
	}
	var dto markerWhitespaceDTO
	if err := json.Unmarshal(encoded, &dto); err != nil {
		t.Fatal(err)
	}
	return dto
}
