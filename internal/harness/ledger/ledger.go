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
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

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

// TurnRecord is one durable admission record.
type TurnRecord struct {
	TurnID                string
	TaskUID               string
	Attempt               int32
	RequestDigest         string
	State                 TurnState
	RejectReason          string
	TerminalReceipt       []byte
	TerminalReceiptDigest string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// Settled reports whether the record needs no further settlement work.
func (r TurnRecord) Settled() bool {
	switch r.State {
	case TurnRejected, TurnTerminal, TurnOutcomeUnknown:
		return true
	default:
		return false
	}
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

// Open opens (creating if needed) the ledger database at path.
func Open(path string) (*Ledger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("ledger path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open wrapper admission ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=FULL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure wrapper admission ledger: %w", err)
		}
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Ledger{db: db}, nil
}

// Close closes the ledger database.
func (l *Ledger) Close() error {
	return l.db.Close()
}

func migrate(db *sql.DB) error {
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
			state                   TEXT NOT NULL CHECK(state IN ('Admitted','Accepted','Rejected','Terminal','OutcomeUnknown')),
			reject_reason           TEXT NOT NULL DEFAULT '',
			terminal_receipt        BLOB,
			terminal_receipt_digest TEXT NOT NULL DEFAULT '',
			created_at              TIMESTAMP NOT NULL,
			updated_at              TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_turn_admissions_unsettled
			ON turn_admissions(state, updated_at ASC)
			WHERE state IN ('Admitted','Accepted')`,
		`INSERT INTO ledger_control (key, value) VALUES ('generation', '1')
			ON CONFLICT(key) DO NOTHING`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migrate wrapper admission ledger: %w", err)
		}
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
func (l *Ledger) AdmitTurn(ctx context.Context, turnID, taskUID string, attempt int32, requestDigest string) (AdmitOutcome, *TurnRecord, error) {
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(taskUID) == "" ||
		attempt < 1 || strings.TrimSpace(requestDigest) == "" {
		return "", nil, fmt.Errorf("turn ID, task UID, positive attempt, and request digest are required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("begin turn admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanTurn(tx.QueryRowContext(ctx, turnSelectSQL+` WHERE turn_id = ?`, turnID))
	switch {
	case err == nil:
		if existing.RequestDigest != requestDigest {
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
		(turn_id, task_uid, attempt, request_digest, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		turnID, taskUID, attempt, requestDigest, string(TurnAdmitted), now, now); err != nil {
		return "", nil, fmt.Errorf("admit turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("commit turn admission: %w", err)
	}
	return AdmitOutcomeAdmitted, &TurnRecord{
		TurnID: turnID, TaskUID: taskUID, Attempt: attempt, RequestDigest: requestDigest,
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
	target := TurnTerminal
	if outcomeUnknown {
		target = TurnOutcomeUnknown
	}
	digest := ReceiptDigest(receipt)
	return l.transition(ctx, turnID, target, "", receipt, digest,
		[]TurnState{TurnAdmitted, TurnAccepted, target})
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

// Generation returns the ledger generation watermark for inventory reports.
func (l *Ledger) Generation(ctx context.Context) (string, error) {
	var value string
	err := l.db.QueryRowContext(ctx, `SELECT value FROM ledger_control WHERE key = 'generation'`).Scan(&value)
	if err != nil {
		return "", fmt.Errorf("read ledger generation: %w", err)
	}
	return value, nil
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

const turnSelectSQL = `SELECT turn_id, task_uid, attempt, request_digest, state,
	reject_reason, terminal_receipt, terminal_receipt_digest, created_at, updated_at
	FROM turn_admissions`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTurn(row rowScanner) (*TurnRecord, error) {
	var (
		record TurnRecord
		state  string
	)
	if err := row.Scan(&record.TurnID, &record.TaskUID, &record.Attempt, &record.RequestDigest,
		&state, &record.RejectReason, &record.TerminalReceipt, &record.TerminalReceiptDigest,
		&record.CreatedAt, &record.UpdatedAt); err != nil {
		return nil, err
	}
	record.State = TurnState(state)
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return &record, nil
}
