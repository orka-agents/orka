package api

import (
	"context"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
)

// Runtime completions and synthesized closures share a prompt-and-tool key.
// A losing State must not forget text already published by the winning State.
func TestExecutionEventToolDedupHistory(t *testing.T) {
	for _, collision := range []string{"runtime-wins-buffered-closure", "buffered-closure-wins-runtime", "runtime-wins-persisted-closure"} {
		for _, followKnownWinner := range []bool{false, true} {
			which := "losing-state"
			if followKnownWinner {
				which = "known-winning-state"
			}
			t.Run(collision+"/"+which, func(t *testing.T) {
				ctx := context.Background()
				journal, _ := newModelHistoryJournal(t, "")
				// Standalone prompts have no Session-wide reopen suppression.
				journal.MapContext.SessionName = ""
				live, err := journal.Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				appendModelHistoryAccepted(t, live, true)
				dedupToolAppend(t, live, dedupToolUpdate(2, harnessv2.ToolCallStatusPending, "."), true)
				competing, err := journal.Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				// Populate both buffers from an identical stream prefix; the
				// fresh State publishes one metadata-free start at sequence 3.
				// No store append deduplicates until the terminal collision.
				if collision != "runtime-wins-persisted-closure" {
					fragment := "."
					if collision == "buffered-closure-wins-runtime" {
						fragment = "pw"
					}
					fragmentEvent := dedupToolUpdate(3, harnessv2.ToolCallStatusInProgress, fragment)
					dedupToolAppend(t, live, fragmentEvent, false)
					dedupToolAppend(t, competing, fragmentEvent, true)
				}
				terminal := dedupToolUpdate(4, harnessv2.ToolCallStatusCompleted, "pw")
				winner, loser := live, competing
				switch collision {
				case "runtime-wins-buffered-closure":
					dedupToolAppend(t, live, terminal, true)
				case "buffered-closure-wins-runtime":
					if err := competing.AppendToolStreamClosuresIfNew(ctx); err != nil {
						t.Fatal(err)
					}
					terminal.Update.ToolCall.Content[0].Text = "."
					dedupToolAppend(t, live, terminal, false)
					winner, loser = competing, live
				case "runtime-wins-persisted-closure":
					dedupToolAppend(t, live, terminal, true)
					if err := competing.AppendPersistedToolClosuresIfNew(ctx, terminal.Identity.Timestamp); err != nil {
						t.Fatal(err)
					}
				}
				before := modelHistoryRows(t, journal)
				terminalRows := dedupToolTerminals(before)
				if len(terminalRows) != 1 {
					t.Fatalf("winning tool terminal count=%d, want 1", len(terminalRows))
				}
				published := NewExecutionEventResponse(terminalRows[0])
				if published.ContentText != "pw" {
					t.Fatalf("winning tool must publish harmless prefix, got %q", published.ContentText)
				}
				next := loser
				if followKnownWinner {
					next = winner
				}
				final := modelHistoryEvent(6)
				final.Type = harnessv2.EventCompleted
				final.Completed = &harnessv2.CompletedEvent{StopReason: harnessv2.ACPStopReasonEndTurn,
					Result: harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "d=synthetic-tool-fixture"}}}}
				// The buffered-closure loser meets its dedup collision and then
				// emits an assistant transcript in the ordinary sorted flush.
				if err := next.AppendBufferedStreamsIfNew(ctx, &final, 5, "d=synthetic-tool-fixture", false); err != nil {
					t.Fatal(err)
				}
				rows := modelHistoryRows(t, journal)
				if len(dedupToolTerminals(rows)) != 1 {
					t.Fatal("competing closure created a second terminal")
				}
				transcript := NewExecutionEventResponse(rows[len(rows)-1])
				if transcript.Type != events.ExecutionEventTypeModelMessage {
					t.Fatalf("last row type=%q", transcript.Type)
				}
				for _, text := range []string{transcript.ContentText, transcript.Summary} {
					if events.RedactExecutionEventText(published.ContentText+text) != published.ContentText+text {
						t.Error("winning public tool prefix plus later public transcript reconstructs a protected assignment")
					}
				}
			})
		}
	}
}

func TestExecutionEventToolDedupPreservesBenignText(t *testing.T) {
	for _, replay := range []bool{false, true} {
		name := "fresh-insert"
		if replay {
			name = "same-state-early-dedup"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			journal, state := newModelHistoryJournal(t, modelHistoryBenign)
			appendModelHistoryAccepted(t, state, true)
			tool := dedupToolUpdate(2, harnessv2.ToolCallStatusCompleted, "/workspace")
			tool.Update.ToolCall.Title = "pwd && ls"
			dedupToolAppend(t, state, tool, true)
			if replay {
				dedupToolAppend(t, state, tool, false)
			}
			const text = "pwd && ls"
			terminal := modelHistoryEvent(3)
			terminal.Type = harnessv2.EventCompleted
			terminal.Completed = &harnessv2.CompletedEvent{
				StopReason: harnessv2.ACPStopReasonEndTurn,
				Result:     harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: text}}},
			}
			row, isNew, err := state.AppendAssistantTranscriptIfNew(ctx, terminal, text, false)
			if err != nil || !isNew || row == nil {
				t.Fatalf("fresh transcript: new=%t error=%v", isNew, err)
			}
			rows := modelHistoryRows(t, journal)
			if len(rows) != 3 {
				t.Fatalf("row count=%d, want 3", len(rows))
			}
			toolDTO := NewExecutionEventResponse(rows[1])
			if toolDTO.Summary != text || toolDTO.ContentText != "/workspace" {
				t.Error("benign pwd tool was unnecessarily masked")
			}
			transcriptDTO := NewExecutionEventResponse(rows[2])
			if transcriptDTO.ContentText != text || transcriptDTO.Summary != text {
				t.Error("known fresh or locally repeated append suppressed benign pwd transcript")
			}
		})
	}
}

func dedupToolUpdate(sequence uint64, status harnessv2.ToolCallStatus, output string) harnessv2.Event {
	e := modelHistoryEvent(sequence)
	e.Type = harnessv2.EventUpdate
	kind := harnessv2.UpdateToolCallUpdate
	if status == harnessv2.ToolCallStatusPending {
		kind = harnessv2.UpdateToolCall
	}
	e.Update = &harnessv2.UpdateEvent{Kind: kind, ToolCall: &harnessv2.ToolCallUpdate{
		ToolCallID: "dedup-terminal-race", Title: "Inspect output", Kind: "shell", Status: status,
		ContentReplace: true, Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: output}},
	}}
	return e
}

func dedupToolAppend(t *testing.T, s *eventjournal.State, event harnessv2.Event, wantNew bool) {
	t.Helper()
	if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
		t.Fatal(err)
	}
	row, isNew, err := s.AppendUpdateIfNew(context.Background(), event)
	if err != nil || isNew != wantNew || (row != nil) != wantNew {
		t.Fatalf("tool append seq=%d: new=%t want=%t error=%v", event.Identity.Sequence, isNew, wantNew, err)
	}
}

func dedupToolTerminals(rows []store.ExecutionEvent) []store.ExecutionEvent {
	var result []store.ExecutionEvent
	for _, row := range rows {
		if row.Type == events.ExecutionEventTypeToolCallCompleted || row.Type == events.ExecutionEventTypeToolCallFailed {
			result = append(result, row)
		}
	}
	return result
}
