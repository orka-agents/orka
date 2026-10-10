package api

import (
	"context"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestExecutionEventTranscriptHistoryUsesPublishedCopies(t *testing.T) {
	for _, test := range []struct {
		name, transcript, contentText, summary string
		originalChars                          int
		maskLater                              bool
	}{
		{
			name: "unpublished-ascii-tail", transcript: strings.Repeat("x", 32768) + "pwd",
			contentText: strings.Repeat("x", 32767) + "…", summary: strings.Repeat("x", 4095) + "…", originalChars: 32771,
		},
		{
			name: "unpublished-unicode-tail", transcript: strings.Repeat("界", 32768) + "pwd",
			contentText: strings.Repeat("界", 32767) + "…", summary: strings.Repeat("界", 4095) + "…", originalChars: 32771,
		},
		{
			name: "visible-content-only", transcript: strings.Repeat("界", 4096) + "pwd",
			contentText: strings.Repeat("界", 4096) + "pwd", summary: strings.Repeat("界", 4095) + "…", maskLater: true,
		},
		{
			name: "visible-summary-only", transcript: strings.Repeat("\t", 32768) + "pwd",
			contentText: strings.Repeat("\t", 32767) + "…", summary: "pwd", originalChars: 32771, maskLater: true,
		},
		{name: "visible-both", transcript: "pwd", contentText: "pwd", summary: "pwd", maskLater: true},
		{
			name: "two-public-copies", transcript: "ken=X\tto",
			contentText: events.ExecutionEventRedactedValue, summary: events.ExecutionEventRedactedValue,
		},
	} {
		for _, path := range []string{"terminal", "stream-closure", "incremental"} {
			t.Run(test.name+"/"+path, func(t *testing.T) {
				ctx := context.Background()
				journal, state := newModelHistoryJournal(t, "")
				runes := []rune(test.transcript)
				var chunk harnessv2.Event
				firstRow := 0
				for offset := 0; offset < len(runes); offset += 1024 {
					text := string(runes[offset:min(offset+1024, len(runes))])
					chunk = modelHistoryEvent(uint64(offset/1024 + 1))
					chunk.Type = harnessv2.EventUpdate
					chunk.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateAssistantMessageChunk,
						AssistantMessage: &harnessv2.AssistantMessageChunk{Text: text}}
					appendMarkerWhitespaceUpdate(t, state, chunk, false)
					if path == "incremental" && offset == 0 {
						// Publish a prefix, then the full transcript at a later identity.
						if _, isNew, err := state.AppendAssistantStreamClosureIfNew(ctx, chunk, text, false); err != nil || !isNew {
							t.Fatalf("append prefix: new=%t err=%v", isNew, err)
						}
						firstRow++
					}
				}
				event := modelHistoryEvent(chunk.Identity.Sequence + 1)
				appendTranscript := state.AppendAssistantStreamClosureIfNew
				if path == "terminal" {
					event.Type = harnessv2.EventCompleted
					event.Completed = &harnessv2.CompletedEvent{StopReason: harnessv2.ACPStopReasonEndTurn,
						Result: harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: test.transcript}}}}
					appendTranscript = state.AppendAssistantTranscriptIfNew
				} else {
					event.Type, event.Update = chunk.Type, chunk.Update
				}
				if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
					t.Fatal(err)
				}
				for _, wantNew := range []bool{true, false} {
					appended, isNew, err := appendTranscript(ctx, event, test.transcript, false)
					if err != nil || isNew != wantNew || (appended != nil) != wantNew {
						t.Fatalf("append transcript: new=%t want=%t err=%v", isNew, wantNew, err)
					}
				}
				const later = "=fixture-value"
				appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(event.Identity.Sequence+1, "Inspect", later), true)
				rows := markerWhitespaceRows(t, journal, firstRow+2)
				first := NewExecutionEventResponse(rows[firstRow])
				if first.ContentText != test.contentText || first.Summary != test.summary {
					t.Fatal("persisted public transcript differs from its expected content/summary boundaries")
				}
				if test.originalChars == 0 {
					if first.Truncation != nil {
						t.Fatalf("unexpected truncation metadata: %+v", first.Truncation)
					}
				} else if first.Truncation == nil || *first.Truncation != (events.ExecutionEventTruncation{
					ContentTextTruncated: true, ContentTextOriginalChars: test.originalChars,
				}) {
					t.Fatalf("truncation metadata = %+v, want content truncated with %d original runes", first.Truncation, test.originalChars)
				}
				wantLater := later
				if test.maskLater {
					wantLater = events.ExecutionEventRedactedValue
				}
				if second := markerWhitespacePublicDTO(t, rows[firstRow+1]); second.ContentText != wantLater {
					t.Errorf("later public output = %q, want %q", second.ContentText, wantLater)
				}
			})
		}
	}
}

func TestExecutionEventToolOutputHistoryUsesPublishedCopies(t *testing.T) {
	// The raw stream fits the journal's 32K bound. Redaction expands the first
	// assignment, pushing the harmless trailing marker out of the public text.
	for _, test := range []struct {
		name, output, contentText, summary string
		originalChars                      int
		maskLater, summaryMasksLater       bool
		twoCopiesSensitive                 bool
	}{
		{
			name: "unpublished-expanded-tail", output: "pwd=x\n" + strings.Repeat("x", 32759) + "pwd",
			contentText: "pwd=[REDACTED]\n" + strings.Repeat("x", 32752) + "…",
			summary:     "pwd=[REDACTED] " + strings.Repeat("x", 4080) + "…", originalChars: 32777,
		},
		{
			name: "unpublished-expanded-unicode-tail", output: "pwd=x\n" + strings.Repeat("界", 32759) + "pwd",
			contentText: "pwd=[REDACTED]\n" + strings.Repeat("界", 32752) + "…",
			summary:     "pwd=[REDACTED] " + strings.Repeat("界", 4080) + "…", originalChars: 32777,
		},
		{
			name: "visible-content-only", output: strings.Repeat("x", 4096) + "pwd",
			contentText: strings.Repeat("x", 4096) + "pwd", summary: strings.Repeat("x", 4095) + "…", maskLater: true,
		},
		{name: "visible-both", output: "pwd", contentText: "pwd", summary: "pwd", maskLater: true},
		{name: "visible-normalized-summary", output: "pwd\u00a0", contentText: "pwd\u00a0", summary: "pwd", summaryMasksLater: true},
		{
			name: "unpublished-normalized-summary-tail", output: strings.Repeat("x", 4096) + "pwd\u00a0",
			contentText: strings.Repeat("x", 4096) + "pwd\u00a0", summary: strings.Repeat("x", 4095) + "…",
		},
		{name: "two-public-copies", output: "ken=X\tto", contentText: "ken=X\tto", summary: "ken=X to", twoCopiesSensitive: true},
	} {
		for _, title := range []string{"Inspect", ""} {
			t.Run(test.name+"/title="+title, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, "")
				event := markerWhitespaceToolEvent(1, title, test.output)
				appendMarkerWhitespaceUpdate(t, state, event, true)
				appendMarkerWhitespaceUpdate(t, state, event, false)
				const later = "=fixture-value"
				appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(2, "Inspect", later), true)
				rows := markerWhitespaceRows(t, journal, 2)
				first := NewExecutionEventResponse(rows[0])
				finalContentText, outputSummary := test.contentText, test.summary
				if title != "" {
					outputSummary = title
				} else if test.twoCopiesSensitive {
					finalContentText, outputSummary = events.ExecutionEventRedactedValue, events.ExecutionEventRedactedValue
				}
				if first.ContentText != finalContentText || first.Summary != outputSummary {
					t.Fatal("public tool content/summary did not match the published cutoff")
				}
				if test.originalChars == 0 {
					if first.Truncation != nil {
						t.Fatalf("unexpected truncation metadata: %+v", first.Truncation)
					}
				} else if first.Truncation == nil || *first.Truncation != (events.ExecutionEventTruncation{
					ContentTextTruncated: true, ContentTextOriginalChars: test.originalChars,
				}) {
					t.Fatalf("unexpected truncation metadata: %+v", first.Truncation)
				}
				wantLater := later
				if test.maskLater || (title == "" && test.summaryMasksLater) {
					wantLater = events.ExecutionEventRedactedValue
				}
				if second := markerWhitespacePublicDTO(t, rows[1]); second.ContentText != wantLater {
					t.Errorf("later public output = %q, want %q", second.ContentText, wantLater)
				}
			})
		}
	}
}

func TestExecutionEventTruncationKeepsBoundarySeparator(t *testing.T) {
	for _, test := range []struct {
		name, prefix, padding string
		limit                 int
		contentText           bool
	}{
		{name: "transcript-summary-ascii", padding: "x", limit: 4096},
		{name: "transcript-summary-unicode", padding: "界", limit: 4096},
		{name: "transcript-content", padding: "界", limit: 32768, contentText: true},
		{name: "tool-output-summary", padding: "界", limit: 4096},
		{name: "diagnostic-message", prefix: "notice: ", padding: "x", limit: 4096},
		{name: "failed-message", prefix: "notice: ", padding: "x", limit: 4096},
		{name: "plan-summary", prefix: "Plan in progress (0/1 complete): ", padding: "x", limit: 4096},
	} {
		for _, beforeEllipsis := range []bool{false, true} {
			name := test.name + "/pw-at-limit"
			if beforeEllipsis {
				name = test.name + "/pw-before-ellipsis"
			}
			t.Run(name, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, "")
				paddingRunes := test.limit - len([]rune(test.prefix)) - 2
				wantSuffix := "p…"
				if beforeEllipsis {
					paddingRunes--
					wantSuffix = "pw…"
				}
				padding := strings.Repeat(test.padding, paddingRunes)
				value := padding + "pw harmless suffix."
				wantFirst := test.prefix + padding + wantSuffix
				event := modelHistoryEvent(1)
				appendEvent := state.AppendUpdateIfNew
				event.Type = harnessv2.EventUpdate
				switch test.name {
				case "transcript-summary-ascii", "transcript-summary-unicode", "transcript-content":
					event.Type = harnessv2.EventCompleted
					event.Completed = &harnessv2.CompletedEvent{
						StopReason: harnessv2.ACPStopReasonEndTurn,
						Result:     harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: value}}},
					}
				case "tool-output-summary":
					event = markerWhitespaceToolEvent(1, "", value)
				case "diagnostic-message":
					event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateDiagnostic,
						Diagnostic: &harnessv2.DiagnosticUpdate{Code: "notice", Message: value}}
				case "failed-message":
					event.Type = harnessv2.EventFailed
					event.Failed = &harnessv2.FailedEvent{Code: "notice", Message: value}
					appendEvent = state.AppendPromptLifecycleIfNew
				case "plan-summary":
					event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan,
						Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{Content: value, Status: harnessv2.PlanEntryInProgress}}}}
				}
				if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
					t.Fatal(err)
				}
				for _, wantNew := range []bool{true, false} {
					var isNew bool
					var err error
					if strings.HasPrefix(test.name, "transcript-") {
						_, isNew, err = state.AppendAssistantTranscriptIfNew(context.Background(), event, value, false)
					} else {
						_, isNew, err = appendEvent(context.Background(), event)
					}
					if err != nil || isNew != wantNew {
						t.Fatalf("append source: new=%t want=%t err=%v", isNew, wantNew, err)
					}
				}
				const later = "d=fixture-value"
				appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(2, "Inspect", later), true)
				rows := markerWhitespaceRows(t, journal, 2)
				first := markerWhitespacePublicDTO(t, rows[0])
				published := first.Summary
				if test.contentText {
					published = first.ContentText
				}
				if published != wantFirst {
					runes := []rune(published)
					t.Fatalf("public cutoff: %d runes, suffix %q; want suffix %q", len(runes), string(runes[max(0, len(runes)-4):]), wantSuffix)
				}
				second := markerWhitespacePublicDTO(t, rows[1])
				if second.ContentText != later {
					t.Errorf("safe later public output = %q, want %q", second.ContentText, later)
				}
				// Join the exact strings returned by SQLite and the API DTO. The
				// ellipsis must not be stripped to manufacture a pwd assignment.
				joined := published + second.ContentText
				if events.RedactExecutionEventText(joined) != joined {
					t.Error("actual public cutoff and later output reconstruct a protected assignment")
				}
				if events.RedactExecutionEventText("pw"+later) == "pw"+later {
					t.Fatal("synthetic fixture must be sensitive without the truncation separator")
				}
			})
		}
	}
}
