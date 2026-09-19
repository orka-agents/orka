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

func TestExecutionEventProviderModelPublicCopyPermutations(t *testing.T) {
	for _, kind := range []string{
		"accepted", "usage", "context", "completed", "completed-explicit", "failed", "cancelled", "outcome-unknown",
		"stream-failure", "terminal-usage", "terminal-usage-explicit",
		"settlement-completed", "settlement-cancelled", "settlement-failed", "settlement-outcome-unknown",
	} {
		for _, fixture := range []struct {
			name     string
			provider string
			model    string
			benign   bool
		}{
			{name: "split-assignment", provider: "pw", model: "d=fixture-value"},
			{name: "reverse-assignment", provider: "d=fixture-value", model: "pw"},
			{name: "trimmed-assignment", provider: " pw\t", model: " d=fixture-value\t"},
			{name: "url-removal", provider: "pw ?x=1", model: "d=fixture-value"},
			{name: "benign", provider: " openai\t", model: " gpt-test\t", benign: true},
		} {
			t.Run(kind+"/"+fixture.name, func(t *testing.T) {
				journal, state := newProviderModelHistoryJournal(t, fixture.provider, fixture.model)
				appendModelHistoryAccepted(t, state, true)
				wantRows := 1
				if kind == "accepted" {
					appendModelHistoryAccepted(t, state, false)
				} else {
					step := strings.TrimSuffix(kind, "-explicit")
					var explicitModel string
					if step != kind {
						explicitModel = fixture.model
					}
					appendModelHistoryStep(t, state, step, explicitModel, true)
					appendModelHistoryStep(t, state, step, explicitModel, false)
					wantRows++
				}
				rows := modelHistoryRows(t, journal)
				if len(rows) != wantRows {
					t.Fatalf("persisted rows = %d, want %d", len(rows), wantRows)
				}
				// Inspect the target record independently so acceptance redaction
				// cannot hide an unsafe later lifecycle or usage projection.
				providers, models := providerModelHistoryPublicCopies(t, rows[len(rows)-1])
				for _, field := range []struct {
					copies [2]string
					input  string
				}{{providers, fixture.provider}, {models, fixture.model}} {
					normalized := strings.TrimSpace(events.RedactExecutionEventText(strings.TrimSpace(field.input)))
					for _, value := range field.copies {
						if value != normalized && value != events.ExecutionEventRedactedValue {
							t.Error("public metadata differs from its final sanitized value or redaction marker")
						}
						if fixture.benign && value != normalized {
							t.Error("benign provider or model was suppressed")
						}
					}
				}
				for _, provider := range providers {
					for _, model := range models {
						for _, joined := range []string{provider + model, model + provider} {
							if events.RedactExecutionEventText(joined) != joined {
								t.Error("distinct provider and model DTO locations reconstruct a protected value")
							}
						}
					}
				}
			})
		}
	}
}

func TestExecutionEventProviderHistoryWithoutModel(t *testing.T) {
	fragment := modelHistoryFragment(t)
	for _, kind := range []string{
		"usage", "context", "completed", "failed", "cancelled", "outcome-unknown", "stream-failure", "terminal-usage",
		"settlement-completed", "settlement-cancelled", "settlement-failed", "settlement-outcome-unknown",
	} {
		t.Run(kind, func(t *testing.T) {
			journal, state := newProviderModelHistoryJournal(t, fragment, "")
			appendModelHistoryAccepted(t, state, true)
			appendModelHistoryStep(t, state, kind, "", true)
			appendModelHistoryStep(t, state, kind, "", false)
			rows := modelHistoryRows(t, journal)
			if len(rows) != 2 {
				t.Fatalf("persisted rows = %d, want 2", len(rows))
			}
			var rawCopies int
			for _, row := range rows {
				providers, models := providerModelHistoryPublicCopies(t, row)
				if models != [2]string{} {
					t.Error("missing model acquired a public value")
				}
				for _, provider := range providers {
					switch provider {
					case fragment:
						rawCopies++
					case events.ExecutionEventRedactedValue:
					default:
						t.Error("public provider differs from its value or redaction marker")
					}
				}
			}
			joined := strings.Repeat(fragment, rawCopies)
			if events.RedactExecutionEventText(joined) != joined {
				t.Error("provider copies across distinct events reconstruct a protected value")
			}
		})
	}
}

func TestExecutionEventProviderHistoryProtectsLaterTool(t *testing.T) {
	journal, state := newProviderModelHistoryJournal(t, "pw", "")
	appendModelHistoryAccepted(t, state, true)
	const value = "provider-history-fixture-value"
	event := modelHistoryEvent(2)
	event.Type = harnessv2.EventUpdate
	event.Update = &harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate,
		ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "provider-history-tool", Kind: "shell", Title: "d=", Status: harnessv2.ToolCallStatusCompleted,
			Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: value}},
		},
	}
	if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
		t.Fatal(err)
	}
	if _, isNew, err := state.AppendUpdateIfNew(context.Background(), event); err != nil || !isNew {
		t.Fatalf("append tool: new=%t err=%v", isNew, err)
	}
	rows := modelHistoryRows(t, journal)
	if len(rows) != 2 {
		t.Fatalf("persisted rows = %d, want 2", len(rows))
	}
	encoded, err := json.Marshal(NewExecutionEventResponse(rows[1]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), value) {
		t.Error("later tool output completes an assignment from the historical provider")
	}
}

func newProviderModelHistoryJournal(t *testing.T, provider, model string) (eventjournal.Journal, *eventjournal.State) {
	t.Helper()
	journal, _ := newModelHistoryJournal(t, model)
	journal.MapContext.Provider = provider
	// Configure the provider before the empty journal receives its first event.
	state, err := journal.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return journal, state
}

func providerModelHistoryPublicCopies(t *testing.T, row store.ExecutionEvent) ([2]string, [2]string) {
	t.Helper()
	encoded, err := json.Marshal(NewExecutionEventResponse(row))
	if err != nil {
		t.Fatal(err)
	}
	var dto struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Content  struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &dto); err != nil {
		t.Fatal(err)
	}
	return [2]string{dto.Provider, dto.Content.Provider}, [2]string{dto.Model, dto.Content.Model}
}
