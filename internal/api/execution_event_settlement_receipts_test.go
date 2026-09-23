package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventSettlementHistoryAfterDuplicateOrReconciledAppend(t *testing.T) {
	for _, test := range []struct {
		name                string
		firstWriteFails     bool
		receiptLost         bool
		wantAppends         int
		wantReconciliations int
	}{
		{name: "first-append-duplicate", wantAppends: 1},
		{name: "first-append-ambiguous-reconciliation", receiptLost: true, wantAppends: 1, wantReconciliations: 1},
		{name: "retry-duplicate", firstWriteFails: true, wantAppends: 2, wantReconciliations: 1},
		{name: "retry-ambiguous-reconciliation", firstWriteFails: true, receiptLost: true, wantAppends: 2, wantReconciliations: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			journal, _ := newModelHistoryJournal(t, modelHistoryBenign)
			journal.MapContext.SessionName = ""
			winner, err := journal.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			appendModelHistoryAccepted(t, winner, true)
			base, ok := journal.EventStore.(store.DeduplicatingExecutionEventStore)
			if !ok {
				t.Fatal("SQLite store must support atomic append")
			}
			intercepted := &settlementReceiptStore{
				DeduplicatingExecutionEventStore: base,
				firstWriteFails:                  test.firstWriteFails,
				receiptLost:                      test.receiptLost,
			}
			competingJournal := journal
			competingJournal.EventStore = intercepted
			competing, err := competingJournal.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Acceptance is already durable before this State opens. Its first
			// store duplicate is the terminal, whose winning payload differs.
			winnerWrites := 0
			intercepted.publishWinner = func() {
				row, isNew, err := winner.AppendPromptStreamFailureIfNew(ctx, modelHistoryEvent(2).Identity.Timestamp, "pw")
				if err != nil || !isNew || row == nil {
					t.Fatalf("winning failure: new=%t error=%v", isNew, err)
				}
				winnerWrites++
			}
			intercepted.active = true
			row, isNew, err := competing.AppendPromptSettlementIfNew(ctx, modelHistorySettlement(t, "settlement-completed"), "")
			intercepted.active = false
			if err != nil || isNew || row != nil {
				t.Fatalf("losing settlement: new=%t error=%v", isNew, err)
			}
			if intercepted.appends != test.wantAppends || intercepted.reconciliations != test.wantReconciliations {
				t.Fatalf("append/reconciliation calls=%d/%d, want %d/%d", intercepted.appends, intercepted.reconciliations, test.wantAppends, test.wantReconciliations)
			}
			if winnerWrites != 1 {
				t.Fatalf("winning writes=%d, want 1", winnerWrites)
			}
			rows := modelHistoryRows(t, journal)
			if len(rows) != 2 || rows[1].Type != events.ExecutionEventTypeModelRequestFailed {
				t.Fatal("only acceptance and the winning failure must be durable")
			}
			published := NewExecutionEventResponse(rows[1])
			var content struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(published.Content, &content); err != nil {
				t.Fatal(err)
			}
			if content.Message != "pw" {
				t.Fatal("winner must publish the harmless assignment prefix")
			}
			appendSettlementHistoryTranscript(t, competing, "d=synthetic-settlement-fixture")
			rows = modelHistoryRows(t, journal)
			if len(rows) != 3 {
				t.Fatalf("row count=%d, want 3", len(rows))
			}
			transcript := NewExecutionEventResponse(rows[2])
			if transcript.ContentText != events.ExecutionEventRedactedValue || transcript.Summary != events.ExecutionEventRedactedValue {
				t.Error("losing settlement forgot winning public history before the following transcript")
			}
		})
	}
}

// Inject a competing durable write and a lost append receipt while keeping
// persistence, deduplication, and reconciliation reads backed by SQLite.
type settlementReceiptStore struct {
	store.DeduplicatingExecutionEventStore
	publishWinner   func()
	firstWriteFails bool
	receiptLost     bool
	active          bool
	appends         int
	reconciliations int
}

func (s *settlementReceiptStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	if !s.active {
		return s.DeduplicatingExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, key)
	}
	s.appends++
	if s.firstWriteFails && s.appends == 1 {
		return nil, false, errors.New("synthetic append failure before write")
	}
	if s.publishWinner != nil {
		publish := s.publishWinner
		s.publishWinner = nil
		publish()
	}
	row, isNew, err := s.DeduplicatingExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, key)
	if err != nil {
		return row, isNew, err
	}
	if s.receiptLost {
		return nil, false, errors.New("synthetic append receipt lost")
	}
	return row, isNew, nil
}

func (s *settlementReceiptStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	if s.active {
		s.reconciliations++
	}
	return s.DeduplicatingExecutionEventStore.ListExecutionEvents(ctx, filter)
}
