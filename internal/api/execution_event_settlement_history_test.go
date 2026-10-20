package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
)

func TestExecutionEventSettlementDedupHistory(t *testing.T) {
	for _, useCompetingState := range []bool{false, true} {
		name := "known-winner-history"
		if useCompetingState {
			name = "competing-writer-history"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			journal, _ := newModelHistoryJournal(t, modelHistoryBenign)
			journal.MapContext.SessionName = ""
			winner, err := journal.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			appendModelHistoryAccepted(t, winner, true)
			// Open after acceptance so the synthetic terminal is the first
			// store duplicate encountered by the competing State.
			competing, err := journal.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			row, isNew, err := winner.AppendPromptStreamFailureIfNew(ctx, modelHistoryEvent(2).Identity.Timestamp, "pw")
			if err != nil || !isNew || row == nil {
				t.Fatalf("append winning failure: new=%t error=%v", isNew, err)
			}
			published := NewExecutionEventResponse(*row)
			var content struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(published.Content, &content); err != nil {
				t.Fatal(err)
			}
			if content.Message != "pw" {
				t.Fatal("winning failure must publish the harmless assignment prefix")
			}
			state := winner
			if useCompetingState {
				state = competing
			}
			row, isNew, err = state.AppendPromptSettlementIfNew(ctx, modelHistorySettlement(t, "settlement-completed"), "")
			if err != nil || isNew || row != nil {
				t.Fatalf("duplicate settlement: new=%t error=%v", isNew, err)
			}
			// The journal API permits transcript publication after settlement.
			// A losing writer must not forget the already-public prefix.
			appendSettlementHistoryTranscript(t, state, "d=synthetic-settlement-fixture")
			rows := modelHistoryRows(t, journal)
			if len(rows) != 3 {
				t.Fatalf("persisted rows = %d, want accepted, failure and transcript", len(rows))
			}
			transcript := NewExecutionEventResponse(rows[2])
			if transcript.ContentText != events.ExecutionEventRedactedValue || transcript.Summary != events.ExecutionEventRedactedValue {
				t.Error("later public transcript reconstructs an assignment with the winning failure prefix")
			}
		})
	}
}

func TestExecutionEventNewSettlementPreservesBenignText(t *testing.T) {
	journal, state := newModelHistoryJournal(t, modelHistoryBenign)
	appendModelHistoryAccepted(t, state, true)
	row, isNew, err := state.AppendPromptSettlementIfNew(context.Background(), modelHistorySettlement(t, "settlement-completed"), "")
	if err != nil || !isNew || row == nil {
		t.Fatalf("new settlement: new=%t error=%v", isNew, err)
	}
	const text = "ordinary transcript"
	appendSettlementHistoryTranscript(t, state, text)
	rows := modelHistoryRows(t, journal)
	if len(rows) != 3 {
		t.Fatalf("persisted rows = %d, want accepted, settlement and transcript", len(rows))
	}
	transcript := NewExecutionEventResponse(rows[2])
	if transcript.ContentText != text || transcript.Summary != text {
		t.Error("new settlement suppressed benign transcript text")
	}
}

func appendSettlementHistoryTranscript(t *testing.T, state *eventjournal.State, text string) {
	t.Helper()
	terminal := modelHistoryEvent(2)
	terminal.Type = harnessv2.EventCompleted
	terminal.Completed = &harnessv2.CompletedEvent{
		StopReason: harnessv2.ACPStopReasonEndTurn,
		Result: harnessv2.PromptResult{Content: []harnessv2.ContentBlock{
			{Type: harnessv2.ContentBlockText, Text: text},
		}},
	}
	if err := terminal.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
		t.Fatal(err)
	}
	row, isNew, err := state.AppendAssistantTranscriptIfNew(context.Background(), terminal, text, false)
	if err != nil || !isNew || row == nil {
		t.Fatalf("append following transcript: new=%t error=%v", isNew, err)
	}
}
