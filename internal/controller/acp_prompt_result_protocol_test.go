package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// lostResponseReceiptStore commits a prompt result receipt and then reports
// its outcome unknown, as the storage client does when it cannot settle the
// write, the first lose times.
type lostResponseReceiptStore struct {
	*sqlite.Store
	lose  int
	saves int
}

func (s *lostResponseReceiptStore) SavePromptResultReceipt(ctx context.Context, receipt store.PromptResultReceipt) error {
	s.saves++
	if err := s.Store.SavePromptResultReceipt(ctx, receipt); err != nil {
		return err
	}
	if s.lose > 0 {
		s.lose--
		return fmt.Errorf("%w: prompt result receipt response lost", store.ErrOutcomeUnknown)
	}
	return nil
}

// P5: a lost receipt response leaves the attempt Running, so the dispatcher
// never moves it to Settling without knowing its receipt is stored. The step
// is idempotent by its attempt ID: settling the same result again finds the
// stored receipt and moves the attempt once, and another result is refused.
func TestSettlingAfterLostResultReceiptResponseIsIdempotent(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := configureRecoveryJournalIdentity(t, fixture)
	receipts := &lostResponseReceiptStore{Store: fixture.controlStore, lose: 1}
	fixture.dispatcher.ResultStore = receipts

	err := fixture.dispatcher.transitionAttemptToSettlingWithResult(fixture.ctx, task, fixture.attemptID, fixture.fence, []byte("the result"))
	if !errors.Is(err, store.ErrOutcomeUnknown) {
		t.Fatalf("settling with a lost receipt response = %v, want ErrOutcomeUnknown", err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionRunning {
		t.Fatalf("attempt after an unknown receipt outcome = %s, want %s", attempt.ExecutionState, store.PromptExecutionRunning)
	}
	for range 2 {
		if err := fixture.dispatcher.transitionAttemptToSettlingWithResult(fixture.ctx, task, fixture.attemptID, fixture.fence, []byte("the result")); err != nil {
			t.Fatalf("settling again: %v", err)
		}
	}
	attempt, err = fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionSettling {
		t.Fatalf("attempt after settling again = %s, want %s", attempt.ExecutionState, store.PromptExecutionSettling)
	}
	receipt, err := fixture.controlStore.GetPromptResultReceipt(fixture.ctx, fixture.attemptID)
	if err != nil || string(receipt.Data) != "the result" || receipt.OperationDigest != attempt.LastOperationDigest {
		t.Fatalf("receipt after convergence = %#v, %v", receipt, err)
	}
	if receipts.saves != 3 {
		t.Fatalf("receipt saves = %d, want one per settling call", receipts.saves)
	}
	if err := fixture.dispatcher.transitionAttemptToSettlingWithResult(fixture.ctx, task, fixture.attemptID, fixture.fence, []byte("another result")); err == nil {
		t.Fatal("settling with another result was accepted")
	}
}
