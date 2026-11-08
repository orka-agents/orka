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

func evalExperimentSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS eval_experiments (
			namespace         TEXT NOT NULL,
			candidate_run_uid TEXT NOT NULL,
			baseline_run_uid  TEXT NOT NULL,
			case_set_digest   TEXT NOT NULL,
			record_json       TEXT NOT NULL,
			reserved_at       TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, candidate_run_uid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_eval_experiments_baseline ON eval_experiments(baseline_run_uid)`,
		`CREATE INDEX IF NOT EXISTS idx_eval_experiments_case_set ON eval_experiments(case_set_digest)`,
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
	if record.ReservedAt.IsZero() {
		return store.ValidationErrorf("eval experiment reservedAt is required")
	}
	if limits.MaxPerBaseline < 1 || limits.MaxPerCaseSet < 1 {
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
		return s.reserveEvalExperimentWrite(ctx, record, limits, encoded)
	})
}

func (s *Store) reserveEvalExperimentWrite(ctx context.Context, record *store.EvalExperimentRecord, limits store.EvalExperimentLimits, encoded []byte) (store.EvalExperimentReservation, error) {
	executor := s.taskDataExecutor(ctx)
	var reservation store.EvalExperimentReservation
	count := func(column, value string) (int32, error) {
		var n int32
		// The column is one of two fixed names, never caller input.
		err := executor.QueryRowContext(ctx, `SELECT COUNT(*) FROM eval_experiments WHERE `+column+` = ?`, value).Scan(&n)
		return n, err
	}
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
	if reservation.BaselineAttempts, err = count("baseline_run_uid", record.BaselineRunUID); err != nil {
		return reservation, err
	}
	if reservation.CaseSetAttempts, err = count("case_set_digest", record.CaseSetDigest); err != nil {
		return reservation, err
	}
	if reservation.Granted {
		return reservation, nil
	}
	if reservation.BaselineAttempts >= limits.MaxPerBaseline || reservation.CaseSetAttempts >= limits.MaxPerCaseSet {
		return reservation, nil
	}
	if _, err := executor.ExecContext(ctx,
		`INSERT INTO eval_experiments (namespace, candidate_run_uid, baseline_run_uid, case_set_digest, record_json, reserved_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		record.Namespace, record.CandidateRunUID, record.BaselineRunUID, record.CaseSetDigest, string(encoded),
		record.ReservedAt.UTC()); err != nil {
		return reservation, err
	}
	reservation.Granted = true
	reservation.BaselineAttempts++
	reservation.CaseSetAttempts++
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
