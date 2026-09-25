package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/orka-agents/orka/internal/events"
)

func TestExecutionEventFallbackUsageAndContextPreserveToolCopies(t *testing.T) {
	for _, kind := range []string{"usage", "context"} {
		for _, title := range []string{"pwd", "pwd && ls"} {
			for _, count := range []int{0, 2} {
				t.Run(fmt.Sprintf("%s/title=%s/history=%d", kind, title, count), func(t *testing.T) {
					journal, _ := newModelHistoryJournal(t, "gpt-test")
					journal.MapContext.Provider = "openai"
					state, err := journal.Open(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					appendModelHistoryAccepted(t, state, true)
					appendModelHistoryStep(t, state, kind, "", true)
					for i := range count {
						appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(uint64(3+i), fmt.Sprintf("Read file %d", i), "ordinary output\n"), true)
					}
					tool := markerWhitespaceToolEvent(uint64(3+count), title, "README.md\ncmd\ninternal\n")
					tool.Update.ToolCall.Kind = "shell"
					for _, wantNew := range []bool{true, false, false} {
						appendMarkerWhitespaceUpdate(t, state, tool, wantNew)
					}
					rows := markerWhitespaceRows(t, journal, 3+count)
					usage, last := NewExecutionEventResponse(rows[1]), NewExecutionEventResponse(rows[len(rows)-1])
					var content struct{ Title, ToolKind string }
					if err := json.Unmarshal(last.Content, &content); err != nil {
						t.Fatal(err)
					}
					if usage.Provider != "openai" || usage.Model != "gpt-test" || usage.Summary == events.ExecutionEventRedactedValue {
						t.Error("safe usage metadata or generated summary was suppressed")
					}
					if last.Summary != title || last.ToolName != "shell" || content.Title != title || content.ToolKind != "shell" || last.ContentText != "README.md\ncmd\ninternal\n" {
						t.Errorf("tool public copies changed after %s and replay", kind)
					}
					if kind == "usage" && usage.InputTokens != 1 {
						t.Error("token usage changed")
					}
					if kind == "context" && (usage.ContextWindowUsed == nil || *usage.ContextWindowUsed != 1 || usage.ContextWindowSize == nil || *usage.ContextWindowSize != 10) {
						t.Error("context telemetry changed")
					}
				})
			}
		}
	}
}

func TestExecutionEventFallbackUsageFallbackStillProtectsLaterAssignments(t *testing.T) {
	for _, kind := range []string{"usage", "context"} {
		t.Run(kind, func(t *testing.T) {
			journal, _ := newModelHistoryJournal(t, "gpt-test")
			journal.MapContext.Provider = "openai"
			state, err := journal.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			appendModelHistoryAccepted(t, state, true)
			appendModelHistoryStep(t, state, kind, "", true)
			appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(3, "Inspect", "=fixture-value"), true)
			rows := markerWhitespaceRows(t, journal, 3)
			last := NewExecutionEventResponse(rows[2])
			want := "=fixture-value"
			if kind == "usage" {
				want = events.ExecutionEventRedactedValue
			}
			if last.ContentText != want {
				t.Errorf("later assignment output=%q, want %q", last.ContentText, want)
			}
		})
	}
}

func TestExecutionEventFallbackAcceptedMetadataDoesNotInventToolCopies(t *testing.T) {
	for _, title := range []string{"Inspect output", ""} {
		t.Run("title="+title, func(t *testing.T) {
			journal, _ := newModelHistoryJournal(t, "gpt-test")
			journal.MapContext.Provider = "openai"
			state, err := journal.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			appendModelHistoryAccepted(t, state, true)
			tool := markerWhitespaceToolEvent(2, title, "ken=X\tto")
			tool.Update.ToolCall.Kind = "shell"
			for _, wantNew := range []bool{true, false} {
				appendMarkerWhitespaceUpdate(t, state, tool, wantNew)
				state, err = journal.Open(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			}
			rows := markerWhitespaceRows(t, journal, 2)
			last := NewExecutionEventResponse(rows[1])
			var content struct{ Title, ToolKind string }
			if err := json.Unmarshal(last.Content, &content); err != nil {
				t.Fatal(err)
			}
			wantTitle, wantKind, wantOutput, wantSummary := title, "shell", "ken=X\tto", title
			if title == "" {
				wantKind, wantOutput, wantSummary = events.ExecutionEventRedactedValue, events.ExecutionEventRedactedValue, events.ExecutionEventRedactedValue
			}
			if last.Summary != wantSummary || last.ToolName != wantKind || content.Title != wantTitle || content.ToolKind != wantKind || last.ContentText != wantOutput {
				t.Errorf("public tool copies mismatch: title=%q summary=%q kind=%q output=%q", content.Title, last.Summary, last.ToolName, last.ContentText)
			}
		})
	}
}

func TestExecutionEventFallbackMarkerCannotReuseMiddleCopy(t *testing.T) {
	for _, twoCopies := range []bool{false, true} {
		for _, reopen := range []bool{false, true} {
			name := map[bool]string{false: "one", true: "two"}[twoCopies] + map[bool]string{false: "/fresh", true: "/reopened"}[reopen]
			t.Run(name, func(t *testing.T) {
				journal, _ := newModelHistoryJournal(t, "gpt-test")
				journal.MapContext.Provider = "openai"
				state, err := journal.Open(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				appendModelHistoryAccepted(t, state, true)
				title := "Inspect output"
				if twoCopies {
					title = ""
				}
				first := markerWhitespaceToolEvent(2, title, "s")
				first.Update.ToolCall.Kind = "shell"
				appendMarkerWhitespaceUpdate(t, state, first, true)
				if reopen {
					state, err = journal.Open(context.Background())
					if err != nil {
						t.Fatal(err)
					}
				}
				lastTool := markerWhitespaceToolEvent(3, "pa", "")
				lastTool.Update.ToolCall.Kind = "word=fixture-value"
				appendMarkerWhitespaceUpdate(t, state, lastTool, true)
				rows := markerWhitespaceRows(t, journal, 3)
				last := NewExecutionEventResponse(rows[2])
				wantSummary, wantKind := "pa", "word=fixture-value"
				// Session recovery intentionally masks all later runtime text;
				// logical field boundaries are not persisted for later turns.
				if twoCopies || reopen {
					wantSummary, wantKind = events.ExecutionEventRedactedValue, events.ExecutionEventRedactedValue
				}
				if last.Summary != wantSummary || last.ToolName != wantKind {
					t.Errorf("public copies differ: summary=%q kind=%q", last.Summary, last.ToolName)
				}
			})
		}
	}
}
