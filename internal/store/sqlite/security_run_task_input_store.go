package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

var _ store.SecurityRunTaskInputStore = (*Store)(nil)

const atomicScanRunPhasePending = "pending"

func normalizeSecurityRunTaskInput(input *store.SecurityRunTaskInput) error {
	if input == nil {
		return store.ValidationErrorf("security run task input is required")
	}
	for field, value := range map[string]string{
		"runUID":         input.RunUID,
		"namespace":      input.Namespace,
		"repositoryScan": input.RepositoryScan,
		"scanRunID":      input.ScanRunID,
		"stage":          input.Stage,
	} {
		if err := validateSecurityID(value, field, true); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(input.RunUID, true); err != nil {
		return err
	}
	if input.SourceVersion < 0 {
		return store.ValidationErrorf("security run task input sourceVersion must be non-negative")
	}

	submittedContent := input.Content
	if !utf8.ValidString(submittedContent) {
		return store.ValidationErrorf("security run task input content must be valid UTF-8")
	}
	if len(submittedContent) > maxSecurityPayloadBytes {
		return store.ValidationErrorf("security run task input content exceeds %d bytes", maxSecurityPayloadBytes)
	}
	if strings.TrimSpace(input.ContentDigest) != "" {
		normalized, err := normalizeSecurityDigest(input.ContentDigest, true, "contentDigest")
		if err != nil {
			return err
		}
		if normalized != securityDigestBytes([]byte(submittedContent)) {
			return store.ValidationErrorf("security run task input content digest mismatch")
		}
	}

	input.Content = security.NormalizeTaskInputSnapshot(submittedContent)
	if len(input.Content) > maxSecurityPayloadBytes {
		return store.ValidationErrorf("security run task input content exceeds %d bytes after normalization", maxSecurityPayloadBytes)
	}
	input.ContentDigest = securityDigestBytes([]byte(input.Content))
	input.CreatedAt = normalizeSecurityTime(input.CreatedAt)
	return nil
}

func securityRunTaskInputRecordDigest(input store.SecurityRunTaskInput) (string, error) {
	input.RecordDigest = ""
	input.CreatedAt = time.Time{}
	return securityRecordDigest(input)
}

func prepareSecurityRunTaskInput(input *store.SecurityRunTaskInput) error {
	if err := normalizeSecurityRunTaskInput(input); err != nil {
		return err
	}
	digest, err := securityRunTaskInputRecordDigest(*input)
	if err != nil {
		return err
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	input.RecordDigest = digest
	return nil
}

// CreateScanRunWithTaskInput atomically reserves a ScanRun and its immutable initial Task input.
func (s *Store) CreateScanRunWithTaskInput(
	ctx context.Context,
	run *store.ScanRun,
	input *store.SecurityRunTaskInput,
) error {
	if err := normalizeScanRunIntegrityFields(run); err != nil {
		return err
	}
	if strings.TrimSpace(run.RequestIdempotencyKey) == "" {
		return store.ValidationErrorf("requestIdempotencyKey is required for atomic scan run reservation")
	}
	if strings.TrimSpace(run.RepositoryScanUID) == "" || run.RepositoryScanGeneration <= 0 {
		return store.ValidationErrorf("repository scan UID and positive generation are required for atomic scan run reservation")
	}
	if run.Phase != atomicScanRunPhasePending {
		return store.ValidationErrorf("atomic scan run reservation requires pending phase")
	}
	if run.Quality.BundleStatus != store.BundleStatusNotStarted {
		return store.ValidationErrorf("atomic scan run reservation requires initial bundle status %q", store.BundleStatusNotStarted)
	}
	if err := prepareSecurityRunTaskInput(input); err != nil {
		return err
	}
	if input.RunUID != run.RunUID || input.Namespace != run.Namespace || input.RepositoryScan != run.RepositoryScan ||
		input.ScanRunID != run.ID {
		return store.ValidationErrorf("security run task input binding does not match scan run")
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	} else {
		run.StartedAt = run.StartedAt.UTC()
	}
	run.CompletedAt = normalizeSecurityTimePtr(run.CompletedAt)
	reasonCodesJSON, err := marshalSecurityJSON(run.Quality.ReasonCodes)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO security_scan_runs
		 (id, run_uid, namespace, repository_scan, repository_scan_uid, repository_scan_generation,
		  task_name, mode, phase, base_commit, head_commit, commit_count, slice_count, reviewed_slice_count,
		  skipped_slice_count, accepted_findings, dropped_findings, scanner_policy_version, policy_digest,
		  idempotency_key, request_idempotency_key, resolved_target_key, target_receipt_id,
		  quality_schema_version, inventory_coverage_status, candidate_coverage_status, coverage_status,
		  validation_scope, validation_execution, attack_path_execution, analysis_attestation_level,
		  target_verification, bundle_status, authorization_status, isolation_status, quality_reason_codes_json,
		  summary, error_message, started_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.RunUID, run.Namespace, run.RepositoryScan, run.RepositoryScanUID, run.RepositoryScanGeneration,
		run.TaskName, run.Mode, run.Phase, run.BaseCommit, run.HeadCommit, run.CommitCount, run.SliceCount,
		run.ReviewedSliceCount, run.SkippedSliceCount, run.AcceptedFindings, run.DroppedFindings,
		run.ScannerPolicyVersion, run.PolicyDigest, run.IdempotencyKey, run.RequestIdempotencyKey,
		run.ResolvedTargetKey, run.TargetReceiptID, run.Quality.SchemaVersion, run.Quality.InventoryCoverageStatus,
		run.Quality.CandidateCoverageStatus, run.Quality.CoverageStatus, run.Quality.ValidationScope,
		run.Quality.ValidationExecution, run.Quality.AttackPathExecution, run.Quality.AnalysisAttestationLevel,
		run.Quality.TargetVerification, run.Quality.BundleStatus, run.Quality.AuthorizationStatus,
		run.Quality.IsolationStatus, reasonCodesJSON, run.Summary, run.ErrorMessage, run.StartedAt, run.CompletedAt,
	)
	if isSQLiteConstraintError(err) {
		return fmt.Errorf("%w: active scan request already exists", store.ErrConflict)
	}
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO security_run_task_inputs
		(run_uid, stage, namespace, repository_scan, scan_run_id, source_version, content, content_digest, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.RunUID, input.Stage, input.Namespace, input.RepositoryScan, input.ScanRunID,
		input.SourceVersion, input.Content, input.ContentDigest, input.RecordDigest, input.CreatedAt)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		var existingDigest string
		if err := tx.QueryRowContext(ctx, `SELECT record_digest FROM security_run_task_inputs
			WHERE run_uid = ? AND stage = ?`, input.RunUID, input.Stage).Scan(&existingDigest); err != nil {
			return err
		}
		if _, err := immutableReplayResult(existingDigest, input.RecordDigest, "security run task input", input.RunUID+"/"+input.Stage); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveSecurityRunTaskInput stores one immutable initial Task input for a run stage.
func (s *Store) SaveSecurityRunTaskInput(ctx context.Context, input *store.SecurityRunTaskInput) (bool, error) {
	if err := prepareSecurityRunTaskInput(input); err != nil {
		return false, err
	}
	digest := input.RecordDigest

	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO security_run_task_inputs
		(run_uid, stage, namespace, repository_scan, scan_run_id, source_version, content, content_digest, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.RunUID, input.Stage, input.Namespace, input.RepositoryScan, input.ScanRunID,
		input.SourceVersion, input.Content, input.ContentDigest, input.RecordDigest, input.CreatedAt)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected > 0 {
		return true, nil
	}

	var existingDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT record_digest FROM security_run_task_inputs
		WHERE run_uid = ? AND stage = ?`, input.RunUID, input.Stage).Scan(&existingDigest); err != nil {
		return false, err
	}
	return immutableReplayResult(existingDigest, digest, "security run task input", input.RunUID+"/"+input.Stage)
}

// GetSecurityRunTaskInput returns one immutable initial Task input for a run stage.
func (s *Store) GetSecurityRunTaskInput(
	ctx context.Context,
	namespace, runUID, stage string,
) (*store.SecurityRunTaskInput, error) {
	for field, value := range map[string]string{
		"namespace": namespace,
		"runUID":    runUID,
		"stage":     stage,
	} {
		if err := validateSecurityID(value, field, true); err != nil {
			return nil, err
		}
	}
	if err := validateSecurityRunUID(runUID, true); err != nil {
		return nil, err
	}

	var input store.SecurityRunTaskInput
	err := s.db.QueryRowContext(ctx, `SELECT run_uid, namespace, repository_scan, scan_run_id, stage,
		source_version, content, content_digest, record_digest, created_at
		FROM security_run_task_inputs WHERE namespace = ? AND run_uid = ? AND stage = ?`,
		namespace, runUID, stage).Scan(
		&input.RunUID, &input.Namespace, &input.RepositoryScan, &input.ScanRunID, &input.Stage,
		&input.SourceVersion, &input.Content, &input.ContentDigest, &input.RecordDigest, &input.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return &input, err
}
