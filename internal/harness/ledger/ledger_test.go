/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ledger

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testRuntimeSessionID = "runtime-session-1"
	testCorrelationID    = "correlation-1"
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

	outcome, record, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID)
	if err != nil || outcome != AdmitOutcomeAdmitted || record.State != TurnAdmitted {
		t.Fatalf("admit = %v %+v %v", outcome, record, err)
	}

	// The same turn ID and digest replays idempotently.
	outcome, record, err = l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID)
	if err != nil || outcome != AdmitOutcomeDuplicate || record.State != TurnAdmitted {
		t.Fatalf("duplicate admit = %v %+v %v", outcome, record, err)
	}

	// The same turn ID with a different digest is permanently rejected.
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:bbbb", testRuntimeSessionID, testCorrelationID); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch, got %v", err)
	}
}

func TestLedgerRejectionIsDurableNonAcceptanceProof(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID); err != nil {
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
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID); err != nil {
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
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID); err != nil {
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
		if _, _, err := l.AdmitTurn(ctx, turn, "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID); err != nil {
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
	if _, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID); err != nil {
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
	if _, _, err := l.AdmitTurn(ctx, "turn-2", "task-uid", 1, "sha256:cccc", testRuntimeSessionID, "correlation-2"); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("expected ErrAdmissionClosed, got %v", err)
	}
	// A pre-close duplicate admission still replays idempotently.
	if outcome, _, err := l.AdmitTurn(ctx, "turn-1", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID); err != nil || outcome != AdmitOutcomeDuplicate {
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

func TestLedgerTerminalOutputSurvivesRestartAndFailsClosedOnCorruption(t *testing.T) {
	ctx := context.Background()
	l, path := openTestLedger(t)
	if _, _, err := l.AdmitTurn(
		ctx, "turn-output", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID,
	); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := l.MarkTurnAccepted(ctx, "turn-output"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	receipt := []byte(`{"outcome":"succeeded","outputRef":"cliwrapper-result-v1"}`)
	want := []byte("durable result bytes")
	if err := l.RecordTurnTerminalWithOutput(ctx, "turn-output", receipt, false, &TurnOutput{
		Ref: "cliwrapper-result-v1", Data: want,
	}); err != nil {
		t.Fatalf("terminal with output: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.GetTurnOutput(ctx, "turn-output", "cliwrapper-result-v1")
	if err != nil || string(got) != string(want) {
		t.Fatalf("GetTurnOutput() = %q, %v, want %q", got, err, want)
	}
	if _, err := reopened.GetTurnOutput(ctx, "turn-output", "wrong-ref"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-ref GetTurnOutput() error = %v, want ErrNotFound", err)
	}
	if _, err := reopened.db.ExecContext(ctx, `UPDATE turn_outputs SET output_payload = ? WHERE turn_id = ?`,
		[]byte("corrupt"), "turn-output"); err != nil {
		t.Fatalf("corrupt output fixture: %v", err)
	}
	if _, err := reopened.GetTurnOutput(ctx, "turn-output", "cliwrapper-result-v1"); err == nil ||
		!strings.Contains(err.Error(), "corrupt terminal turn output") {
		t.Fatalf("corrupt GetTurnOutput() error = %v", err)
	}
}

//nolint:gocyclo // One test keeps the full close, drain, prepare, restart, and activation protocol in order.
func TestLedgerRolloverRequiresCompletedDrainAndExactGeneration(t *testing.T) {
	ctx := context.Background()
	l, path := openTestLedger(t)
	if _, err := l.PrepareRollover(ctx, "2"); err == nil || !strings.Contains(err.Error(), "must be closed") {
		t.Fatalf("PrepareRollover(open) error = %v, want closed-admission rejection", err)
	}
	if _, _, err := l.AdmitTurn(
		ctx, "turn-rollover", "task-uid", 1, "sha256:aaaa", testRuntimeSessionID, testCorrelationID,
	); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := l.MarkTurnAccepted(ctx, "turn-rollover"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := l.CloseAdmission(ctx); err != nil {
		t.Fatalf("close admission: %v", err)
	}
	if _, err := l.PrepareRollover(ctx, "2"); err == nil || !strings.Contains(err.Error(), "drain is incomplete") {
		t.Fatalf("PrepareRollover(unsettled) error = %v, want incomplete-drain rejection", err)
	}
	receipt := []byte(`{"outcome":"succeeded"}`)
	if err := l.RecordTurnTerminal(ctx, "turn-rollover", receipt, false); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	current, err := l.PrepareRollover(ctx, "2")
	if err != nil || current != "1" {
		t.Fatalf("PrepareRollover() = %q, %v, want current generation 1", current, err)
	}
	if current, err := l.PrepareRollover(ctx, "2"); err != nil || current != "1" {
		t.Fatalf("PrepareRollover(idempotent) = %q, %v", current, err)
	}
	if current, err := l.PrepareRollover(ctx, "3"); err != nil || current != "1" {
		t.Fatalf("PrepareRollover(superseding) = %q, %v, want current generation 1", current, err)
	}
	if err := l.ActivateGeneration(ctx, "2"); err == nil || !strings.Contains(err.Error(), "was not prepared") {
		t.Fatalf("ActivateGeneration(superseded) error = %v", err)
	}
	if err := l.ActivateGeneration(ctx, "1"); err != nil {
		t.Fatalf("ActivateGeneration(same generation): %v", err)
	}
	if closed, _, err := l.AdmissionClosed(ctx); err != nil || !closed {
		t.Fatalf("same-generation activation reopened admission: closed=%v err=%v", closed, err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.ActivateGeneration(ctx, "3"); err != nil {
		t.Fatalf("ActivateGeneration(prepared): %v", err)
	}
	if generation, err := reopened.Generation(ctx); err != nil || generation != "3" {
		t.Fatalf("Generation() = %q, %v, want 3", generation, err)
	}
	if closed, _, err := reopened.AdmissionClosed(ctx); err != nil || closed {
		t.Fatalf("activated generation admission closed=%v, err=%v, want open", closed, err)
	}
	if err := reopened.ActivateGeneration(ctx, "3"); err != nil {
		t.Fatalf("ActivateGeneration(idempotent): %v", err)
	}
	record, err := reopened.GetTurn(ctx, "turn-rollover")
	if err != nil || record.State != TurnTerminal || string(record.TerminalReceipt) != string(receipt) {
		t.Fatalf("terminal tombstone after rollover = %#v, %v", record, err)
	}
}

func TestLedgerAbortRolloverReopensOnlyExactCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	if err := l.CloseAdmission(ctx); err != nil {
		t.Fatalf("CloseAdmission: %v", err)
	}
	if _, err := l.PrepareRollover(ctx, "2"); err != nil {
		t.Fatalf("PrepareRollover: %v", err)
	}
	if err := l.AbortRollover(ctx, "2"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("AbortRollover(stale generation) error = %v, want exact-generation rejection", err)
	}
	if closed, _, err := l.AdmissionClosed(ctx); err != nil || !closed {
		t.Fatalf("stale abort changed admission: closed=%v err=%v", closed, err)
	}
	if err := l.AbortRollover(ctx, "1"); err != nil {
		t.Fatalf("AbortRollover: %v", err)
	}
	if closed, _, err := l.AdmissionClosed(ctx); err != nil || closed {
		t.Fatalf("aborted rollover admission closed=%v err=%v, want open", closed, err)
	}
	if err := l.ActivateGeneration(ctx, "2"); err == nil || !strings.Contains(err.Error(), "was not prepared") {
		t.Fatalf("ActivateGeneration(aborted) error = %v, want missing preparation", err)
	}
	if err := l.AbortRollover(ctx, "1"); err != nil {
		t.Fatalf("AbortRollover(idempotent): %v", err)
	}

	if err := l.CloseAdmission(ctx); err != nil {
		t.Fatalf("CloseAdmission(without prepare): %v", err)
	}
	if err := l.AbortRollover(ctx, "1"); err == nil || !strings.Contains(err.Error(), "without a prepared rollover") {
		t.Fatalf("AbortRollover(bare close) error = %v, want fail-closed rejection", err)
	}
	if closed, _, err := l.AdmissionClosed(ctx); err != nil || !closed {
		t.Fatalf("bare-close abort changed admission: closed=%v err=%v", closed, err)
	}
}

func TestLedgerFreshGenerationInitializationDoesNotOverrideExistingLedger(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "generation.db")
	l, err := OpenWithGeneration(path, "17")
	if err != nil {
		t.Fatalf("OpenWithGeneration(fresh): %v", err)
	}
	if generation, err := l.Generation(ctx); err != nil || generation != "17" {
		t.Fatalf("fresh Generation() = %q, %v, want 17", generation, err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenWithGeneration(path, "18")
	if err != nil {
		t.Fatalf("OpenWithGeneration(existing): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if generation, err := reopened.Generation(ctx); err != nil || generation != "17" {
		t.Fatalf("existing Generation() = %q, %v, want preserved 17", generation, err)
	}
	if err := reopened.ActivateGeneration(ctx, "18"); err == nil || !strings.Contains(err.Error(), "was not prepared") {
		t.Fatalf("ActivateGeneration(unprepared existing ledger) error = %v", err)
	}
}

func TestLedgerMigratesPreviewSchemaWithoutLosingExistingRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "preview.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open preview fixture: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE ledger_control (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE turn_admissions (
			turn_id TEXT PRIMARY KEY,
			task_uid TEXT NOT NULL,
			attempt INTEGER NOT NULL CHECK(attempt > 0),
			request_digest TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('Admitted','Accepted','Rejected','Terminal','OutcomeUnknown')),
			reject_reason TEXT NOT NULL DEFAULT '',
			terminal_receipt BLOB,
			terminal_receipt_digest TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
		`INSERT INTO ledger_control (key, value) VALUES ('generation', 'preview-7')`,
		`INSERT INTO turn_admissions (
			turn_id, task_uid, attempt, request_digest, state, reject_reason,
			terminal_receipt, terminal_receipt_digest, created_at, updated_at
		) VALUES (
			'turn-preview', 'task-preview', 1, 'sha256:preview', 'Rejected',
			'preview rejection', NULL, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatalf("create preview fixture: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close preview fixture: %v", err)
	}

	l, err := OpenWithGeneration(path, "replacement-8")
	if err != nil {
		t.Fatalf("migrate preview ledger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if generation, err := l.Generation(ctx); err != nil || generation != "preview-7" {
		t.Fatalf("migrated generation = %q, %v, want preview-7", generation, err)
	}
	record, err := l.GetTurn(ctx, "turn-preview")
	if err != nil || record.State != TurnRejected || record.RejectReason != "preview rejection" {
		t.Fatalf("migrated preview record = %#v, %v", record, err)
	}
	if record.RuntimeSessionID != "" || record.CorrelationID != "" {
		t.Fatalf("legacy identity defaults = %q/%q, want empty", record.RuntimeSessionID, record.CorrelationID)
	}
	if _, _, err := l.AdmitTurn(
		ctx, "turn-new", "task-new", 1, "sha256:new", testRuntimeSessionID, testCorrelationID,
	); err != nil {
		t.Fatalf("admit after migration: %v", err)
	}
}
