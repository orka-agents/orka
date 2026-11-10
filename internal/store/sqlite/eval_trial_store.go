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

// maxEvalTrialsPerRun matches the EvalRun cap on cases × trials.
const maxEvalTrialsPerRun = 500

func evalTrialSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS eval_trials (
			namespace    TEXT NOT NULL,
			eval_run     TEXT NOT NULL,
			eval_run_uid TEXT NOT NULL,
			case_id      TEXT NOT NULL,
			trial        INTEGER NOT NULL CHECK(trial >= 1),
			outcome      TEXT NOT NULL CHECK(outcome IN ('pass', 'fail', 'infraError')),
			record_json  TEXT NOT NULL,
			created_at   TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, eval_run, eval_run_uid, case_id, trial)
		)`,
	}
}

func validateEvalTrialRecord(record *store.EvalTrialRecord) error {
	if record == nil {
		return store.ValidationErrorf("eval trial record is required")
	}
	if record.SchemaVersion != store.EvalTrialSchemaVersion {
		return store.ValidationErrorf("eval trial schemaVersion must be %q", store.EvalTrialSchemaVersion)
	}
	for field, value := range map[string]string{
		"namespace": record.Namespace, "evalRun": record.EvalRun, "evalRunUID": record.EvalRunUID,
		"caseID": record.CaseID, "taskName": record.TaskName,
	} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return store.ValidationErrorf("eval trial %s is required and must not have surrounding whitespace", field)
		}
	}
	if record.Trial < 1 {
		return store.ValidationErrorf("eval trial number must be at least 1")
	}
	switch record.Outcome {
	case store.EvalTrialOutcomePass, store.EvalTrialOutcomeFail:
		if record.InfraClass != "" {
			return store.ValidationErrorf("eval trial infraClass is only allowed for infraError outcomes")
		}
	case store.EvalTrialOutcomeInfraError:
		if record.InfraClass == "" {
			return store.ValidationErrorf("eval trial infraError outcome requires infraClass")
		}
	default:
		return store.ValidationErrorf("eval trial outcome %q is not supported", record.Outcome)
	}
	if record.CompletedAt.IsZero() {
		return store.ValidationErrorf("eval trial completedAt is required")
	}
	return nil
}

// CreateEvalTrial records one completed trial. It is a contract write: with a
// write envelope it runs under the operation receipt and controller epoch
// fence like every other state write.
func (s *Store) CreateEvalTrial(ctx context.Context, record *store.EvalTrialRecord) error {
	if err := validateEvalTrialRecord(record); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode eval trial record: %w", err)
	}
	if len(encoded) > store.MaxEvalTrialRecordBytes {
		return store.ValidationErrorf("eval trial record exceeds %d bytes", store.MaxEvalTrialRecordBytes)
	}
	_, err = writeOperation(ctx, s, "CreateEvalTrial", nil, func(ctx context.Context) (noResult, error) {
		return noResult{}, s.createEvalTrialWrite(ctx, record, encoded)
	})
	return err
}

func (s *Store) createEvalTrialWrite(ctx context.Context, record *store.EvalTrialRecord, encoded []byte) error {
	executor := s.taskDataExecutor(ctx)
	result, err := executor.ExecContext(ctx,
		`INSERT INTO eval_trials (namespace, eval_run, eval_run_uid, case_id, trial, outcome, record_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, eval_run, eval_run_uid, case_id, trial) DO NOTHING`,
		record.Namespace, record.EvalRun, record.EvalRunUID, record.CaseID, record.Trial, record.Outcome,
		string(encoded), record.CompletedAt.UTC(),
	)
	if err != nil {
		return err
	}
	if inserted, err := result.RowsAffected(); err != nil || inserted == 1 {
		return err
	}
	// The trial is already recorded: an identical repeat succeeds.
	var existing string
	err = executor.QueryRowContext(ctx,
		`SELECT record_json FROM eval_trials
		 WHERE namespace = ? AND eval_run = ? AND eval_run_uid = ? AND case_id = ? AND trial = ?`,
		record.Namespace, record.EvalRun, record.EvalRunUID, record.CaseID, record.Trial,
	).Scan(&existing)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: eval trial record changed concurrently", store.ErrConflict)
		}
		return err
	}
	if !bytes.Equal([]byte(existing), encoded) {
		return fmt.Errorf("%w: eval trial %s/%s case %q trial %d is already recorded", store.ErrDuplicateMismatch,
			record.Namespace, record.EvalRun, record.CaseID, record.Trial)
	}
	return nil
}

// ListEvalTrials returns one EvalRun incarnation's records by case and trial.
func (s *Store) ListEvalTrials(ctx context.Context, namespace, evalRun, evalRunUID string) ([]store.EvalTrialRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT record_json FROM eval_trials
		 WHERE namespace = ? AND eval_run = ? AND eval_run_uid = ?
		 ORDER BY case_id, trial
		 LIMIT ?`,
		namespace, evalRun, evalRunUID, maxEvalTrialsPerRun+1,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []store.EvalTrialRecord
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var record store.EvalTrialRecord
		if err := json.Unmarshal([]byte(encoded), &record); err != nil {
			return nil, fmt.Errorf("decode eval trial record: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(records) > maxEvalTrialsPerRun {
		return nil, fmt.Errorf("eval run %s/%s has more than %d trial records", namespace, evalRun, maxEvalTrialsPerRun)
	}
	return records, nil
}

// DeleteEvalTrials deletes the records of every incarnation of an EvalRun.
func (s *Store) DeleteEvalTrials(ctx context.Context, namespace, evalRun string) error {
	return writeOperationErr(ctx, s, "DeleteEvalTrials", func(ctx context.Context) error {
		_, err := s.taskDataExecutor(ctx).ExecContext(ctx,
			`DELETE FROM eval_trials WHERE namespace = ? AND eval_run = ?`, namespace, evalRun)
		return err
	})
}
