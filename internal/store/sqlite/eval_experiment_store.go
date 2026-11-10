package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

// evalExperimentMaxCaseKeys bounds one reservation's held-out case keys.
const evalExperimentMaxCaseKeys = 10000

func evalExperimentSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS eval_experiments (
			namespace         TEXT NOT NULL,
			candidate_run_uid TEXT NOT NULL,
			baseline_run_uid  TEXT NOT NULL,
			record_json       TEXT NOT NULL,
			reserved_at       TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, candidate_run_uid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_eval_experiments_baseline ON eval_experiments(baseline_run_uid)`,
		`CREATE TABLE IF NOT EXISTS eval_experiment_cases (
			case_key          TEXT NOT NULL,
			namespace         TEXT NOT NULL,
			candidate_run_uid TEXT NOT NULL,
			PRIMARY KEY (case_key, namespace, candidate_run_uid)
		)`,
	}
}

func validateEvalExperimentRecord(record *store.EvalExperimentRecord, limits store.EvalExperimentLimits) error {
	if record == nil {
		return store.ValidationErrorf("eval experiment record is required")
	}
	if record.SchemaVersion != store.EvalExperimentSchemaVersion {
		return store.ValidationErrorf("eval experiment schemaVersion must be %q", store.EvalExperimentSchemaVersion)
	}
	for field, value := range map[string]string{
		"namespace": record.Namespace, "candidateRun": record.CandidateRun, "candidateRunUID": record.CandidateRunUID,
		"baselineRun": record.BaselineRun, "baselineRunUID": record.BaselineRunUID, "caseSetDigest": record.CaseSetDigest,
		"gateDigest": record.GateDigest,
	} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return store.ValidationErrorf("eval experiment %s is required and must not have surrounding whitespace", field)
		}
	}
	if len(record.CaseKeys) == 0 || len(record.CaseKeys) > evalExperimentMaxCaseKeys {
		return store.ValidationErrorf("eval experiment caseKeys must hold 1 to %d keys", evalExperimentMaxCaseKeys)
	}
	for i, key := range record.CaseKeys {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
			return store.ValidationErrorf("eval experiment caseKeys must not be empty or have surrounding whitespace")
		}
		if i > 0 && key <= record.CaseKeys[i-1] {
			return store.ValidationErrorf("eval experiment caseKeys must be sorted and distinct")
		}
	}
	if record.ReservedAt.IsZero() {
		return store.ValidationErrorf("eval experiment reservedAt is required")
	}
	if limits.MaxPerBaseline < 1 || limits.MaxPerCase < 1 {
		return store.ValidationErrorf("eval experiment limits must be at least 1")
	}
	return nil
}

// ReserveEvalExperiment counts and adds an attempt in one write.
func (s *Store) ReserveEvalExperiment(ctx context.Context, record *store.EvalExperimentRecord, limits store.EvalExperimentLimits) (store.EvalExperimentReservation, error) {
	if err := validateEvalExperimentRecord(record, limits); err != nil {
		return store.EvalExperimentReservation{}, err
	}
	normalized := *record
	normalized.ReservedAt = record.ReservedAt.UTC()
	record = &normalized
	encoded, err := json.Marshal(record)
	if err != nil {
		return store.EvalExperimentReservation{}, fmt.Errorf("encode eval experiment record: %w", err)
	}
	return writeOperation(ctx, s, "ReserveEvalExperiment", nil, func(ctx context.Context) (store.EvalExperimentReservation, error) {
		// Count and insert under the writer lock, also without an envelope,
		// so concurrent reservations never overgrant and a failed case insert
		// leaves no partial record.
		var reservation store.EvalExperimentReservation
		err := s.inWriteTransaction(ctx, func(ctx context.Context, _ *sql.Tx) error {
			var err error
			reservation, err = s.reserveEvalExperimentWrite(ctx, record, limits, encoded)
			return err
		})
		return reservation, err
	})
}

func (s *Store) reserveEvalExperimentWrite(ctx context.Context, record *store.EvalExperimentRecord, limits store.EvalExperimentLimits, encoded []byte) (store.EvalExperimentReservation, error) {
	executor := s.taskDataExecutor(ctx)
	var reservation store.EvalExperimentReservation
	var existing string
	err := executor.QueryRowContext(ctx,
		`SELECT record_json FROM eval_experiments WHERE namespace = ? AND candidate_run_uid = ?`,
		record.Namespace, record.CandidateRunUID).Scan(&existing)
	switch {
	case err == nil:
		// A repeat for the same candidate keeps its earlier grant.
		if !bytes.Equal([]byte(existing), encoded) {
			return reservation, fmt.Errorf("%w: candidate %s/%s already reserved a different experiment", store.ErrDuplicateMismatch,
				record.Namespace, record.CandidateRun)
		}
		reservation.Granted = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return reservation, err
	}
	if err := executor.QueryRowContext(ctx, `SELECT COUNT(*) FROM eval_experiments WHERE baseline_run_uid = ?`,
		record.BaselineRunUID).Scan(&reservation.BaselineAttempts); err != nil {
		return reservation, err
	}
	for _, key := range record.CaseKeys {
		var n int32
		if err := executor.QueryRowContext(ctx, `SELECT COUNT(*) FROM eval_experiment_cases WHERE case_key = ?`, key).Scan(&n); err != nil {
			return reservation, err
		}
		reservation.CaseAttempts = max(reservation.CaseAttempts, n)
	}
	if reservation.Granted {
		return reservation, nil
	}
	if reservation.BaselineAttempts >= limits.MaxPerBaseline || reservation.CaseAttempts >= limits.MaxPerCase {
		return reservation, nil
	}
	if _, err := executor.ExecContext(ctx,
		`INSERT INTO eval_experiments (namespace, candidate_run_uid, baseline_run_uid, record_json, reserved_at)
		 VALUES (?, ?, ?, ?, ?)`,
		record.Namespace, record.CandidateRunUID, record.BaselineRunUID, string(encoded), record.ReservedAt); err != nil {
		return reservation, err
	}
	for _, key := range record.CaseKeys {
		if _, err := executor.ExecContext(ctx,
			`INSERT INTO eval_experiment_cases (case_key, namespace, candidate_run_uid) VALUES (?, ?, ?)`,
			key, record.Namespace, record.CandidateRunUID); err != nil {
			return reservation, err
		}
	}
	reservation.Granted = true
	reservation.BaselineAttempts++
	reservation.CaseAttempts++
	return reservation, nil
}

// GetEvalExperiment returns the reservation a candidate incarnation holds.
func (s *Store) GetEvalExperiment(ctx context.Context, namespace, candidateRunUID string) (*store.EvalExperimentRecord, error) {
	var encoded string
	err := s.taskDataExecutor(ctx).QueryRowContext(ctx,
		`SELECT record_json FROM eval_experiments WHERE namespace = ? AND candidate_run_uid = ?`,
		namespace, candidateRunUID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var record store.EvalExperimentRecord
	if err := json.Unmarshal([]byte(encoded), &record); err != nil {
		return nil, fmt.Errorf("decode eval experiment record: %w", err)
	}
	return &record, nil
}
