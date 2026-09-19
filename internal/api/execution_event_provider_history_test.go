package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventProviderModelPublicCopies(t *testing.T) {
	for _, test := range []struct {
		name, provider, model string
		benign                bool
	}{
		{name: "split assignment", provider: "pw", model: "d=fixture-value"},
		{name: "normalized provider", provider: "pw ?x=1", model: "d=fixture-value"},
		{name: "provider without model", provider: "ken=fixture-valueto"},
		{name: "benign pair", provider: "pw", model: modelHistoryBenign, benign: true},
		{name: "benign normalized provider", provider: "pw ?x=1", model: modelHistoryBenign, benign: true},
	} {
		for _, kind := range []string{
			"accepted", "usage", "context", "completed", "completed-explicit", "failed", "cancelled", "outcome-unknown",
			"stream-failure", "terminal-usage", "terminal-usage-explicit", "settlement-completed", "settlement-cancelled",
			"settlement-failed", "settlement-outcome-unknown",
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				configuredModel, explicitModel := test.model, ""
				if strings.HasSuffix(kind, "-explicit") && test.model != "" {
					configuredModel, explicitModel = modelHistoryBenign, test.model
				}
				journal, state := newProviderHistoryJournal(t, test.provider, configuredModel)
				appendModelHistoryAccepted(t, state, true)
				wantRows := 1
				if kind != "accepted" {
					step := strings.TrimSuffix(kind, "-explicit")
					appendModelHistoryStep(t, state, step, explicitModel, true)
					appendModelHistoryStep(t, state, step, explicitModel, false)
					wantRows++
				}
				rows := modelHistoryRows(t, journal)
				if len(rows) != wantRows {
					t.Fatalf("persisted rows = %d, want %d", len(rows), wantRows)
				}
				copies := providerModelPublicCopies(t, rows[len(rows)-1])
				for index, value := range copies {
					want := test.provider
					if index%2 == 1 {
						want = test.model
					}
					want = strings.TrimSpace(events.RedactExecutionEventText(want))
					if !test.benign && want != "" {
						want = events.ExecutionEventRedactedValue
					}
					if value != want {
						t.Errorf("public telemetry copy %d = %q, want %q", index, value, want)
					}
				}
			})
		}
	}
}

func TestExecutionEventProviderHistoryAfterReopen(t *testing.T) {
	fragment := modelHistoryFragment(t)
	journal, state := newProviderHistoryJournal(t, fragment, "")
	appendModelHistoryAccepted(t, state, true)
	reopened, err := journal.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	appendModelHistoryAccepted(t, reopened, false)
	appendModelHistoryStep(t, reopened, "usage", "", true)
	appendModelHistoryStep(t, reopened, "usage", "", false)
	rows := modelHistoryRows(t, journal)
	if len(rows) != 2 {
		t.Fatalf("persisted rows after reopen = %d, want 2", len(rows))
	}
	copies := providerModelPublicCopies(t, rows[1])
	if copies[0] != events.ExecutionEventRedactedValue || copies[2] != events.ExecutionEventRedactedValue {
		t.Fatal("provider survived fail-closed journal history after reopen")
	}
}

func TestExecutionEventProviderHistoryAcrossRecords(t *testing.T) {
	fragment := modelHistoryFragment(t)
	for _, kind := range []string{"usage", "completed", "stream-failure", "terminal-usage", "settlement-cancelled"} {
		t.Run(kind, func(t *testing.T) {
			journal, state := newProviderHistoryJournal(t, fragment, "")
			appendModelHistoryAccepted(t, state, true)
			appendModelHistoryStep(t, state, kind, "", true)
			appendModelHistoryStep(t, state, kind, "", false)
			rows := modelHistoryRows(t, journal)
			if len(rows) != 2 {
				t.Fatalf("persisted rows = %d, want 2", len(rows))
			}
			first, second := providerModelPublicCopies(t, rows[0]), providerModelPublicCopies(t, rows[1])
			if first[0] != fragment || first[2] != fragment {
				t.Fatal("initial two harmless provider copies were suppressed")
			}
			if second[0] != events.ExecutionEventRedactedValue || second[2] != events.ExecutionEventRedactedValue {
				t.Fatal("later provider copies reconstructed a protected value across API records")
			}
		})
	}
}

func newProviderHistoryJournal(t *testing.T, provider, model string) (eventjournal.Journal, *eventjournal.State) {
	t.Helper()
	journal, _ := newModelHistoryJournal(t, model)
	journal.MapContext.Provider = provider
	state, err := journal.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return journal, state
}

func providerModelPublicCopies(t *testing.T, row store.ExecutionEvent) [4]string {
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
	return [4]string{dto.Provider, dto.Model, dto.Content.Provider, dto.Content.Model}
}
