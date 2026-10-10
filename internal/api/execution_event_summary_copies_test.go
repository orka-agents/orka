package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventPrefixedSummaryUsesPublishedCopies(t *testing.T) {
	for _, kind := range []string{"diagnostic", "failed", "outcome-unknown", "stream-failure", "plan"} {
		prefix := "notice: "
		switch kind {
		case "stream-failure":
			prefix = "prompt_stream_error: "
		case "plan":
			prefix = "Plan in progress (0/1 complete): "
		}
		for _, test := range []struct {
			name, value, summary, later string
			maskLater                   bool
		}{
			{
				name:  "unpublished-normalized-assignment",
				value: strings.Repeat("x", 4070) + "pwd\u00a0=fixture-value",
			},
			{
				name:    "prefix-hides-normalized-tail",
				value:   strings.Repeat("x", 4096-len(prefix)) + "pwd\u00a0=",
				summary: prefix + strings.Repeat("x", 4095-len(prefix)) + "…", later: "fixture-value",
			},
			{
				name:    "visible-normalized-tail",
				value:   strings.Repeat("x", 4095-len(prefix)-2) + "pa\u00a0",
				summary: prefix + strings.Repeat("x", 4095-len(prefix)-2) + "pa", later: "ssword=fixture-value", maskLater: true,
			},
			{name: "raw-tail-remains-public", value: "pa", summary: prefix + "pa", later: "ssword=fixture-value", maskLater: true},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, "")
				firstRow, sequence := 0, uint64(1)
				if kind == "stream-failure" {
					appendModelHistoryAccepted(t, state, true)
					firstRow, sequence = 1, 2
				}
				if test.name == "unpublished-normalized-assignment" {
					if kind != "plan" {
						test.value = strings.Repeat("x", 4096) + "pwd\u00a0=fixture-value"
					}
					test.summary = prefix + strings.Repeat("x", 4095-len(prefix)) + "…"
					test.later = "ordinary output."
				}
				for _, wantNew := range []bool{true, false} {
					appendSummaryCopySource(t, state, sequence, kind, "notice", test.value, wantNew)
				}
				appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(sequence+1, "Inspect", test.later), true)
				rows := markerWhitespaceRows(t, journal, firstRow+2)
				first := NewExecutionEventResponse(rows[firstRow])
				if first.Summary != test.summary {
					t.Errorf("summary did not preserve the exact prefixed publication (got %d runes, want %d)", len([]rune(first.Summary)), len([]rune(test.summary)))
				}
				var content struct{ Code, Message string }
				if err := json.Unmarshal(first.Content, &content); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "diagnostic":
					if first.ContentText != test.value || content.Message != "" || content.Code != "notice" {
						t.Error("diagnostic lost its public contentText/code or invented a raw message copy")
					}
				case "plan":
					wantDocument := "# Plan\n- [ ] " + strings.TrimSpace(test.value) + " _(in progress)_"
					planStore := journal.EventStore.(store.PlanStore)
					plan, err := planStore.GetPlan(context.Background(), defaultNamespace, modelHistoryTaskName)
					if err != nil || plan.PlanDocument != wantDocument || plan.Summary != test.summary || first.ContentText != wantDocument {
						t.Errorf("plan/event raw documents or summaries differ from publication: %v", err)
					}
				default:
					if content.Message != test.value || first.ContentText != "" {
						t.Error("failure lost its full public raw JSON message")
					}
				}
				wantLater := test.later
				if test.maskLater {
					wantLater = events.ExecutionEventRedactedValue
				}
				if got := markerWhitespacePublicDTO(t, rows[firstRow+1]).ContentText; got != wantLater {
					t.Errorf("later output = %q, want %q", got, wantLater)
				}
			})
		}
	}
}

func TestExecutionEventSummaryUsesFinalCodePrefix(t *testing.T) {
	for _, test := range []struct{ name, code, publicCode, prefix string }{
		{name: "normalized", code: "note\tcode", publicCode: "note\tcode", prefix: "note code: "},
		{name: "url-removed", code: "?discard=1", prefix: ": "},
		{name: "whitespace-after-url-removal", code: "notice ?discard=1", publicCode: "notice ", prefix: "notice : "},
		{name: "unicode", code: "界", publicCode: "界", prefix: "界: "},
	} {
		for _, kind := range []string{"diagnostic", "failed", "outcome-unknown"} {
			t.Run(test.name+"/"+kind, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, "")
				padding := 4096 - len([]rune(test.prefix))
				value := strings.Repeat("x", padding) + "pwd\u00a0="
				for _, wantNew := range []bool{true, false} {
					appendSummaryCopySource(t, state, 1, kind, test.code, value, wantNew)
				}
				appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(2, "Inspect", "fixture-value"), true)
				rows := markerWhitespaceRows(t, journal, 2)
				first := NewExecutionEventResponse(rows[0])
				if first.Summary != test.prefix+strings.Repeat("x", padding-1)+"…" {
					t.Error("summary did not use its final code prefix before truncation")
				}
				var content struct{ Code, Message string }
				if err := json.Unmarshal(first.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.Code != test.publicCode || (kind == "diagnostic" && first.ContentText != value) || (kind != "diagnostic" && content.Message != value) {
					t.Error("code/message raw publication changed")
				}
				if got := markerWhitespacePublicDTO(t, rows[1]).ContentText; got != "fixture-value" {
					t.Errorf("unpublished normalized tail suppressed later output: %q", got)
				}
			})
		}
	}
}

func TestExecutionEventHistoryRedactsCodeAndMessageTogether(t *testing.T) {
	for _, kind := range []string{"diagnostic", "failed", "outcome-unknown"} {
		t.Run(kind, func(t *testing.T) {
			journal, state := newModelHistoryJournal(t, "")
			appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(1, "Inspect", "pwd"), true)
			// A shorter masked code would expose the normalized pa tail if the
			// history check masked only code rather than both logical fields.
			code := "=fixture-value. " + strings.Repeat("x", 40)
			message := strings.Repeat("x", 4069) + "pa\u00a0"
			for _, wantNew := range []bool{true, false} {
				appendSummaryCopySource(t, state, 2, kind, code, message, wantNew)
			}
			appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(3, "Inspect", "ssword=fixture-value"), true)
			appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(4, "Inspect", "ordinary output."), true)
			rows := markerWhitespaceRows(t, journal, 4)
			first := NewExecutionEventResponse(rows[1])
			var content struct{ Code, Message string }
			if err := json.Unmarshal(first.Content, &content); err != nil {
				t.Fatal(err)
			}
			if content.Code != events.ExecutionEventRedactedValue || first.Summary != "[REDACTED]: [REDACTED]" {
				t.Error("history must mask both code and message before publishing the summary")
			}
			if kind == "diagnostic" {
				if first.ContentText != events.ExecutionEventRedactedValue || content.Message != "" {
					t.Error("diagnostic must mask contentText without inventing a raw JSON message")
				}
			} else if content.Message != events.ExecutionEventRedactedValue || first.ContentText != "" {
				t.Error("failure must mask the raw JSON message without inventing contentText")
			}
			if got := markerWhitespacePublicDTO(t, rows[2]).ContentText; got != events.ExecutionEventRedactedValue {
				t.Errorf("historical assignment marker failed to mask later output: %q", got)
			}
			if got := markerWhitespacePublicDTO(t, rows[3]).ContentText; got != "ordinary output." {
				t.Errorf("masking and replay suppressed benign output: %q", got)
			}
		})
	}
}

func TestExecutionEventSummaryChecksHistoryAndCurrentCopies(t *testing.T) {
	for _, kind := range []string{"diagnostic", "failed", "outcome-unknown", "stream-failure", "plan"} {
		for _, source := range []string{"history", "current"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, "")
				sequence, firstRow := uint64(1), 0
				if kind == "stream-failure" {
					appendModelHistoryAccepted(t, state, true)
					sequence, firstRow = 2, 1
				}
				message := "pa\u00a0"
				if source == "history" {
					appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(sequence, "Inspect", "ssword=fixture-value. "), true)
					sequence++
					firstRow++
				} else {
					// Only the normalized summary ends in pa; it must be checked
					// against the other actual copy of this same message.
					message = "ssword=fixture-value. " + message
				}
				for _, wantNew := range []bool{true, false} {
					appendSummaryCopySource(t, state, sequence, kind, "notice", message, wantNew)
				}
				first := NewExecutionEventResponse(markerWhitespaceRows(t, journal, firstRow+1)[firstRow])
				wantSummary := "[REDACTED]: [REDACTED]"
				if kind == "plan" {
					wantSummary = "Plan in progress (0/1 complete): [REDACTED]"
				}
				if first.Summary != wantSummary {
					t.Errorf("same-event summary escaped copy/history check: %q", first.Summary)
				}
			})
		}
	}
}

func TestExecutionEventPlanPrefixSurvivesHistoryRedaction(t *testing.T) {
	journal, state := newModelHistoryJournal(t, "")
	appendMarkerWhitespaceUpdate(t, state, markerWhitespaceToolEvent(1, "Inspect", "pwd"), true)
	event := modelHistoryEvent(2)
	event.Type = harnessv2.EventUpdate
	event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
		{Content: "done", Status: harnessv2.PlanEntryCompleted},
		{Content: "=fixture-value. " + strings.Repeat("x", 4069) + "pa\u00a0", Status: harnessv2.PlanEntryInProgress},
		{Content: "next", Status: harnessv2.PlanEntryInProgress},
	}}}
	for _, wantNew := range []bool{true, false} {
		projection := state.ProjectPlanUpdate(*event.Update.Plan)
		appended, isNew, err := state.AppendPlanUpdateIfNew(context.Background(), event, &store.PlanState{
			Namespace: defaultNamespace, TaskName: modelHistoryTaskName,
			Summary: projection.Summary, PlanDocument: projection.Document,
		})
		if err != nil || isNew != wantNew || (appended != nil) != wantNew {
			t.Fatalf("append plan: new=%t want=%t err=%v", isNew, wantNew, err)
		}
	}
	first := NewExecutionEventResponse(markerWhitespaceRows(t, journal, 2)[1])
	const wantSummary = "Plan in progress (1/3 complete): [REDACTED]"
	plan, err := journal.EventStore.(store.PlanStore).GetPlan(context.Background(), defaultNamespace, modelHistoryTaskName)
	if err != nil || plan.Summary != wantSummary || first.Summary != wantSummary {
		t.Fatalf("masked entry changed the count-derived plan prefix: %v", err)
	}
	var content struct {
		TotalEntries, CompletedEntries, InProgressEntries, ProgressPct int
		GoalComplete                                                   bool
	}
	if err := json.Unmarshal(first.Content, &content); err != nil {
		t.Fatal(err)
	}
	if content.TotalEntries != 3 || content.CompletedEntries != 1 || content.InProgressEntries != 2 || content.ProgressPct != 33 || content.GoalComplete {
		t.Errorf("redaction changed plan numeric fields: %+v", content)
	}
}

func TestExecutionEventSummaryCopiesSurviveReplay(t *testing.T) {
	for _, kind := range []string{"diagnostic", "failed", "outcome-unknown", "stream-failure", "plan"} {
		t.Run(kind, func(t *testing.T) {
			journal, state := newModelHistoryJournal(t, "")
			firstRow, sequence := 0, uint64(1)
			prefix := ".: "
			switch kind {
			case "stream-failure":
				appendModelHistoryAccepted(t, state, true)
				firstRow, sequence, prefix = 1, 2, "prompt_stream_error: "
			case "plan":
				prefix = "Plan in progress (0/1 complete): "
			}
			for _, wantNew := range []bool{true, false, false} {
				appendSummaryCopySource(t, state, sequence, kind, ".", "s", wantNew)
			}
			first := NewExecutionEventResponse(markerWhitespaceRows(t, journal, firstRow+1)[firstRow])
			if first.Summary != prefix+"s" {
				t.Fatal("first public summary lost its copy of the fragment")
			}
			var content struct{ Message string }
			if err := json.Unmarshal(first.Content, &content); err != nil {
				t.Fatal(err)
			}
			if kind == "diagnostic" {
				if first.ContentText != "s" || content.Message != "" {
					t.Fatal("diagnostic must publish contentText plus summary, not raw JSON message")
				}
			} else if kind == "plan" {
				plan, err := journal.EventStore.(store.PlanStore).GetPlan(context.Background(), defaultNamespace, modelHistoryTaskName)
				if err != nil || plan.PlanDocument != "# Plan\n- [ ] s _(in progress)_" || plan.PlanDocument != first.ContentText || plan.Summary != first.Summary {
					t.Fatal("plan must publish the fragment in two documents and two summaries")
				}
			} else if content.Message != "s" || first.ContentText != "" {
				t.Fatal("failure must publish raw JSON message plus summary, not contentText")
			}
			// One source copy cannot supply both s characters in password.
			event := markerWhitespaceToolEvent(sequence+1, "pa", "")
			event.Update.ToolCall.Kind = "word=fixture-value"
			appendMarkerWhitespaceUpdate(t, state, event, true)
			rows := markerWhitespaceRows(t, journal, firstRow+2)
			last := NewExecutionEventResponse(rows[firstRow+1])
			if last.Summary != events.ExecutionEventRedactedValue || last.ToolName != events.ExecutionEventRedactedValue {
				t.Fatal("two actual historical copies must prevent credential reconstruction after replay")
			}
		})
	}
}

func appendSummaryCopySource(t *testing.T, state *eventjournal.State, sequence uint64, kind, code, value string, wantNew bool) {
	t.Helper()
	event := modelHistoryEvent(sequence)
	appendEvent := state.AppendUpdateIfNew
	switch kind {
	case "diagnostic":
		event.Type = harnessv2.EventUpdate
		event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateDiagnostic, Diagnostic: &harnessv2.DiagnosticUpdate{Code: code, Message: value}}
	case "failed":
		event.Type = harnessv2.EventFailed
		event.Failed = &harnessv2.FailedEvent{Code: code, Message: value}
		appendEvent = state.AppendPromptLifecycleIfNew
	case "outcome-unknown":
		event.Type = harnessv2.EventOutcomeUnknown
		event.OutcomeUnknown = &harnessv2.OutcomeUnknownEvent{Code: code, Message: value}
		appendEvent = state.AppendPromptLifecycleIfNew
	case "stream-failure":
		appended, isNew, err := state.AppendPromptStreamFailureIfNew(context.Background(), event.Identity.Timestamp, value)
		if err != nil || isNew != wantNew || (appended != nil) != wantNew {
			t.Fatalf("append stream failure: new=%t want=%t err=%v", isNew, wantNew, err)
		}
		return
	case "plan":
		event.Type = harnessv2.EventUpdate
		event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{Content: value, Status: harnessv2.PlanEntryInProgress}}}}
		appendEvent = func(ctx context.Context, event harnessv2.Event) (*store.ExecutionEvent, bool, error) {
			projection := state.ProjectPlanUpdate(*event.Update.Plan)
			return state.AppendPlanUpdateIfNew(ctx, event, &store.PlanState{
				Namespace: defaultNamespace, TaskName: modelHistoryTaskName,
				Summary: projection.Summary, PlanDocument: projection.Document,
			})
		}
	}
	if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
		t.Fatal(err)
	}
	appended, isNew, err := appendEvent(context.Background(), event)
	if err != nil || isNew != wantNew || (appended != nil) != wantNew {
		t.Fatalf("append %s: new=%t want=%t err=%v", kind, isNew, wantNew, err)
	}
}
