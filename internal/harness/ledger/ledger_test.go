/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func TestLedgerAdmissionIdempotencyAndDigestMismatch(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)

	outcome, record, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa")
	if err != nil || outcome != AdmitOutcomeAdmitted || record.State != TurnAdmitted {
		t.Fatalf("admit = %v %+v %v", outcome, record, err)
	}

	// The same turn ID and digest replays idempotently.
	outcome, record, err = l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa")
	if err != nil || outcome != AdmitOutcomeDuplicate || record.State != TurnAdmitted {
		t.Fatalf("duplicate admit = %v %+v %v", outcome, record, err)
	}

	// The same turn ID with a different digest is permanently rejected.
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:bbbb"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch, got %v", err)
	}
}

func TestLedgerRejectionIsDurableNonAcceptanceProof(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := l.MarkTurnRejected(ctx, "turn-1", "endpoint refused"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	record, err := l.GetTurn(ctx, "turn-1")
	if err != nil || record.State != TurnRejected || !record.Settled() {
		t.Fatalf("rejected record = %+v, %v", record, err)
	}
	// A rejected turn can no longer be accepted.
	if err := l.MarkTurnAccepted(ctx, "turn-1"); err == nil {
		t.Fatal("accepting a rejected turn must fail")
	}
}

func TestLedgerTerminalReceiptsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	l, path := openTestLedger(t)
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := l.MarkTurnAccepted(ctx, "turn-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	receipt := []byte(`{"outcome":"succeeded"}`)
	if err := l.RecordTurnTerminal(ctx, "turn-1", receipt, false); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	// Identical replay is idempotent; a different receipt conflicts.
	if err := l.RecordTurnTerminal(ctx, "turn-1", receipt, false); err != nil {
		t.Fatalf("terminal replay: %v", err)
	}
	if err := l.RecordTurnTerminal(ctx, "turn-1", []byte(`{"outcome":"failed"}`), false); err == nil {
		t.Fatal("conflicting terminal receipt must fail")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	record, err := reopened.GetTurn(ctx, "turn-1")
	if err != nil || record.State != TurnTerminal || record.TerminalReceiptDigest != ReceiptDigest(receipt) {
		t.Fatalf("terminal record after restart = %+v, %v", record, err)
	}
	if string(record.TerminalReceipt) != string(receipt) {
		t.Fatalf("terminal receipt payload mismatch: %s", record.TerminalReceipt)
	}
}

func TestLedgerRejectsCorruptTerminalReceiptAtReadBoundary(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := l.MarkTurnAccepted(ctx, "turn-1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := l.RecordTurnTerminal(ctx, "turn-1", []byte(`{"outcome":"succeeded"}`), false); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	if _, err := l.db.ExecContext(ctx, `UPDATE turn_admissions SET terminal_receipt = ? WHERE turn_id = ?`,
		[]byte(`{"outcome":"corrupt"}`), "turn-1"); err != nil {
		t.Fatalf("corrupt receipt fixture: %v", err)
	}

	if _, err := l.GetTurn(ctx, "turn-1"); err == nil || !strings.Contains(err.Error(), "receipt digest mismatch") {
		t.Fatalf("GetTurn() error = %v, want receipt digest mismatch", err)
	}
}

func TestLedgerOutcomeUnknownAndUnsettledInventory(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	for _, turn := range []string{"turn-1", "turn-2", "turn-3"} {
		if _, _, err := l.AdmitTurn(ctx, turn, "task-uid", 1, "sha256:aaaa"); err != nil {
			t.Fatalf("admit %s: %v", turn, err)
		}
	}
	if err := l.MarkTurnAccepted(ctx, "turn-2"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := l.RecordTurnTerminal(ctx, "turn-3", []byte(`{"reason":"abandoned"}`), true); err != nil {
		t.Fatalf("outcome unknown: %v", err)
	}
	record, err := l.GetTurn(ctx, "turn-3")
	if err != nil || record.State != TurnOutcomeUnknown || !record.Settled() {
		t.Fatalf("outcome-unknown record = %+v, %v", record, err)
	}

	unsettled, err := l.ListUnsettledTurns(ctx)
	if err != nil || len(unsettled) != 2 {
		t.Fatalf("unsettled = %+v, %v", unsettled, err)
	}
}

func TestLedgerAdmissionCloseIsDurableAndFailClosed(t *testing.T) {
	ctx := context.Background()
	l, path := openTestLedger(t)
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := l.CloseAdmission(ctx); err != nil {
		t.Fatalf("close admission: %v", err)
	}
	if err := l.CloseAdmission(ctx); err != nil {
		t.Fatalf("idempotent close admission: %v", err)
	}
	closed, closedAt, err := l.AdmissionClosed(ctx)
	if err != nil || !closed || closedAt.IsZero() {
		t.Fatalf("admission closed = %v %v %v", closed, closedAt, err)
	}

	// New admissions fail closed; existing turns still settle.
	if _, _, err := l.AdmitTurn(ctx, "turn-2", "task-uid", 1, "sha256:cccc"); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("expected ErrAdmissionClosed, got %v", err)
	}
	// A pre-close duplicate admission still replays idempotently.
	if outcome, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa"); err != nil || outcome != AdmitOutcomeDuplicate {
		t.Fatalf("duplicate after close = %v, %v", outcome, err)
	}
	if err := l.MarkTurnAccepted(ctx, "turn-1"); err != nil {
		t.Fatalf("accept after close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	closed, _, err = reopened.AdmissionClosed(ctx)
	if err != nil || !closed {
		t.Fatalf("admission close must survive restart, got %v %v", closed, err)
	}
	generation, err := reopened.Generation(ctx)
	if err != nil || generation == "" {
		t.Fatalf("generation = %q, %v", generation, err)
	}
}
