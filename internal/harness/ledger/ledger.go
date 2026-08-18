/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package ledger implements the durable harness v1 wrapper admission ledger.
//
// The persisted-frame journal remains the cross-restart idempotency backstop
// after the first mapped frame is durable. This ledger closes only the gaps
// the frame journal cannot cover:
//
//   - admission before the first frame is persisted;
//   - acceptance when the response is lost;
//   - idempotent handling of the same turn ID and request digest;
//   - permanent rejection of the same turn ID with a different digest;
//   - durable admission-close and drain inventory;
//   - terminal or explicit OutcomeUnknown receipts surviving wrapper restart.
//
// It does not duplicate frame storage, transcript persistence, or frame
// deduplication. It lives on a dedicated wrapper PVC, separate from the
// controller store.
package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const maxTurnOutputBytes = 50 << 20

// TurnState is the durable ledger state of one admitted turn.
type TurnState string

const (
	// TurnAdmitted records admission before any frame is durable.
	TurnAdmitted TurnState = "Admitted"
	// TurnAccepted records that the runtime accepted the turn, even when the
	// acceptance response was lost.
	TurnAccepted TurnState = "Accepted"
	// TurnRejected is a durable ledger-backed non-acceptance proof: no
	// executor accepted the request. It is the only state that proves a safe
	// resend is possible.
	TurnRejected TurnState = "Rejected"
	// TurnTerminal records an authoritative terminal receipt.
	TurnTerminal TurnState = "Terminal"
	// TurnOutcomeUnknown records an explicit permanent unknown outcome.
	TurnOutcomeUnknown TurnState = "OutcomeUnknown"
)

// ErrAdmissionClosed rejects new admissions after the durable admission close.
var ErrAdmissionClosed = errors.New("wrapper turn admission is closed")

// ErrDigestMismatch permanently rejects a turn ID replayed with a different
// request digest.
var ErrDigestMismatch = errors.New("turn ID was already admitted with a different request digest")

// ErrNotFound reports a missing turn record.
var ErrNotFound = errors.New("turn record not found")

// ErrOutputAcknowledgementMismatch reports an acknowledgement that is not
// fenced to the output and terminal receipt durably recorded for the turn.
var ErrOutputAcknowledgementMismatch = errors.New("turn output acknowledgement does not match durable receipt")

// ErrSettlementAcknowledgementMismatch reports a controller acknowledgement
// that is not fenced to the exact durable request and terminal evidence.
var ErrSettlementAcknowledgementMismatch = errors.New("turn settlement acknowledgement does not match durable evidence")

const retiredGenerationPrefix = "retired:"

// TurnRecord is one durable admission record.
type TurnRecord struct {
	TurnID                string
	TaskUID               string
	Attempt               int32
	RequestDigest         string
	RuntimeSessionID      string
	CorrelationID         string
	State                 TurnState
	RejectReason          string
	TerminalReceipt       []byte
	TerminalReceiptDigest string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	SettledAt             *time.Time
}

// TurnOutput is the bounded output payload referenced by a terminal receipt.
// It is committed atomically with the receipt so a durable OutputRef never
// points at process-local or partially persisted state.
type TurnOutput struct {
	Ref  string
	Data []byte
}

// AdmitOutcome describes the result of an admission request.
type AdmitOutcome string

const (
	// AdmitOutcomeAdmitted is a fresh admission.
	AdmitOutcomeAdmitted AdmitOutcome = "admitted"
	// AdmitOutcomeDuplicate is an idempotent replay of the same turn ID and
	// request digest; the existing record is returned unchanged.
	AdmitOutcomeDuplicate AdmitOutcome = "duplicate"
)

// Ledger is the wrapper-local durable admission ledger.
type Ledger struct {
	db *sql.DB
}

// OpenWithGeneration opens the ledger and initializes a newly created control
// table with the caller's deployment generation. An existing ledger keeps its
// durable generation and still requires the explicit close/drain rollover CAS.
func OpenWithGeneration(path, initialGeneration string) (*Ledger, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("ledger path is required")
	}
	if err := validateGeneration(initialGeneration); err != nil {
		return nil, err
	}
	var err error
	path, err = prepareLedgerPath(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open wrapper admission ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	for _, pragma := range []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=FULL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure wrapper admission ledger: %w", err)
		}
	}
	if err := migrate(db, strings.TrimSpace(initialGeneration)); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := secureLedgerFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Ledger{db: db}, nil
}

func prepareLedgerPath(path string) (string, error) {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	volumeRoot := filepath.Clean(filepath.VolumeName(parent) + string(filepath.Separator))
	if parent == "." || parent == volumeRoot {
		return "", fmt.Errorf("wrapper admission ledger path must use a dedicated directory")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create wrapper admission ledger directory: %w", err)
	}
	if err := secureLedgerPath(parent, true, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return "", fmt.Errorf("create wrapper admission ledger: %w", createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", fmt.Errorf("close new wrapper admission ledger: %w", closeErr)
		}
	case err != nil:
		return "", fmt.Errorf("inspect wrapper admission ledger: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		return "", fmt.Errorf("wrapper admission ledger must be a regular file")
	}
	if err := secureLedgerPath(path, false, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func secureLedgerFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := secureLedgerPath(candidate, false, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func secureLedgerPath(path string, directory bool, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		kind := "file"
		if directory {
			kind = "directory"
		}
		return fmt.Errorf("wrapper admission ledger %s must be a non-symlink %s", path, kind)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("restrict wrapper admission ledger %s permissions: %w", path, err)
	}
	info, err = os.Lstat(path)
	if err != nil {
		return fmt.Errorf("verify wrapper admission ledger %s permissions: %w", path, err)
	}
	if info.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("wrapper admission ledger %s permissions are %04o, want %04o", path, info.Mode().Perm(), mode.Perm())
	}
	return nil
}

// Close closes the ledger database.
func (l *Ledger) Close() error {
	return l.db.Close()
}

func migrate(db *sql.DB, initialGeneration string) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ledger_control (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS turn_admissions (
			turn_id                 TEXT PRIMARY KEY,
			task_uid                TEXT NOT NULL,
			attempt                 INTEGER NOT NULL CHECK(attempt > 0),
			request_digest          TEXT NOT NULL,
			runtime_session_id      TEXT NOT NULL DEFAULT '',
			correlation_id          TEXT NOT NULL DEFAULT '',
			state                   TEXT NOT NULL CHECK(state IN ('Admitted','Accepted','Rejected','Terminal','OutcomeUnknown')),
			reject_reason           TEXT NOT NULL DEFAULT '',
			terminal_receipt        BLOB,
			terminal_receipt_digest TEXT NOT NULL DEFAULT '',
			created_at              TIMESTAMP NOT NULL,
			updated_at              TIMESTAMP NOT NULL,
			settled_at              TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_turn_admissions_unsettled
			ON turn_admissions(state, updated_at ASC)
			WHERE state IN ('Admitted','Accepted')`,
		`CREATE TABLE IF NOT EXISTS turn_outputs (
			turn_id        TEXT PRIMARY KEY,
			output_ref     TEXT NOT NULL,
			output_payload BLOB NOT NULL,
			output_digest  TEXT NOT NULL,
			output_size    INTEGER NOT NULL CHECK(output_size >= 0),
			created_at     TIMESTAMP NOT NULL,
			FOREIGN KEY(turn_id) REFERENCES turn_admissions(turn_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS turn_output_acknowledgements (
			turn_id                 TEXT PRIMARY KEY,
			output_ref              TEXT NOT NULL,
			output_digest           TEXT NOT NULL,
			terminal_receipt_digest TEXT NOT NULL,
			acknowledged_at          TIMESTAMP NOT NULL,
			FOREIGN KEY(turn_id) REFERENCES turn_admissions(turn_id) ON DELETE CASCADE
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migrate wrapper admission ledger: %w", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO ledger_control (key, value) VALUES ('generation', ?)
		ON CONFLICT(key) DO NOTHING`, initialGeneration); err != nil {
		return fmt.Errorf("initialize wrapper admission ledger generation: %w", err)
	}
	// These ALTERs support ledgers created by the first coexistence preview.
	// Existing unsettled rows are reconciled conservatively by the wrapper;
	// terminal rows already carry their exact identity inside the receipt.
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "runtime_session_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "correlation_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "settled_at", definition: "TIMESTAMP"},
	} {
		if err := ensureTurnAdmissionColumn(db, column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_turn_admissions_reclaimable
		ON turn_admissions(settled_at, updated_at ASC)
		WHERE settled_at IS NOT NULL`); err != nil {
		return fmt.Errorf("index reclaimable wrapper admission ledger turns: %w", err)
	}
	return nil
}

func ensureTurnAdmissionColumn(db *sql.DB, name, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(turn_admissions)`)
	if err != nil {
		return fmt.Errorf("inspect wrapper admission ledger columns: %w", err)
	}
	found := false
	for rows.Next() {
		var (
			cid       int
			column    string
			dataType  string
			notNull   int
			defaultV  sql.NullString
			primaryID int
		)
		if err := rows.Scan(&cid, &column, &dataType, &notNull, &defaultV, &primaryID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan wrapper admission ledger columns: %w", err)
		}
		if column == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close wrapper admission ledger column scan: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect wrapper admission ledger columns: %w", err)
	}
	if found {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE turn_admissions ADD COLUMN ` + name + ` ` + definition); err != nil {
		return fmt.Errorf("add wrapper admission ledger column %s: %w", name, err)
	}
	return nil
}

// ReceiptDigest returns the canonical digest of a terminal receipt payload.
func ReceiptDigest(receipt []byte) string {
	sum := sha256.Sum256(receipt)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// AdmitTurn durably admits one turn before StartTurn is written. The same
// turn ID with the same request digest is idempotent; the same turn ID with a
// different digest fails permanently; a closed admission fails closed.
func (l *Ledger) AdmitTurn(
	ctx context.Context,
	turnID, taskUID string,
	attempt int32,
	requestDigest, runtimeSessionID, correlationID string,
) (AdmitOutcome, *TurnRecord, error) {
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(taskUID) == "" ||
		attempt < 1 || strings.TrimSpace(requestDigest) == "" ||
		strings.TrimSpace(runtimeSessionID) == "" || strings.TrimSpace(correlationID) == "" {
		return "", nil, fmt.Errorf("turn ID, task UID, positive attempt, request digest, runtime session ID, and correlation ID are required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("begin turn admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanTurn(tx.QueryRowContext(ctx, turnSelectSQL+` WHERE turn_id = ?`, turnID))
	switch {
	case err == nil:
		if existing.RequestDigest != requestDigest ||
			existing.RuntimeSessionID != runtimeSessionID || existing.CorrelationID != correlationID {
			return "", nil, fmt.Errorf("%w: turn %s", ErrDigestMismatch, turnID)
		}
		if err := tx.Commit(); err != nil {
			return "", nil, fmt.Errorf("commit duplicate turn admission: %w", err)
		}
		return AdmitOutcomeDuplicate, existing, nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return "", nil, fmt.Errorf("read turn admission: %w", err)
	}

	closed, _, err := admissionClosed(ctx, tx)
	if err != nil {
		return "", nil, err
	}
	if closed {
		return "", nil, ErrAdmissionClosed
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO turn_admissions
		(turn_id, task_uid, attempt, request_digest, runtime_session_id, correlation_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		turnID, taskUID, attempt, requestDigest, runtimeSessionID, correlationID, string(TurnAdmitted), now, now); err != nil {
		return "", nil, fmt.Errorf("admit turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("commit turn admission: %w", err)
	}
	return AdmitOutcomeAdmitted, &TurnRecord{
		TurnID: turnID, TaskUID: taskUID, Attempt: attempt, RequestDigest: requestDigest,
		RuntimeSessionID: runtimeSessionID, CorrelationID: correlationID,
		State: TurnAdmitted, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// MarkTurnAccepted durably records acceptance. Idempotent for already-accepted
// turns; invalid from settled states.
func (l *Ledger) MarkTurnAccepted(ctx context.Context, turnID string) error {
	return l.transition(ctx, turnID, TurnAccepted, "", nil, "", []TurnState{TurnAdmitted, TurnAccepted})
}

// MarkTurnRejected durably records a definitive non-acceptance. It is the only
// submission-state proof that permits a safe resend through a new attempt.
func (l *Ledger) MarkTurnRejected(ctx context.Context, turnID, reason string) error {
	return l.transition(ctx, turnID, TurnRejected, reason, nil, "", []TurnState{TurnAdmitted, TurnRejected})
}

// RecordTurnTerminal durably records the authoritative terminal receipt, or an
// explicit permanent OutcomeUnknown when outcomeUnknown is true. A replay with
// the same receipt digest is idempotent; a conflicting receipt fails.
func (l *Ledger) RecordTurnTerminal(ctx context.Context, turnID string, receipt []byte, outcomeUnknown bool) error {
	return l.RecordTurnTerminalWithOutput(ctx, turnID, receipt, outcomeUnknown, nil)
}

// RecordTurnTerminalWithOutput atomically records the canonical terminal
// receipt and its optional locally referenced output payload.
func (l *Ledger) RecordTurnTerminalWithOutput(
	ctx context.Context,
	turnID string,
	receipt []byte,
	outcomeUnknown bool,
	output *TurnOutput,
) error {
	if strings.TrimSpace(turnID) == "" || len(receipt) == 0 {
		return fmt.Errorf("turn ID and terminal receipt are required")
	}
	if output != nil {
		if strings.TrimSpace(output.Ref) == "" {
			return fmt.Errorf("turn output ref is required")
		}
		if len(output.Data) > maxTurnOutputBytes {
			return fmt.Errorf("turn output exceeds durable ledger limit")
		}
	}
	target := TurnTerminal
	if outcomeUnknown {
		target = TurnOutcomeUnknown
	}
	digest := ReceiptDigest(receipt)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin terminal turn transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanTurn(tx.QueryRowContext(ctx, turnSelectSQL+` WHERE turn_id = ?`, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return fmt.Errorf("read turn admission: %w", err)
	}
	if existing.State == target {
		if existing.TerminalReceiptDigest != digest {
			return fmt.Errorf("turn %s already recorded a different terminal receipt", turnID)
		}
		if output != nil {
			var acknowledgedRef, acknowledgedOutputDigest, acknowledgedReceiptDigest string
			ackErr := tx.QueryRowContext(ctx, `SELECT output_ref, output_digest, terminal_receipt_digest
				FROM turn_output_acknowledgements WHERE turn_id = ?`, turnID).Scan(
				&acknowledgedRef, &acknowledgedOutputDigest, &acknowledgedReceiptDigest,
			)
			if ackErr == nil {
				if acknowledgedRef != output.Ref || acknowledgedOutputDigest != ReceiptDigest(output.Data) ||
					acknowledgedReceiptDigest != digest {
					return fmt.Errorf("turn %s already acknowledged different terminal output", turnID)
				}
				return tx.Commit()
			}
			if !errors.Is(ackErr, sql.ErrNoRows) {
				return fmt.Errorf("read terminal turn output acknowledgement: %w", ackErr)
			}
			persisted, outputErr := getTurnOutput(ctx, tx, turnID, output.Ref)
			if outputErr != nil || !bytes.Equal(persisted, output.Data) {
				return fmt.Errorf("turn %s already recorded different terminal output", turnID)
			}
		}
		return tx.Commit()
	}
	if existing.State != TurnAdmitted && existing.State != TurnAccepted {
		return fmt.Errorf("turn %s cannot move from %s to %s", turnID, existing.State, target)
	}
	if outcomeUnknown && output != nil {
		return fmt.Errorf("outcome-unknown turn cannot store referenced output")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE turn_admissions SET
			state = ?, reject_reason = '', terminal_receipt = ?, terminal_receipt_digest = ?, updated_at = ?
			WHERE turn_id = ?`, string(target), receipt, digest, now, turnID); err != nil {
		return fmt.Errorf("transition terminal turn: %w", err)
	}
	if output != nil {
		outputDigest := ReceiptDigest(output.Data)
		if _, err := tx.ExecContext(ctx, `INSERT INTO turn_outputs
			(turn_id, output_ref, output_payload, output_digest, output_size, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			turnID, output.Ref, output.Data, outputDigest, len(output.Data), now); err != nil {
			return fmt.Errorf("persist terminal turn output: %w", err)
		}
	}
	return tx.Commit()
}

func (l *Ledger) transition(ctx context.Context, turnID string, target TurnState, rejectReason string, receipt []byte, receiptDigest string, validFrom []TurnState) error {
	if strings.TrimSpace(turnID) == "" {
		return fmt.Errorf("turn ID is required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin turn transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanTurn(tx.QueryRowContext(ctx, turnSelectSQL+` WHERE turn_id = ?`, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return fmt.Errorf("read turn admission: %w", err)
	}

	if existing.State == target {
		// Idempotent replay; terminal receipts must match exactly.
		if (target == TurnTerminal || target == TurnOutcomeUnknown) && existing.TerminalReceiptDigest != receiptDigest {
			return fmt.Errorf("turn %s already recorded a different terminal receipt", turnID)
		}
		return tx.Commit()
	}
	allowed := slices.Contains(validFrom, existing.State)
	if !allowed {
		return fmt.Errorf("turn %s cannot move from %s to %s", turnID, existing.State, target)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE turn_admissions SET
			state = ?, reject_reason = ?, terminal_receipt = ?, terminal_receipt_digest = ?, updated_at = ?
		WHERE turn_id = ?`,
		string(target), rejectReason, receipt, receiptDigest, time.Now().UTC(), turnID); err != nil {
		return fmt.Errorf("transition turn: %w", err)
	}
	return tx.Commit()
}

// GetTurn returns one durable turn record.
func (l *Ledger) GetTurn(ctx context.Context, turnID string) (*TurnRecord, error) {
	record, err := scanTurn(l.db.QueryRowContext(ctx, turnSelectSQL+` WHERE turn_id = ?`, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return nil, fmt.Errorf("get turn admission: %w", err)
	}
	return record, nil
}

// GetTurnOutput returns a validated output payload only for a terminal turn
// and the exact opaque reference recorded with its terminal receipt.
func (l *Ledger) GetTurnOutput(ctx context.Context, turnID, outputRef string) ([]byte, error) {
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(outputRef) == "" {
		return nil, fmt.Errorf("turn ID and output ref are required")
	}
	return getTurnOutput(ctx, l.db, turnID, outputRef)
}

// AcknowledgeTurnOutput atomically records a receipt-bound tombstone and
// deletes the acknowledged output payload. The tombstone makes a lost
// acknowledgement response or controller restart safe to retry without
// retaining the potentially large blob.
func (l *Ledger) AcknowledgeTurnOutput(
	ctx context.Context,
	turnID, outputRef, terminalReceiptDigest string,
) error {
	turnID = strings.TrimSpace(turnID)
	outputRef = strings.TrimSpace(outputRef)
	terminalReceiptDigest = strings.TrimSpace(terminalReceiptDigest)
	if turnID == "" || outputRef == "" {
		return fmt.Errorf("turn ID and output ref are required")
	}
	if !isCanonicalReceiptDigest(terminalReceiptDigest) {
		return fmt.Errorf("terminal receipt digest must be a canonical sha256 digest")
	}

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin terminal turn output acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state, persistedReceiptDigest string
	err = tx.QueryRowContext(ctx, `SELECT state, terminal_receipt_digest
		FROM turn_admissions WHERE turn_id = ?`, turnID).Scan(&state, &persistedReceiptDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return fmt.Errorf("read terminal turn for output acknowledgement: %w", err)
	}
	if TurnState(state) != TurnTerminal || persistedReceiptDigest != terminalReceiptDigest {
		return fmt.Errorf("%w: turn %s", ErrOutputAcknowledgementMismatch, turnID)
	}

	var acknowledgedRef, acknowledgedReceiptDigest string
	err = tx.QueryRowContext(ctx, `SELECT output_ref, terminal_receipt_digest
		FROM turn_output_acknowledgements WHERE turn_id = ?`, turnID).Scan(
		&acknowledgedRef, &acknowledgedReceiptDigest,
	)
	if err == nil {
		if acknowledgedRef != outputRef || acknowledgedReceiptDigest != terminalReceiptDigest {
			return fmt.Errorf("%w: turn %s", ErrOutputAcknowledgementMismatch, turnID)
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read terminal turn output acknowledgement: %w", err)
	}

	var persistedRef, outputDigest string
	err = tx.QueryRowContext(ctx, `SELECT output_ref, output_digest FROM turn_outputs WHERE turn_id = ?`, turnID).Scan(
		&persistedRef, &outputDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: output for turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return fmt.Errorf("read terminal turn output for acknowledgement: %w", err)
	}
	if persistedRef != outputRef {
		return fmt.Errorf("%w: turn %s", ErrOutputAcknowledgementMismatch, turnID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO turn_output_acknowledgements
		(turn_id, output_ref, output_digest, terminal_receipt_digest, acknowledged_at)
		VALUES (?, ?, ?, ?, ?)`, turnID, persistedRef, outputDigest, terminalReceiptDigest, time.Now().UTC()); err != nil {
		return fmt.Errorf("persist terminal turn output acknowledgement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM turn_outputs WHERE turn_id = ?`, turnID); err != nil {
		return fmt.Errorf("delete acknowledged terminal turn output: %w", err)
	}
	return tx.Commit()
}

// AcknowledgeTurnSettlement records that the controller durably settled the
// exact request and terminal evidence. Only acknowledged rows are eligible for
// later retention-based reclamation.
func (l *Ledger) AcknowledgeTurnSettlement(
	ctx context.Context,
	turnID, requestDigest, terminalReceiptDigest string,
) error {
	turnID = strings.TrimSpace(turnID)
	requestDigest = strings.TrimSpace(requestDigest)
	terminalReceiptDigest = strings.TrimSpace(terminalReceiptDigest)
	if turnID == "" || !isCanonicalReceiptDigest(requestDigest) {
		return fmt.Errorf("turn ID and canonical request digest are required")
	}
	if terminalReceiptDigest != "" && !isCanonicalReceiptDigest(terminalReceiptDigest) {
		return fmt.Errorf("terminal receipt digest must be empty or a canonical sha256 digest")
	}

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin turn settlement acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state, persistedRequestDigest, persistedReceiptDigest string
	var settledAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT state, request_digest, terminal_receipt_digest, settled_at
		FROM turn_admissions WHERE turn_id = ?`, turnID).Scan(
		&state, &persistedRequestDigest, &persistedReceiptDigest, &settledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return fmt.Errorf("read turn settlement evidence: %w", err)
	}
	if persistedRequestDigest != requestDigest {
		return fmt.Errorf("%w: turn %s", ErrSettlementAcknowledgementMismatch, turnID)
	}
	switch TurnState(state) {
	case TurnRejected:
		if terminalReceiptDigest != "" || persistedReceiptDigest != "" {
			return fmt.Errorf("%w: turn %s", ErrSettlementAcknowledgementMismatch, turnID)
		}
	case TurnTerminal, TurnOutcomeUnknown:
		if terminalReceiptDigest == "" || terminalReceiptDigest != persistedReceiptDigest {
			return fmt.Errorf("%w: turn %s", ErrSettlementAcknowledgementMismatch, turnID)
		}
		var outputCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_outputs WHERE turn_id = ?`, turnID).Scan(&outputCount); err != nil {
			return fmt.Errorf("read unsettled terminal turn output count: %w", err)
		}
		if outputCount != 0 {
			return fmt.Errorf("%w: turn %s still has unacknowledged output", ErrSettlementAcknowledgementMismatch, turnID)
		}
	default:
		return fmt.Errorf("%w: turn %s is not terminal", ErrSettlementAcknowledgementMismatch, turnID)
	}
	if settledAt.Valid {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turn_admissions SET settled_at = ?
		WHERE turn_id = ? AND settled_at IS NULL`, time.Now().UTC(), turnID); err != nil {
		return fmt.Errorf("acknowledge turn settlement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit turn settlement acknowledgement: %w", err)
	}
	return nil
}

// ReclaimSettledTurnsBefore removes a bounded batch of controller-acknowledged
// rows only after the configured retention cutoff. Child output and
// acknowledgement rows are deleted by the foreign-key cascade.
func (l *Ledger) ReclaimSettledTurnsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if cutoff.IsZero() {
		return 0, fmt.Errorf("settled turn reclamation cutoff is required")
	}
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("settled turn reclamation limit must be between 1 and 1000")
	}
	result, err := l.db.ExecContext(ctx, `DELETE FROM turn_admissions WHERE turn_id IN (
		SELECT turn_id FROM turn_admissions
		WHERE settled_at IS NOT NULL AND settled_at <= ?
		ORDER BY settled_at ASC, turn_id ASC LIMIT ?
	)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("reclaim settled wrapper turns: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count reclaimed wrapper turns: %w", err)
	}
	return count, nil
}

func isCanonicalReceiptDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	encoded := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size && encoded == strings.ToLower(encoded)
}

type queryScanner interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getTurnOutput(ctx context.Context, q queryScanner, turnID, outputRef string) ([]byte, error) {
	var (
		state   string
		ref     string
		payload []byte
		digest  string
		size    int
	)
	err := q.QueryRowContext(ctx, `SELECT a.state, o.output_ref, o.output_payload, o.output_digest, o.output_size
		FROM turn_admissions AS a
		JOIN turn_outputs AS o ON o.turn_id = a.turn_id
		WHERE a.turn_id = ?`, turnID).Scan(&state, &ref, &payload, &digest, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: output for turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return nil, fmt.Errorf("read terminal turn output: %w", err)
	}
	if TurnState(state) != TurnTerminal || ref != outputRef {
		return nil, fmt.Errorf("%w: output for turn %s", ErrNotFound, turnID)
	}
	if size != len(payload) || size < 0 || size > maxTurnOutputBytes || ReceiptDigest(payload) != digest {
		return nil, fmt.Errorf("corrupt terminal turn output")
	}
	return append([]byte(nil), payload...), nil
}

// ListUnsettledTurns returns every Admitted or Accepted turn for drain
// inventory and shutdown proof.
func (l *Ledger) ListUnsettledTurns(ctx context.Context) ([]TurnRecord, error) {
	rows, err := l.db.QueryContext(ctx, turnSelectSQL+`
		WHERE state IN ('Admitted','Accepted') ORDER BY updated_at ASC, turn_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list unsettled turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []TurnRecord
	for rows.Next() {
		record, err := scanTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("scan turn admission: %w", err)
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

// CloseAdmission durably closes new turn admission. Idempotent.
func (l *Ledger) CloseAdmission(ctx context.Context) error {
	if _, err := l.db.ExecContext(ctx, `INSERT INTO ledger_control (key, value)
		VALUES ('admission-closed-at', ?)
		ON CONFLICT(key) DO NOTHING`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("close turn admission: %w", err)
	}
	return nil
}

// AdmissionClosed reports whether admission is durably closed and when.
func (l *Ledger) AdmissionClosed(ctx context.Context) (bool, time.Time, error) {
	return admissionClosed(ctx, l.db)
}

// PrepareRollover records the exact generation that may reopen admission after
// a completed drain. It is idempotent for the same generation. A later rollout
// may supersede an unactivated preparation because admission is still closed
// and the no-unsettled-turn invariant is rechecked in the same transaction.
func (l *Ledger) PrepareRollover(ctx context.Context, nextGeneration string) (string, error) {
	if err := validateGeneration(nextGeneration); err != nil {
		return "", err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin wrapper ledger rollover preparation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, found, err := controlValue(ctx, tx, "generation")
	if err != nil || !found {
		if err == nil {
			err = errors.New("generation is missing")
		}
		return "", fmt.Errorf("read wrapper ledger generation: %w", err)
	}
	if current == nextGeneration {
		return "", fmt.Errorf("next wrapper ledger generation must differ from current generation")
	}
	closed, _, err := admissionClosed(ctx, tx)
	if err != nil {
		return "", err
	}
	if !closed {
		return "", fmt.Errorf("wrapper admission must be closed before rollover preparation")
	}
	unsettled, err := unsettledTurnCount(ctx, tx)
	if err != nil {
		return "", err
	}
	if unsettled != 0 {
		return "", fmt.Errorf("wrapper drain is incomplete: %d turn(s) remain unsettled", unsettled)
	}
	pending, pendingFound, err := controlValue(ctx, tx, "pending-generation")
	if err != nil {
		return "", fmt.Errorf("read pending wrapper ledger generation: %w", err)
	}
	if pendingFound {
		if pending == nextGeneration {
			if err := tx.Commit(); err != nil {
				return "", fmt.Errorf("commit idempotent wrapper ledger rollover preparation: %w", err)
			}
			return current, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ledger_control SET value = ? WHERE key = 'pending-generation'`, nextGeneration); err != nil {
			return "", fmt.Errorf("supersede wrapper ledger generation preparation: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_control (key, value) VALUES ('pending-generation', ?)`, nextGeneration); err != nil {
		return "", fmt.Errorf("prepare wrapper ledger generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit wrapper ledger rollover preparation: %w", err)
	}
	return current, nil
}

// AbortRollover atomically discards a prepared replacement and reopens the
// fully drained current generation. The exact current generation is required
// so a stale rollback hook can never reopen a different wrapper generation.
// An already-open matching generation is an idempotent success; a bare close
// without a prepared rollover remains fail-closed.
func (l *Ledger) AbortRollover(ctx context.Context, expectedGeneration string) error {
	if err := validateGeneration(expectedGeneration); err != nil {
		return err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin wrapper ledger rollover abort: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, found, err := controlValue(ctx, tx, "generation")
	if err != nil || !found {
		if err == nil {
			err = errors.New("generation is missing")
		}
		return fmt.Errorf("read wrapper ledger generation: %w", err)
	}
	if current != expectedGeneration {
		return fmt.Errorf("wrapper ledger generation %q does not match rollback generation %q", current, expectedGeneration)
	}
	pending, pendingFound, err := controlValue(ctx, tx, "pending-generation")
	if err != nil {
		return fmt.Errorf("read pending wrapper ledger generation: %w", err)
	}
	closed, _, err := admissionClosed(ctx, tx)
	if err != nil {
		return err
	}
	if !pendingFound {
		if closed {
			return fmt.Errorf("wrapper admission is closed without a prepared rollover")
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit idempotent wrapper ledger rollover abort: %w", err)
		}
		return nil
	}
	if err := validateGeneration(pending); err != nil {
		return fmt.Errorf("pending wrapper ledger generation is invalid: %w", err)
	}
	if !closed {
		return fmt.Errorf("prepared wrapper rollover is missing its admission close")
	}
	unsettled, err := unsettledTurnCount(ctx, tx)
	if err != nil {
		return err
	}
	if unsettled != 0 {
		return fmt.Errorf("wrapper drain is incomplete: %d turn(s) remain unsettled", unsettled)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ledger_control WHERE key IN ('pending-generation', 'admission-closed-at')`); err != nil {
		return fmt.Errorf("abort wrapper ledger rollover: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit wrapper ledger rollover abort: %w", err)
	}
	return nil
}

// ActivateGeneration atomically reopens a fully drained ledger only for the
// exact generation prepared by the prior wrapper. A deliberate retirement
// marker may be replaced by the first later enabled generation. Starting the
// same generation is otherwise permitted but never removes a durable close.
func (l *Ledger) ActivateGeneration(ctx context.Context, desiredGeneration string) error {
	if err := validateGeneration(desiredGeneration); err != nil {
		return err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin wrapper ledger generation activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, found, err := controlValue(ctx, tx, "generation")
	if err != nil || !found {
		if err == nil {
			err = errors.New("generation is missing")
		}
		return fmt.Errorf("read wrapper ledger generation: %w", err)
	}
	pending, pendingFound, err := controlValue(ctx, tx, "pending-generation")
	if err != nil {
		return fmt.Errorf("read pending wrapper ledger generation: %w", err)
	}
	retirementPrepared := pendingFound && isRetiredGeneration(pending)
	if current == desiredGeneration && !retirementPrepared {
		return tx.Commit()
	}
	if !pendingFound || pending != desiredGeneration && !retirementPrepared {
		return fmt.Errorf("wrapper ledger generation %q was not prepared", desiredGeneration)
	}
	closed, _, err := admissionClosed(ctx, tx)
	if err != nil {
		return err
	}
	if !closed {
		return fmt.Errorf("wrapper admission must remain closed until generation activation")
	}
	unsettled, err := unsettledTurnCount(ctx, tx)
	if err != nil {
		return err
	}
	if unsettled != 0 {
		return fmt.Errorf("wrapper drain is incomplete: %d turn(s) remain unsettled", unsettled)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ledger_control SET value = ? WHERE key = 'generation'`, desiredGeneration); err != nil {
		return fmt.Errorf("activate wrapper ledger generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ledger_control WHERE key IN ('pending-generation', 'admission-closed-at')`); err != nil {
		return fmt.Errorf("reopen wrapper admission for activated generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit wrapper ledger generation activation: %w", err)
	}
	return nil
}

func isRetiredGeneration(value string) bool {
	digest := strings.TrimPrefix(strings.TrimSpace(value), retiredGenerationPrefix)
	if digest == value || len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func validateGeneration(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("wrapper ledger generation must contain 1 to 128 characters")
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return fmt.Errorf("wrapper ledger generation contains unsupported characters")
	}
	return nil
}

func controlValue(ctx context.Context, q queryRower, key string) (string, bool, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT value FROM ledger_control WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func unsettledTurnCount(ctx context.Context, q queryRower) (int, error) {
	var count int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_admissions WHERE state IN ('Admitted','Accepted')`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unsettled wrapper turns: %w", err)
	}
	return count, nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func admissionClosed(ctx context.Context, q queryRower) (bool, time.Time, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT value FROM ledger_control WHERE key = 'admission-closed-at'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("read admission close marker: %w", err)
	}
	closedAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return true, time.Time{}, nil //nolint:nilerr // A malformed marker still proves closure.
	}
	return true, closedAt, nil
}

const turnSelectSQL = `SELECT turn_id, task_uid, attempt, request_digest,
	runtime_session_id, correlation_id, state,
	reject_reason, terminal_receipt, terminal_receipt_digest, created_at, updated_at, settled_at
	FROM turn_admissions`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTurn(row rowScanner) (*TurnRecord, error) {
	var (
		record    TurnRecord
		state     string
		settledAt sql.NullTime
	)
	if err := row.Scan(&record.TurnID, &record.TaskUID, &record.Attempt, &record.RequestDigest,
		&record.RuntimeSessionID, &record.CorrelationID, &state,
		&record.RejectReason, &record.TerminalReceipt, &record.TerminalReceiptDigest,
		&record.CreatedAt, &record.UpdatedAt, &settledAt); err != nil {
		return nil, err
	}
	record.State = TurnState(state)
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	if settledAt.Valid {
		value := settledAt.Time.UTC()
		record.SettledAt = &value
	}
	if err := validateTurnRecordIntegrity(record); err != nil {
		return nil, err
	}
	return &record, nil
}

func validateTurnRecordIntegrity(record TurnRecord) error {
	switch record.State {
	case TurnTerminal, TurnOutcomeUnknown:
		if len(record.TerminalReceipt) == 0 || strings.TrimSpace(record.TerminalReceiptDigest) == "" {
			return fmt.Errorf("corrupt terminal turn record: receipt and digest are required")
		}
		if ReceiptDigest(record.TerminalReceipt) != record.TerminalReceiptDigest {
			return fmt.Errorf("corrupt terminal turn record: receipt digest mismatch")
		}
	case TurnAdmitted, TurnAccepted, TurnRejected:
		if len(record.TerminalReceipt) != 0 || record.TerminalReceiptDigest != "" {
			return fmt.Errorf("corrupt nonterminal turn record: unexpected terminal receipt")
		}
	default:
		return fmt.Errorf("corrupt turn record: unsupported state %q", record.State)
	}
	return nil
}
