package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestExecutionEventOmittedToolHistoryMatchesSQLiteDTO(t *testing.T) {
	for _, mode := range []string{"upstream", "overflow", "multiple blocks", "metadata pending", "retained"} {
		for _, title := range []string{"Inspect", ""} {
			t.Run(mode+"/title="+title, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, "")
				event := markerWhitespaceToolEvent(1, title, "pwd")
				event.Update.ToolCall.Kind = "shell"
				firstRow := 0
				switch mode {
				case "upstream":
					start := markerWhitespaceToolEvent(1, title, "pwd")
					start.Update.ToolCall.Kind = "shell"
					start.Update.ToolCall.Status = harnessv2.ToolCallStatusInProgress
					appendMarkerWhitespaceUpdate(t, state, start, true)
					firstRow = 1
					event.Identity.Sequence = 2
					event.Update.ToolCall.Content = nil
					event.Update.ToolCall.ContentOmitted = true
				case "overflow":
					event.Update.ToolCall.Content[0].Text = strings.Repeat("界", 32768) + "pwd"
				case "multiple blocks":
					event.Update.ToolCall.Content = append(event.Update.ToolCall.Content, harnessv2.ContentBlock{Type: harnessv2.ContentBlockText, Text: "second"})
				case "metadata pending":
					event.Update.ToolCall.Status = harnessv2.ToolCallStatusInProgress
					event.Update.ToolCall.Title = "pwd"
					event.Update.ToolCall.Kind = "pwd"
				}
				appendMarkerWhitespaceUpdate(t, state, event, true)
				appendMarkerWhitespaceUpdate(t, state, event, false)
				appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(event.Identity.Sequence+1, "Inspect", "=fixture-value"), true)
				rows := markerWhitespaceRows(t, journal, firstRow+2)
				encoded, err := json.Marshal(NewExecutionEventResponse(rows[firstRow]))
				if err != nil {
					t.Fatal(err)
				}
				var first struct {
					Summary, ContentText, ToolName string
					Truncation                     *events.ExecutionEventTruncation
					Content                        map[string]any
				}
				if err := json.Unmarshal(encoded, &first); err != nil {
					t.Fatal(err)
				}
				wantSummary := title
				if title == "" {
					wantSummary = "Tool call completed"
				}
				wantLater := "=fixture-value"
				if mode == "retained" {
					if title == "" {
						wantSummary = "pwd"
					}
					if first.ContentText != "pwd" {
						t.Fatal("retained output missing from SQLite DTO")
					}
					wantLater = events.ExecutionEventRedactedValue
				} else if first.ContentText != "" {
					t.Fatal("unpublished output present in SQLite DTO")
				}
				if mode == "metadata pending" {
					wantSummary = "Tool call in progress"
					if _, ok := first.Content["title"]; ok || first.Content["toolKind"] != nil || first.ToolName != "" ||
						first.Content["metadataOmitted"] != "streamed_metadata_pending_completion_redaction" {
						t.Fatal("pending metadata reached the public DTO")
					}
				} else if first.Content["title"] != title || first.Content["toolKind"] != "shell" || first.ToolName != "shell" {
					t.Fatal("public tool metadata changed")
				}
				if first.Summary != wantSummary {
					t.Errorf("summary = %q, want %q", first.Summary, wantSummary)
				}
				assertOmittedToolMetadata(t, mode, first.Truncation, first.Content)
				if later := markerWhitespacePublicDTO(t, rows[firstRow+1]); later.ContentText != wantLater {
					t.Errorf("later output = %q, want %q", later.ContentText, wantLater)
				}
			})
		}
	}
}

func assertOmittedToolMetadata(t *testing.T, mode string, truncation *events.ExecutionEventTruncation, content map[string]any) {
	t.Helper()
	if mode == "upstream" || mode == "overflow" {
		if truncation == nil || !truncation.ContentTextTruncated ||
			content["contentOmitted"] != "streamed_text_truncated_or_omitted" {
			t.Fatal("omission metadata missing from SQLite DTO")
		}
	} else if truncation != nil {
		t.Fatal("unexpected truncation metadata")
	}
	if mode == "multiple blocks" && content["contentOmitted"] != "streamed_text_multiple_blocks_omitted" {
		t.Fatal("multiple-block omission reason changed")
	}
}
