package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	modelHistoryTaskName = "model-history-task"
	modelHistoryBenign   = "gpt-test"
)

func TestExecutionEventFallbackModelHistory(t *testing.T) {
	fragment := modelHistoryFragment(t)
	for _, kind := range []string{
		"usage", "context", "completed", "failed", "cancelled", "outcome-unknown", "stream-failure", "terminal-usage",
		"settlement-completed", "settlement-cancelled", "settlement-failed", "settlement-outcome-unknown",
	} {
		for _, model := range []struct {
			name  string
			value string
		}{
			{name: "split-token", value: fragment},
			{name: "benign", value: modelHistoryBenign},
		} {
			t.Run(kind+"/"+model.name, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, model.value)
				appendModelHistoryAccepted(t, state, true)
				appendModelHistoryStep(t, state, kind, "", true)
				appendModelHistoryStep(t, state, kind, "", false)
				rows := modelHistoryRows(t, journal)
				if len(rows) != 2 {
					t.Fatalf("persisted rows = %d, want accepted and one follow-up", len(rows))
				}
				assertModelHistoryPublicCopies(t, rows, model.value)
			})
		}
	}
}

func TestExecutionEventFallbackModelHistoryAfterReopen(t *testing.T) {
	for _, kind := range []string{"usage", "completed", "stream-failure", "settlement-cancelled"} {
		t.Run(kind, func(t *testing.T) {
			fragment := modelHistoryFragment(t)
			journal, state := newModelHistoryJournal(t, fragment)
			appendModelHistoryAccepted(t, state, true)
			reopened, err := journal.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			appendModelHistoryAccepted(t, reopened, false)
			appendModelHistoryStep(t, reopened, kind, "", true)
			appendModelHistoryStep(t, reopened, kind, "", false)
			rows := modelHistoryRows(t, journal)
			if len(rows) != 2 {
				t.Fatalf("persisted rows after reopen = %d, want 2", len(rows))
			}
			assertModelHistoryPublicCopies(t, rows, fragment)
		})
	}
}

func TestExecutionEventModelHistoryAcceptedReplay(t *testing.T) {
	journal, state := newModelHistoryJournal(t, modelHistoryBenign)
	appendModelHistoryAccepted(t, state, true)
	for range 3 {
		appendModelHistoryAccepted(t, state, false)
	}
	reopened, err := journal.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	appendModelHistoryAccepted(t, reopened, false)
	rows := modelHistoryRows(t, journal)
	if len(rows) != 1 {
		t.Fatalf("replayed accepted event produced %d rows, want 1", len(rows))
	}
	assertModelHistoryPublicCopies(t, rows, modelHistoryBenign)
}

func TestExecutionEventModelFinalNormalization(t *testing.T) {
	for _, model := range []struct {
		name   string
		value  string
		benign bool
	}{
		{name: "split-key", value: "ken=X\tto ?x=1"},
		{name: "benign", value: modelHistoryBenign + " ?x=1", benign: true},
	} {
		normalized := strings.TrimSpace(events.RedactExecutionEventText(model.value))
		if !model.benign && events.RedactExecutionEventText(normalized+normalized) == normalized+normalized {
			t.Fatal("normalization fixture must become sensitive when its two final model copies are joined")
		}
		for _, kind := range []string{
			"accepted", "usage", "context", "completed", "completed-explicit", "failed", "cancelled", "outcome-unknown",
			"stream-failure", "terminal-usage", "terminal-usage-explicit", "settlement-cancelled",
		} {
			t.Run(kind+"/"+model.name, func(t *testing.T) {
				journal, state := newModelHistoryJournal(t, model.value)
				appendModelHistoryAccepted(t, state, true)
				wantRows := 1
				if kind == "accepted" {
					appendModelHistoryAccepted(t, state, false)
				} else {
					step := strings.TrimSuffix(kind, "-explicit")
					var explicitModel string
					if step != kind {
						explicitModel = model.value
					}
					appendModelHistoryStep(t, state, step, explicitModel, true)
					appendModelHistoryStep(t, state, step, explicitModel, false)
					wantRows++
				}
				rows := modelHistoryRows(t, journal)
				if len(rows) != wantRows {
					t.Fatalf("persisted rows = %d, want %d", len(rows), wantRows)
				}
				// Inspect the target record alone so an acceptance failure cannot
				// hide whether a later projection also normalizes after matching.
				copies := modelHistoryPublicCopies(t, rows[len(rows)-1])
				for _, value := range copies {
					if value == events.ExecutionEventRedactedValue {
						if model.benign {
							t.Error("benign normalized model was suppressed")
						}
					} else if strings.TrimSpace(value) != normalized {
						t.Error("public model is neither the sanitized model nor its redaction marker")
					}
				}
				// Use the exact DTO strings, including any remaining whitespace.
				// Either field can precede the other when public values are joined.
				for _, joined := range []string{copies[0] + copies[1], copies[1] + copies[0]} {
					if events.RedactExecutionEventText(joined) != joined {
						t.Error("final normalized model DTO copies reconstruct a protected value")
					}
				}
			})
		}
	}
}

func modelHistoryFragment(t *testing.T) string {
	t.Helper()
	fragment := "sk-" + strings.Repeat("a", 7)
	twice, thrice := strings.Repeat(fragment, 2), strings.Repeat(fragment, 3)
	if events.RedactExecutionEventText(twice) != twice || events.RedactExecutionEventText(thrice) == thrice {
		t.Fatal("synthetic model fixture must become sensitive on its third copy")
	}
	return fragment
}

func newModelHistoryJournal(t *testing.T, model string) (eventjournal.Journal, *eventjournal.State) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "model-history.db")
	db, err := sqlite.NewDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	journal := eventjournal.Journal{
		EventStore: sqlite.NewStore(db, dbPath),
		MapContext: eventjournal.MapContext{
			Namespace: defaultNamespace, TaskName: modelHistoryTaskName, SessionName: "model-history-session", Model: model,
		},
	}
	state, err := journal.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return journal, state
}

func modelHistoryEvent(sequence uint64) harnessv2.Event {
	return harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion,
		Identity: harnessv2.EventIdentity{
			RuntimeInstanceID: "model-runtime", SupervisorBootID: "model-boot", RuntimeSessionUID: "model-session",
			RuntimeSessionGeneration: 1, TaskUID: "model-task", TaskAttempt: 1, PromptID: "model-prompt",
			Sequence: sequence, RequestDigest: harnessv2.RequestDigest("sha256:" + strings.Repeat("a", 64)),
			Timestamp: time.Date(2026, 9, 18, 12, 0, 0, int(sequence)*int(time.Millisecond), time.UTC),
		},
	}
}

func appendModelHistoryAccepted(t *testing.T, state *eventjournal.State, wantNew bool) {
	t.Helper()
	event := modelHistoryEvent(1)
	event.Type = harnessv2.EventAccepted
	event.Accepted = &harnessv2.AcceptedEvent{
		AcceptedAt: event.Identity.Timestamp, ACPVersion: harnessv2.ACPProfileV1,
		Lease: harnessv2.PromptLease{
			Generation: 1, IssuedAt: event.Identity.Timestamp, ExpiresAt: event.Identity.Timestamp.Add(time.Minute),
		},
	}
	if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
		t.Fatal(err)
	}
	appended, isNew, err := state.AppendPromptLifecycleIfNew(context.Background(), event)
	if err != nil || isNew != wantNew || (appended != nil) != wantNew {
		t.Fatalf("append accepted: new=%t want=%t err=%v", isNew, wantNew, err)
	}
}

func appendModelHistoryStep(t *testing.T, state *eventjournal.State, kind, explicitModel string, wantNew bool) {
	t.Helper()
	ctx := context.Background()
	event := modelHistoryEvent(2)
	var appended *store.ExecutionEvent
	var isNew bool
	var err error
	if kind == "stream-failure" {
		appended, isNew, err = state.AppendPromptStreamFailureIfNew(ctx, event.Identity.Timestamp, ".")
	} else if strings.HasPrefix(kind, "settlement-") {
		settlement := modelHistorySettlement(t, kind)
		appended, isNew, err = state.AppendPromptSettlementIfNew(ctx, settlement, harnessv2.CancelReasonTaskTimeout)
	} else {
		switch kind {
		case "usage", "context":
			usage := harnessv2.UsageUpdate{InputTokens: 1}
			if kind == "context" {
				used, size := uint64(1), uint64(10)
				usage = harnessv2.UsageUpdate{ContextWindowUsed: &used, ContextWindowSize: &size}
			}
			event.Type = harnessv2.EventUpdate
			event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &usage}
		case "completed", "terminal-usage":
			event.Type = harnessv2.EventCompleted
			event.Completed = &harnessv2.CompletedEvent{
				StopReason: harnessv2.ACPStopReasonEndTurn,
				Result: harnessv2.PromptResult{
					Model:   explicitModel,
					Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "."}},
					Usage:   harnessv2.UsageUpdate{InputTokens: 1},
				},
			}
		case "failed":
			event.Type = harnessv2.EventFailed
			event.Failed = &harnessv2.FailedEvent{Code: ".", Message: "."}
		case "cancelled":
			event.Type = harnessv2.EventCancelled
			event.Cancelled = &harnessv2.CancelledEvent{StopReason: harnessv2.ACPStopReasonCancelled, Reason: "."}
		case "outcome-unknown":
			event.Type = harnessv2.EventOutcomeUnknown
			event.OutcomeUnknown = &harnessv2.OutcomeUnknownEvent{Code: ".", Message: "."}
		default:
			t.Fatalf("unknown model history step %q", kind)
		}
		if validationErr := event.Validate(harnessv2.DefaultEventStreamLimits()); validationErr != nil {
			t.Fatal(validationErr)
		}
		switch {
		case kind == "terminal-usage":
			appended, isNew, err = state.AppendTerminalUsageIfNew(ctx, event)
		case event.Type == harnessv2.EventUpdate:
			appended, isNew, err = state.AppendUpdateIfNew(ctx, event)
		default:
			appended, isNew, err = state.AppendPromptLifecycleIfNew(ctx, event)
		}
	}
	if err != nil || isNew != wantNew || (appended != nil) != wantNew {
		t.Fatalf("append %s: new=%t want=%t err=%v", kind, isNew, wantNew, err)
	}
}

func modelHistorySettlement(t *testing.T, kind string) harnessv2.PromptSettlement {
	t.Helper()
	settlement := harnessv2.PromptSettlement{SettledAt: modelHistoryEvent(2).Identity.Timestamp}
	switch kind {
	case "settlement-completed":
		settlement.TerminalEvent, settlement.Outcome = harnessv2.EventCompleted, harnessv2.PromptOutcomeSucceeded
		settlement.StopReason = harnessv2.ACPStopReasonEndTurn
	case "settlement-cancelled":
		settlement.TerminalEvent, settlement.Outcome = harnessv2.EventCancelled, harnessv2.PromptOutcomeCancelled
		settlement.StopReason = harnessv2.ACPStopReasonCancelled
	case "settlement-failed":
		settlement.TerminalEvent, settlement.Outcome = harnessv2.EventFailed, harnessv2.PromptOutcomeFailed
		settlement.StopReason = harnessv2.ACPStopReasonRefusal
	case "settlement-outcome-unknown":
		settlement.TerminalEvent, settlement.Outcome = harnessv2.EventOutcomeUnknown, harnessv2.PromptOutcomeUnknown
	default:
		t.Fatalf("unknown settlement history step %q", kind)
	}
	if err := settlement.Validate(); err != nil {
		t.Fatal(err)
	}
	return settlement
}

func modelHistoryRows(t *testing.T, journal eventjournal.Journal) []store.ExecutionEvent {
	t.Helper()
	rows, err := journal.EventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: defaultNamespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: modelHistoryTaskName, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertModelHistoryPublicCopies(t *testing.T, rows []store.ExecutionEvent, model string) {
	t.Helper()
	var rawCopies int
	for _, row := range rows {
		// Each persisted row is counted once. The root model and content.model
		// are two real DTO locations; replaying the same event adds no rows.
		for _, value := range modelHistoryPublicCopies(t, row) {
			switch value {
			case model:
				rawCopies++
			case events.ExecutionEventRedactedValue:
				if model == modelHistoryBenign {
					t.Errorf("benign model was suppressed in event %d", row.Seq)
				}
			default:
				t.Errorf("event %d did not expose the model or its redaction marker", row.Seq)
			}
		}
	}
	joined := strings.Repeat(model, rawCopies)
	if events.RedactExecutionEventText(joined) != joined {
		t.Errorf("%d raw model DTO copies reconstruct a protected value", rawCopies)
	}
}

func modelHistoryPublicCopies(t *testing.T, row store.ExecutionEvent) [2]string {
	t.Helper()
	encoded, err := json.Marshal(NewExecutionEventResponse(row))
	if err != nil {
		t.Fatal(err)
	}
	var dto struct {
		Model   string `json:"model"`
		Content struct {
			Model string `json:"model"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &dto); err != nil {
		t.Fatal(err)
	}
	return [2]string{dto.Model, dto.Content.Model}
}
