package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	verification "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/store"
)

// Admission and listing share a bound so long-running submissions cannot hide
// newer work behind a permanently full oldest-first reconciliation batch.
const maxActiveKubernetesValidationSubmissions = 1000

func kubernetesValidationTableStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS validation_submissions (
				namespace TEXT NOT NULL, request_id TEXT NOT NULL, submitted_by TEXT NOT NULL,
				attempt_id TEXT NOT NULL, original_task_name TEXT NOT NULL, patched_task_name TEXT NOT NULL,
				original_task_uid TEXT NOT NULL DEFAULT '', patched_task_uid TEXT NOT NULL DEFAULT '',
				manifest BLOB NOT NULL CHECK(length(manifest) <= 1048576),
				provenance BLOB NOT NULL CHECK(length(provenance) <= 134217728),
				binding BLOB, run_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
				failure TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
				PRIMARY KEY(namespace, request_id), UNIQUE(namespace, original_task_name))`,
		`CREATE TABLE IF NOT EXISTS validation_dispatches (
				run_id TEXT NOT NULL, side TEXT NOT NULL, check_id TEXT NOT NULL,
				job_name TEXT NOT NULL, spec_digest TEXT NOT NULL, job_uid TEXT NOT NULL DEFAULT '',
				pod_uid TEXT NOT NULL DEFAULT '', container_id TEXT NOT NULL DEFAULT '',
				state TEXT NOT NULL, failure TEXT NOT NULL DEFAULT '',
				PRIMARY KEY(run_id, side, check_id), UNIQUE(job_name))`,
	}
}

func kubernetesValidationIndexStatements() []string {
	return []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS validation_submissions_patched_task
			ON validation_submissions(namespace, patched_task_name) WHERE patched_task_name <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS validation_submissions_run
			ON validation_submissions(run_id) WHERE run_id <> ''`,
		`CREATE INDEX IF NOT EXISTS validation_submissions_active
			ON validation_submissions(namespace, state, created_at, request_id)`,
	}
}

func (storage *Store) InitializeKubernetesValidationStore(ctx context.Context) error {
	if err := storage.InitializePatchVerificationStore(ctx); err != nil {
		return err
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		for _, statement := range kubernetesValidationTableStatements() {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if err := migrateKubernetesValidationSubmissions(ctx, tx); err != nil {
			return err
		}
		for _, statement := range kubernetesValidationIndexStatements() {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

func migrateKubernetesValidationSubmissions(ctx context.Context, tx *sql.Tx) error {
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='validation_submissions'`).Scan(&definition); err != nil {
		return err
	}
	oldConstraint := regexp.MustCompile(`(?i),\s*UNIQUE\s*\(\s*namespace\s*,\s*patched_task_name\s*\)`)
	if !oldConstraint.MatchString(definition) {
		return nil
	}
	// SQLite cannot drop a table-level UNIQUE constraint. Rebuild transactionally
	// from the existing definition, preserving every other constraint and index.
	definition = oldConstraint.ReplaceAllString(definition, "")
	rows, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_master
		WHERE tbl_name='validation_submissions' AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name`)
	if err != nil {
		return err
	}
	var dependentSQL []string
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			_ = rows.Close()
			return err
		}
		dependentSQL = append(dependentSQL, statement)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE TABLE validation_submissions_migration ` + definition[strings.IndexByte(definition, '('):],
		`INSERT INTO validation_submissions_migration SELECT * FROM validation_submissions`,
		`DROP TABLE validation_submissions`,
		`ALTER TABLE validation_submissions_migration RENAME TO validation_submissions`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, statement := range dependentSQL {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func validationJSON(value any, limit int) ([]byte, error) {
	content, err := json.Marshal(value)
	if err != nil || len(content) > limit {
		return nil, verification.ErrLimit
	}
	return content, nil
}

func (storage *Store) createKubernetesValidationSubmissionWrite(ctx context.Context, submission verification.KubernetesSubmission) error {
	manifest, err := validationJSON(submission.Manifest, verification.MaxManifestBytes)
	if err != nil {
		return err
	}
	provenance, err := validationJSON(submission.Provenance, verification.MaxRunBlobBytes)
	if err != nil {
		return err
	}
	if strings.TrimSpace(submission.Namespace) == "" || submission.RequestID == "" || submission.SubmittedBy == "" ||
		submission.AttemptID == "" || submission.OriginalTaskName == "" || submission.State != verification.SubmissionPreparing ||
		verification.ValidateManifest(submission.Manifest) != nil ||
		!kubernetesValidationTaskNamesValid(&submission) ||
		submission.OriginalTaskUID != "" || submission.PatchedTaskUID != "" || submission.Binding != nil ||
		submission.RunID != "" || submission.Failure != "" {
		return verification.ErrEvidence
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
			SELECT 1 FROM validation_submissions WHERE namespace=? AND state IN (?,?,?) LIMIT ?)`,
			submission.Namespace, verification.SubmissionPreparing, verification.SubmissionRunning,
			verification.SubmissionCancelling, maxActiveKubernetesValidationSubmissions).Scan(&active); err != nil {
			return err
		}
		if active >= maxActiveKubernetesValidationSubmissions {
			return verification.ErrLimit
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO validation_submissions
			(namespace,request_id,submitted_by,attempt_id,original_task_name,patched_task_name,manifest,provenance,state,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, submission.Namespace, submission.RequestID, submission.SubmittedBy,
			submission.AttemptID, submission.OriginalTaskName, submission.PatchedTaskName, manifest, provenance,
			submission.State, now, now)
		if err != nil {
			return fmt.Errorf("create validation submission: %w", err)
		}
		return rowsAffectedExactlyOne(result, "validation submission creation")
	})
}

func scanKubernetesValidation(row interface{ Scan(...any) error }) (*verification.KubernetesSubmission, error) {
	var submission verification.KubernetesSubmission
	var manifest, provenance, binding []byte
	var created, updated string
	err := row.Scan(&submission.Namespace, &submission.RequestID, &submission.SubmittedBy, &submission.AttemptID,
		&submission.OriginalTaskName, &submission.PatchedTaskName, &submission.OriginalTaskUID, &submission.PatchedTaskUID,
		&manifest, &provenance, &binding, &submission.RunID, &submission.State, &submission.Failure, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, verification.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(manifest, &submission.Manifest) != nil || json.Unmarshal(provenance, &submission.Provenance) != nil {
		return nil, verification.ErrIntegrity
	}
	if len(binding) > 0 {
		submission.Binding = &verification.Binding{}
		if json.Unmarshal(binding, submission.Binding) != nil {
			return nil, verification.ErrIntegrity
		}
	}
	if submission.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, verification.ErrIntegrity
	}
	if submission.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return nil, verification.ErrIntegrity
	}
	return &submission, nil
}

const kubernetesValidationColumns = `namespace,request_id,submitted_by,attempt_id,original_task_name,patched_task_name,
	original_task_uid,patched_task_uid,manifest,provenance,binding,run_id,state,failure,created_at,updated_at`

func (storage *Store) GetKubernetesValidationSubmission(ctx context.Context, namespace, requestID string) (*verification.KubernetesSubmission, error) {
	if strings.TrimSpace(namespace) == "" || requestID == "" {
		return nil, verification.ErrBinding
	}
	return scanKubernetesValidation(storage.taskDataExecutor(ctx).QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
		` FROM validation_submissions WHERE namespace=? AND request_id=?`, namespace, requestID))
}

func (storage *Store) GetKubernetesValidationSubmissionByTask(ctx context.Context, namespace, taskName string) (*verification.KubernetesSubmission, error) {
	if strings.TrimSpace(namespace) == "" || taskName == "" {
		return nil, verification.ErrBinding
	}
	return scanKubernetesValidation(storage.taskDataExecutor(ctx).QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
		` FROM validation_submissions WHERE namespace=? AND (original_task_name=? OR patched_task_name=?)`, namespace, taskName, taskName))
}

func (storage *Store) GetKubernetesValidationSubmissionByRun(ctx context.Context, namespace, runID string) (*verification.KubernetesSubmission, error) {
	if strings.TrimSpace(namespace) == "" || runID == "" {
		return nil, verification.ErrBinding
	}
	return scanKubernetesValidation(storage.taskDataExecutor(ctx).QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
		` FROM validation_submissions WHERE namespace=? AND run_id=?`, namespace, runID))
}

// ListActiveKubernetesValidationSubmissions returns only Namespace, RequestID,
// State, and CreatedAt. Manifest, provenance, binding, and other fields are omitted
// to keep reconciliation sweeps bounded independently of payload size. Callers
// must reload each entry with GetKubernetesValidationSubmission before processing.
func (storage *Store) ListActiveKubernetesValidationSubmissions(ctx context.Context, namespace string) ([]verification.KubernetesSubmission, error) {
	if strings.TrimSpace(namespace) == "" {
		return nil, verification.ErrBinding
	}
	rows, err := storage.taskDataExecutor(ctx).QueryContext(ctx, `SELECT namespace,request_id,state,created_at FROM validation_submissions
		WHERE namespace=? AND state IN (?,?,?) ORDER BY created_at,request_id LIMIT ?`, namespace,
		verification.SubmissionPreparing, verification.SubmissionRunning, verification.SubmissionCancelling,
		maxActiveKubernetesValidationSubmissions+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	submissions := make([]verification.KubernetesSubmission, 0)
	for rows.Next() {
		if len(submissions) == maxActiveKubernetesValidationSubmissions {
			// Pre-cap POC databases may already exceed admission. Never silently
			// omit their newest submissions from every reconciliation pass.
			return nil, verification.ErrLimit
		}
		var submission verification.KubernetesSubmission
		var created string
		if err := rows.Scan(&submission.Namespace, &submission.RequestID, &submission.State, &created); err != nil {
			return nil, err
		}
		if submission.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, verification.ErrIntegrity
		}
		submissions = append(submissions, submission)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RFC3339Nano has variable precision, so lexical SQL ordering alone is not
	// chronological for timestamps with and without fractional seconds.
	sort.Slice(submissions, func(left, right int) bool {
		if submissions[left].CreatedAt.Equal(submissions[right].CreatedAt) {
			return submissions[left].RequestID < submissions[right].RequestID
		}
		return submissions[left].CreatedAt.Before(submissions[right].CreatedAt)
	})
	return submissions, nil
}

func kubernetesValidationTaskNamesValid(submission *verification.KubernetesSubmission) bool {
	if submission.OriginalTaskName == "" {
		return false
	}
	if submission.Manifest.Action == verification.ValidateReport {
		return submission.PatchedTaskName == ""
	}
	return submission.PatchedTaskName != "" && submission.PatchedTaskName != submission.OriginalTaskName
}

func validateKubernetesValidationBinding(submission *verification.KubernetesSubmission, binding verification.Binding) error {
	if !kubernetesValidationTaskNamesValid(submission) || verification.ValidateManifest(submission.Manifest) != nil ||
		verification.ValidateRunBinding(submission.Manifest, binding) != nil ||
		binding.AttemptID != submission.AttemptID ||
		binding.OriginalTaskID != submission.OriginalTaskUID || binding.PatchedTaskID != submission.PatchedTaskUID {
		return verification.ErrBinding
	}
	return nil
}

func (storage *Store) bindKubernetesValidationTasksWrite(ctx context.Context, namespace, requestID, originalUID, patchedUID string, binding verification.Binding) error {
	if strings.TrimSpace(namespace) == "" || requestID == "" || originalUID == "" ||
		binding.OriginalTaskID != originalUID || binding.PatchedTaskID != patchedUID {
		return verification.ErrBinding
	}
	content, err := validationJSON(binding, 4096)
	if err != nil {
		return err
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, err := scanKubernetesValidation(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
			` FROM validation_submissions WHERE namespace=? AND request_id=?`, namespace, requestID))
		if err != nil {
			return err
		}
		if err := validateKubernetesValidationBinding(submission, binding); err != nil {
			return err
		}
		record, err := requirePatchVerificationBinding(ctx, tx, binding)
		if err != nil {
			return err
		}
		if submission.Binding != nil || submission.RunID != "" {
			if submission.Binding != nil && *submission.Binding == binding && submission.RunID == binding.RunID {
				switch submission.State {
				case verification.SubmissionRunning, verification.SubmissionCancelling, verification.SubmissionTerminal:
					return nil
				default:
					return verification.ErrIntegrity
				}
			}
			return verification.ErrConflict
		}
		if submission.State != verification.SubmissionPreparing || record.State != verification.RunRunning {
			return verification.ErrClosed
		}
		var claimed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_submissions WHERE run_id=?`, binding.RunID).Scan(&claimed); err != nil {
			return err
		}
		if claimed != 0 {
			return verification.ErrBinding
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_submissions SET binding=?,run_id=?,state=?,updated_at=?
			WHERE namespace=? AND request_id=? AND state=?`, content, binding.RunID, verification.SubmissionRunning,
			time.Now().UTC().Format(time.RFC3339Nano), namespace, requestID, verification.SubmissionPreparing)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation submission binding")
	})
}

// AttachCancelledKubernetesValidationRun links a stopped preparatory run for audit.
// It only accepts cancelling submissions and never changes their execution state.
func (storage *Store) attachCancelledKubernetesValidationRunWrite(ctx context.Context, namespace, requestID string, binding verification.Binding) error {
	if strings.TrimSpace(namespace) == "" || requestID == "" {
		return verification.ErrBinding
	}
	content, err := validationJSON(binding, 4096)
	if err != nil {
		return err
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, err := scanKubernetesValidation(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
			` FROM validation_submissions WHERE namespace=? AND request_id=?`, namespace, requestID))
		if err != nil {
			return err
		}
		if submission.State != verification.SubmissionCancelling {
			return verification.ErrClosed
		}
		if err := validateKubernetesValidationBinding(submission, binding); err != nil {
			return err
		}
		record, err := requirePatchVerificationBinding(ctx, tx, binding)
		if err != nil {
			return err
		}
		if record.State != verification.RunCancelled && record.State != verification.RunInterrupted {
			return verification.ErrClosed
		}
		if submission.Binding != nil || submission.RunID != "" {
			if submission.Binding != nil && *submission.Binding == binding && submission.RunID == binding.RunID {
				return nil
			}
			return verification.ErrConflict
		}
		var claimed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_submissions WHERE run_id=?`, binding.RunID).Scan(&claimed); err != nil {
			return err
		}
		if claimed != 0 {
			return verification.ErrBinding
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_submissions SET binding=?,run_id=?,updated_at=?
			WHERE namespace=? AND request_id=? AND state=?`, content, binding.RunID,
			time.Now().UTC().Format(time.RFC3339Nano), namespace, requestID, verification.SubmissionCancelling)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "cancelled validation audit binding")
	})
}

func (storage *Store) recordKubernetesValidationTaskUIDWrite(ctx context.Context, namespace, requestID, side, taskUID string) error {
	column := "original_task_uid"
	if side == verification.Patched {
		column = "patched_task_uid"
	} else if side != verification.Original {
		return verification.ErrEvidence
	}
	if strings.TrimSpace(namespace) == "" || requestID == "" || taskUID == "" {
		return verification.ErrBinding
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, err := scanKubernetesValidation(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
			` FROM validation_submissions WHERE namespace=? AND request_id=?`, namespace, requestID))
		if err != nil {
			return err
		}
		taskName, previousUID, otherUID := submission.OriginalTaskName, submission.OriginalTaskUID, submission.PatchedTaskUID
		if side == verification.Patched {
			taskName, previousUID, otherUID = submission.PatchedTaskName, submission.PatchedTaskUID, submission.OriginalTaskUID
		}
		if taskName == "" || taskUID == otherUID {
			return verification.ErrBinding
		}
		if previousUID != "" {
			if previousUID == taskUID {
				return nil
			}
			return verification.ErrConflict
		}
		// Cancellation may race Task creation before its UID is journaled.
		// Retain that identity for fenced cleanup without reopening execution.
		if submission.State != verification.SubmissionPreparing && submission.State != verification.SubmissionCancelling {
			return verification.ErrClosed
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_submissions SET `+column+`=?,updated_at=?
			WHERE namespace=? AND request_id=? AND state IN (?,?) AND `+column+`=''`, taskUID,
			time.Now().UTC().Format(time.RFC3339Nano), namespace, requestID,
			verification.SubmissionPreparing, verification.SubmissionCancelling)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation task identity")
	})
}

func (storage *Store) requestKubernetesValidationCancellationWrite(ctx context.Context, namespace, requestID string) error {
	if strings.TrimSpace(namespace) == "" || requestID == "" {
		return verification.ErrBinding
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		state, err := kubernetesValidationSubmissionState(ctx, tx, namespace, requestID)
		if err != nil {
			return err
		}
		switch state {
		case verification.SubmissionTerminal:
			return nil
		case verification.SubmissionPreparing, verification.SubmissionRunning, verification.SubmissionCancelling:
		default:
			return verification.ErrIntegrity
		}
		submission, err := scanKubernetesValidation(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
			` FROM validation_submissions WHERE namespace=? AND request_id=?`, namespace, requestID))
		if err != nil {
			return err
		}
		if submission.Binding != nil || submission.RunID != "" {
			if submission.Binding == nil || submission.Binding.RunID != submission.RunID ||
				validateKubernetesValidationBinding(submission, *submission.Binding) != nil {
				return verification.ErrBinding
			}
			record, err := requirePatchVerificationBinding(ctx, tx, *submission.Binding)
			if err != nil {
				return err
			}
			// Fence finalization under the same write lock as the submission
			// marker; polling the marker later cannot prevent a favorable seal.
			if record.State == verification.RunRunning {
				reason := patchVerificationTerminalAssessment(record, verification.RunCancelled).Reason
				if err := patchVerificationIncident(ctx, tx, record, string(verification.RunCancelled),
					"lifecycle", "", reason, verification.RunCancelled); err != nil {
					return err
				}
			}
		}
		if state == verification.SubmissionCancelling {
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_submissions SET state=?,updated_at=?
			WHERE namespace=? AND request_id=? AND state IN (?,?)`, verification.SubmissionCancelling,
			time.Now().UTC().Format(time.RFC3339Nano), namespace, requestID,
			verification.SubmissionPreparing, verification.SubmissionRunning)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation submission cancellation")
	})
}

func kubernetesValidationSubmissionState(ctx context.Context, tx *sql.Tx, namespace, requestID string) (verification.SubmissionState, error) {
	var state verification.SubmissionState
	err := tx.QueryRowContext(ctx, `SELECT state FROM validation_submissions WHERE namespace=? AND request_id=?`, namespace, requestID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", verification.ErrRunNotFound
	}
	return state, err
}

func (storage *Store) completeKubernetesValidationSubmissionWrite(ctx context.Context, namespace, requestID, reason string) error {
	if strings.TrimSpace(namespace) == "" || requestID == "" {
		return verification.ErrBinding
	}
	if !utf8.ValidString(reason) || patchVerificationCredentials.MatchString(reason) {
		return verification.ErrEvidence
	}
	reason = boundedKubernetesValidationFailure(reason)
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		state, err := kubernetesValidationSubmissionState(ctx, tx, namespace, requestID)
		if err != nil {
			return err
		}
		switch state {
		case verification.SubmissionTerminal:
			return nil
		case verification.SubmissionPreparing, verification.SubmissionRunning, verification.SubmissionCancelling:
		default:
			return verification.ErrIntegrity
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_submissions SET state=?,failure=?,updated_at=?
			WHERE namespace=? AND request_id=? AND state IN (?,?,?)`, verification.SubmissionTerminal, reason,
			time.Now().UTC().Format(time.RFC3339Nano), namespace, requestID,
			verification.SubmissionPreparing, verification.SubmissionRunning, verification.SubmissionCancelling)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation submission completion")
	})
}

func boundedKubernetesValidationFailure(reason string) string {
	if len(reason) > 256 {
		reason = reason[:256]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	return reason
}

func (storage *Store) createKubernetesValidationDispatchWrite(ctx context.Context, dispatch verification.KubernetesDispatch) error {
	if dispatch.RunID == "" || dispatch.CheckID == "" || dispatch.JobName == "" ||
		store.ValidateCanonicalDigest("dispatch spec", dispatch.SpecDigest) != nil || dispatch.State != verification.DispatchPlanned ||
		dispatch.JobUID != "" || dispatch.PodUID != "" || dispatch.ContainerID != "" || dispatch.Failure != "" {
		return verification.ErrEvidence
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, record, err := requireKubernetesValidationDispatchBinding(ctx, tx, dispatch.RunID, dispatch.Side, dispatch.CheckID)
		if err != nil {
			return err
		}
		if submission.State != verification.SubmissionRunning || record.State != verification.RunRunning {
			return verification.ErrClosed
		}
		previous, err := scanKubernetesValidationDispatch(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationDispatchColumns+
			` FROM validation_dispatches WHERE run_id=? AND side=? AND check_id=?`, dispatch.RunID, dispatch.Side, dispatch.CheckID))
		if err == nil {
			if previous.JobName == dispatch.JobName && previous.SpecDigest == dispatch.SpecDigest {
				return nil
			}
			return verification.ErrConflict
		}
		if !errors.Is(err, verification.ErrRunNotFound) {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO validation_dispatches(run_id,side,check_id,job_name,spec_digest,state) VALUES(?,?,?,?,?,?)`,
			dispatch.RunID, dispatch.Side, dispatch.CheckID, dispatch.JobName, dispatch.SpecDigest, dispatch.State)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation dispatch planning")
	})
}

func requireKubernetesValidationDispatchBinding(ctx context.Context, tx *sql.Tx, runID, side, checkID string) (*verification.KubernetesSubmission, *verification.Record, error) {
	if runID == "" || checkID == "" || (side != verification.Original && side != verification.Patched) {
		return nil, nil, verification.ErrBinding
	}
	submission, err := scanKubernetesValidation(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationColumns+
		` FROM validation_submissions WHERE run_id=?`, runID))
	if err != nil {
		return nil, nil, err
	}
	if submission.Binding == nil || submission.Binding.RunID != runID ||
		validateKubernetesValidationBinding(submission, *submission.Binding) != nil ||
		(side == verification.Patched && submission.Manifest.Action == verification.ValidateReport) {
		return nil, nil, verification.ErrBinding
	}
	found := false
	for _, check := range submission.Manifest.Checks {
		found = found || check.ID == checkID
	}
	if !found {
		return nil, nil, verification.ErrBinding
	}
	record, err := requirePatchVerificationBinding(ctx, tx, *submission.Binding)
	return submission, record, err
}

const kubernetesValidationDispatchColumns = `run_id,side,check_id,job_name,spec_digest,job_uid,pod_uid,container_id,state,failure`

func scanKubernetesValidationDispatch(row interface{ Scan(...any) error }) (*verification.KubernetesDispatch, error) {
	var dispatch verification.KubernetesDispatch
	err := row.Scan(
		&dispatch.RunID, &dispatch.Side, &dispatch.CheckID, &dispatch.JobName, &dispatch.SpecDigest,
		&dispatch.JobUID, &dispatch.PodUID, &dispatch.ContainerID, &dispatch.State, &dispatch.Failure)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, verification.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return &dispatch, nil
}

func (storage *Store) GetKubernetesValidationDispatch(ctx context.Context, runID, side, checkID string) (*verification.KubernetesDispatch, error) {
	if runID == "" || checkID == "" || (side != verification.Original && side != verification.Patched) {
		return nil, verification.ErrBinding
	}
	return scanKubernetesValidationDispatch(storage.taskDataExecutor(ctx).QueryRowContext(ctx, `SELECT `+kubernetesValidationDispatchColumns+
		` FROM validation_dispatches WHERE run_id=? AND side=? AND check_id=?`, runID, side, checkID))
}

func (storage *Store) markKubernetesValidationDispatchCreatedWrite(ctx context.Context, runID, side, checkID, jobUID string) error {
	if jobUID == "" {
		return verification.ErrBinding
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, record, err := requireKubernetesValidationDispatchBinding(ctx, tx, runID, side, checkID)
		if err != nil {
			return err
		}
		dispatch, err := scanKubernetesValidationDispatch(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationDispatchColumns+
			` FROM validation_dispatches WHERE run_id=? AND side=? AND check_id=?`, runID, side, checkID))
		if err != nil {
			return err
		}
		if dispatch.JobUID != "" {
			if dispatch.JobUID != jobUID {
				return verification.ErrConflict
			}
			switch dispatch.State {
			case verification.DispatchCreated, verification.DispatchObserved, verification.DispatchRecorded, verification.DispatchFailed:
				return nil
			default:
				return verification.ErrIntegrity
			}
		}
		if dispatch.State != verification.DispatchPlanned {
			return verification.ErrConflict
		}
		if submission.State != verification.SubmissionRunning || record.State != verification.RunRunning {
			return verification.ErrClosed
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_dispatches SET job_uid=?,state=? WHERE run_id=? AND side=? AND check_id=? AND state=? AND job_uid=''`,
			jobUID, verification.DispatchCreated, runID, side, checkID, verification.DispatchPlanned)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation dispatch creation")
	})
}

func (storage *Store) markKubernetesValidationDispatchObservedWrite(ctx context.Context, runID, side, checkID, jobUID, podUID, containerID string) error {
	if jobUID == "" || podUID == "" || containerID == "" {
		return verification.ErrBinding
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, record, err := requireKubernetesValidationDispatchBinding(ctx, tx, runID, side, checkID)
		if err != nil {
			return err
		}
		dispatch, err := scanKubernetesValidationDispatch(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationDispatchColumns+
			` FROM validation_dispatches WHERE run_id=? AND side=? AND check_id=?`, runID, side, checkID))
		if err != nil {
			return err
		}
		if dispatch.JobUID != jobUID {
			return verification.ErrConflict
		}
		if dispatch.PodUID != "" || dispatch.ContainerID != "" {
			if dispatch.PodUID != podUID || dispatch.ContainerID != containerID {
				return verification.ErrConflict
			}
			switch dispatch.State {
			case verification.DispatchObserved, verification.DispatchRecorded, verification.DispatchFailed:
				return nil
			default:
				return verification.ErrIntegrity
			}
		}
		if dispatch.State != verification.DispatchCreated {
			return verification.ErrConflict
		}
		if submission.State != verification.SubmissionRunning || record.State != verification.RunRunning {
			return verification.ErrClosed
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_dispatches SET pod_uid=?,container_id=?,state=?
			WHERE run_id=? AND side=? AND check_id=? AND state=? AND job_uid=? AND pod_uid=''`,
			podUID, containerID, verification.DispatchObserved, runID, side, checkID, verification.DispatchCreated, jobUID)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation dispatch observation")
	})
}

func (storage *Store) markKubernetesValidationDispatchRecordedWrite(ctx context.Context, runID, side, checkID, jobUID, podUID string) error {
	if jobUID == "" || podUID == "" {
		return verification.ErrBinding
	}
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, record, err := requireKubernetesValidationDispatchBinding(ctx, tx, runID, side, checkID)
		if err != nil {
			return err
		}
		dispatch, err := scanKubernetesValidationDispatch(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationDispatchColumns+
			` FROM validation_dispatches WHERE run_id=? AND side=? AND check_id=?`, runID, side, checkID))
		if err != nil {
			return err
		}
		if dispatch.JobUID != jobUID || dispatch.PodUID != podUID {
			return verification.ErrConflict
		}
		if dispatch.State == verification.DispatchRecorded {
			return nil
		}
		if dispatch.State != verification.DispatchObserved {
			return verification.ErrConflict
		}
		if submission.State != verification.SubmissionRunning {
			return verification.ErrClosed
		}
		recorded := false
		for _, evidence := range record.Evidence {
			observation := evidence.Observation
			if observation.Side == side && observation.CheckID == checkID &&
				observation.ContainerID == dispatch.ContainerID && evidence.Rejection == "" &&
				patchVerificationObservationIdentity(*submission.Binding, observation) {
				recorded = true
				break
			}
		}
		if !recorded {
			return verification.ErrEvidence
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_dispatches SET state=?
			WHERE run_id=? AND side=? AND check_id=? AND state=? AND job_uid=? AND pod_uid=?`,
			verification.DispatchRecorded, runID, side, checkID, verification.DispatchObserved, jobUID, podUID)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation dispatch recording")
	})
}

func (storage *Store) markKubernetesValidationDispatchFailedWrite(ctx context.Context, runID, side, checkID, reason string) error {
	if !utf8.ValidString(reason) || patchVerificationCredentials.MatchString(reason) {
		return verification.ErrEvidence
	}
	reason = boundedKubernetesValidationFailure(reason)
	return storage.withPatchVerificationTx(ctx, func(tx *sql.Tx) error {
		submission, _, err := requireKubernetesValidationDispatchBinding(ctx, tx, runID, side, checkID)
		if err != nil {
			return err
		}
		dispatch, err := scanKubernetesValidationDispatch(tx.QueryRowContext(ctx, `SELECT `+kubernetesValidationDispatchColumns+
			` FROM validation_dispatches WHERE run_id=? AND side=? AND check_id=?`, runID, side, checkID))
		if err != nil {
			return err
		}
		if dispatch.State == verification.DispatchFailed {
			if dispatch.Failure == reason {
				return nil
			}
			return verification.ErrConflict
		}
		if dispatch.State != verification.DispatchPlanned && dispatch.State != verification.DispatchCreated && dispatch.State != verification.DispatchObserved {
			return verification.ErrConflict
		}
		if submission.State != verification.SubmissionRunning && submission.State != verification.SubmissionCancelling {
			return verification.ErrClosed
		}
		result, err := tx.ExecContext(ctx, `UPDATE validation_dispatches SET state=?,failure=?
			WHERE run_id=? AND side=? AND check_id=? AND state IN (?,?,?)`, verification.DispatchFailed, reason,
			runID, side, checkID, verification.DispatchPlanned, verification.DispatchCreated, verification.DispatchObserved)
		if err != nil {
			return err
		}
		return rowsAffectedExactlyOne(result, "validation dispatch failure")
	})
}
