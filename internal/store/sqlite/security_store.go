package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/store"
)

func parseOffsetCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return offset, nil
}

func nextOffsetCursor(offset, count, limit int) string {
	if limit <= 0 || count < limit {
		return ""
	}
	return strconv.Itoa(offset + count)
}

func shortStoreHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func marshalSecurityJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalSecurityJSON(payload string, value any) error {
	if strings.TrimSpace(payload) == "" {
		payload = "[]"
	}
	return json.Unmarshal([]byte(payload), value)
}

// CreateScanRun inserts a new scan run.
func (s *Store) CreateScanRun(ctx context.Context, run *store.ScanRun) error {
	now := time.Now()
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO security_scan_runs
		 (id, namespace, repository_scan, task_name, mode, phase, base_commit, head_commit, commit_count,
		  slice_count, reviewed_slice_count, skipped_slice_count, accepted_findings, dropped_findings,
		  scanner_policy_version, policy_digest, idempotency_key, summary, error_message, started_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Namespace, run.RepositoryScan, run.TaskName, run.Mode, run.Phase,
		run.BaseCommit, run.HeadCommit, run.CommitCount, run.SliceCount, run.ReviewedSliceCount,
		run.SkippedSliceCount, run.AcceptedFindings, run.DroppedFindings,
		run.ScannerPolicyVersion, run.PolicyDigest, run.IdempotencyKey, run.Summary, run.ErrorMessage,
		run.StartedAt, run.CompletedAt,
	)
	return err
}

// UpdateScanRun updates a scan run.
func (s *Store) UpdateScanRun(ctx context.Context, run *store.ScanRun) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE security_scan_runs
		 SET task_name = ?, mode = ?, phase = ?, base_commit = ?, head_commit = ?, commit_count = ?,
		     slice_count = ?, reviewed_slice_count = ?, skipped_slice_count = ?, accepted_findings = ?, dropped_findings = ?,
		     scanner_policy_version = ?, policy_digest = ?, idempotency_key = ?,
		     summary = ?, error_message = ?, started_at = ?, completed_at = ?
		 WHERE namespace = ? AND id = ?`,
		run.TaskName, run.Mode, run.Phase, run.BaseCommit, run.HeadCommit, run.CommitCount,
		run.SliceCount, run.ReviewedSliceCount, run.SkippedSliceCount, run.AcceptedFindings, run.DroppedFindings,
		run.ScannerPolicyVersion, run.PolicyDigest, run.IdempotencyKey,
		run.Summary, run.ErrorMessage, run.StartedAt, run.CompletedAt, run.Namespace, run.ID,
	)
	return err
}

// GetScanRun fetches a scan run by ID.
func (s *Store) GetScanRun(ctx context.Context, namespace, id string) (*store.ScanRun, error) {
	var run store.ScanRun
	err := s.db.QueryRowContext(ctx,
		`SELECT id, namespace, repository_scan, task_name, mode, phase, started_at, completed_at,
		        base_commit, head_commit, commit_count, slice_count, reviewed_slice_count, skipped_slice_count,
		        accepted_findings, dropped_findings, scanner_policy_version, policy_digest, idempotency_key,
		        summary, error_message
		 FROM security_scan_runs WHERE namespace = ? AND id = ?`,
		namespace, id,
	).Scan(
		&run.ID, &run.Namespace, &run.RepositoryScan, &run.TaskName, &run.Mode, &run.Phase,
		&run.StartedAt, &run.CompletedAt, &run.BaseCommit, &run.HeadCommit, &run.CommitCount,
		&run.SliceCount, &run.ReviewedSliceCount, &run.SkippedSliceCount, &run.AcceptedFindings,
		&run.DroppedFindings, &run.ScannerPolicyVersion, &run.PolicyDigest, &run.IdempotencyKey,
		&run.Summary, &run.ErrorMessage,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// ListScanRuns lists scan runs for a repository scan ordered newest first.
func (s *Store) ListScanRuns(ctx context.Context, namespace, repositoryScan string, limit int, cursor string) ([]store.ScanRun, string, error) {
	offset, err := parseOffsetCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, namespace, repository_scan, task_name, mode, phase, started_at, completed_at,
		        base_commit, head_commit, commit_count, slice_count, reviewed_slice_count, skipped_slice_count,
		        accepted_findings, dropped_findings, scanner_policy_version, policy_digest, idempotency_key,
		        summary, error_message
		 FROM security_scan_runs
		 WHERE namespace = ? AND repository_scan = ?
		 ORDER BY started_at DESC, id DESC
		 LIMIT ? OFFSET ?`,
		namespace, repositoryScan, limit, offset,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck

	var runs []store.ScanRun
	for rows.Next() {
		var run store.ScanRun
		if err := rows.Scan(
			&run.ID, &run.Namespace, &run.RepositoryScan, &run.TaskName, &run.Mode, &run.Phase,
			&run.StartedAt, &run.CompletedAt, &run.BaseCommit, &run.HeadCommit, &run.CommitCount,
			&run.SliceCount, &run.ReviewedSliceCount, &run.SkippedSliceCount, &run.AcceptedFindings,
			&run.DroppedFindings, &run.ScannerPolicyVersion, &run.PolicyDigest, &run.IdempotencyKey,
			&run.Summary, &run.ErrorMessage,
		); err != nil {
			return nil, "", err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	return runs, nextOffsetCursor(offset, len(runs), limit), nil
}

// UpsertReviewSlice inserts or updates a deterministic review slice.
func (s *Store) UpsertReviewSlice(ctx context.Context, slice *store.ReviewSlice) error {
	if slice.SchemaVersion == 0 {
		slice.SchemaVersion = 1
	}
	if slice.Kind == "" {
		slice.Kind = "unknown"
	}
	if slice.Confidence == "" {
		slice.Confidence = "medium"
	}
	if slice.Status == "" {
		slice.Status = "pending"
	}

	entrypointsJSON, err := marshalSecurityJSON(slice.Entrypoints)
	if err != nil {
		return err
	}
	ownedFilesJSON, err := marshalSecurityJSON(slice.OwnedFiles)
	if err != nil {
		return err
	}
	contextFilesJSON, err := marshalSecurityJSON(slice.ContextFiles)
	if err != nil {
		return err
	}
	testsJSON, err := marshalSecurityJSON(slice.Tests)
	if err != nil {
		return err
	}
	tagsJSON, err := marshalSecurityJSON(slice.Tags)
	if err != nil {
		return err
	}
	trustBoundariesJSON, err := marshalSecurityJSON(slice.TrustBoundaries)
	if err != nil {
		return err
	}
	changedFilesJSON, err := marshalSecurityJSON(slice.ChangedFiles)
	if err != nil {
		return err
	}
	changedLineRangesJSON, err := marshalSecurityJSON(slice.ChangedLineRanges)
	if err != nil {
		return err
	}

	now := time.Now()
	if slice.CreatedAt.IsZero() {
		slice.CreatedAt = now
	}
	slice.UpdatedAt = now

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO security_review_slices
		 (id, namespace, repository_scan, source, title, summary, kind, confidence, status,
		  entrypoints_json, owned_files_json, context_files_json, tests_json, tags_json,
		  trust_boundaries_json, changed_files_json, changed_line_ranges_json, last_scan_run_id, last_reviewed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, repository_scan, id) DO UPDATE SET
		   source = excluded.source,
		   title = excluded.title,
		   summary = excluded.summary,
		   kind = excluded.kind,
		   confidence = excluded.confidence,
		   status = excluded.status,
		   entrypoints_json = excluded.entrypoints_json,
		   owned_files_json = excluded.owned_files_json,
		   context_files_json = excluded.context_files_json,
		   tests_json = excluded.tests_json,
		   tags_json = excluded.tags_json,
		   trust_boundaries_json = excluded.trust_boundaries_json,
		   changed_files_json = excluded.changed_files_json,
		   changed_line_ranges_json = excluded.changed_line_ranges_json,
		   last_scan_run_id = excluded.last_scan_run_id,
		   last_reviewed_at = COALESCE(excluded.last_reviewed_at, security_review_slices.last_reviewed_at),
		   updated_at = excluded.updated_at`,
		slice.ID, slice.Namespace, slice.RepositoryScan, slice.Source, slice.Title, slice.Summary,
		slice.Kind, slice.Confidence, slice.Status, entrypointsJSON, ownedFilesJSON, contextFilesJSON,
		testsJSON, tagsJSON, trustBoundariesJSON, changedFilesJSON, changedLineRangesJSON,
		slice.LastScanRunID, slice.LastReviewedAt, slice.CreatedAt, slice.UpdatedAt,
	)
	return err
}

func scanReviewSlice(scanner interface {
	Scan(dest ...any) error
}) (*store.ReviewSlice, error) {
	var (
		slice                 store.ReviewSlice
		entrypointsJSON       string
		ownedFilesJSON        string
		contextFilesJSON      string
		testsJSON             string
		tagsJSON              string
		trustBoundariesJSON   string
		changedFilesJSON      string
		changedLineRangesJSON string
	)
	err := scanner.Scan(
		&slice.ID, &slice.Namespace, &slice.RepositoryScan, &slice.Source, &slice.Title,
		&slice.Summary, &slice.Kind, &slice.Confidence, &slice.Status, &entrypointsJSON,
		&ownedFilesJSON, &contextFilesJSON, &testsJSON, &tagsJSON, &trustBoundariesJSON,
		&changedFilesJSON, &changedLineRangesJSON, &slice.LastScanRunID, &slice.LastReviewedAt, &slice.CreatedAt, &slice.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	slice.SchemaVersion = 1
	if err := unmarshalSecurityJSON(entrypointsJSON, &slice.Entrypoints); err != nil {
		return nil, err
	}
	if err := unmarshalSecurityJSON(ownedFilesJSON, &slice.OwnedFiles); err != nil {
		return nil, err
	}
	if err := unmarshalSecurityJSON(contextFilesJSON, &slice.ContextFiles); err != nil {
		return nil, err
	}
	if err := unmarshalSecurityJSON(testsJSON, &slice.Tests); err != nil {
		return nil, err
	}
	if err := unmarshalSecurityJSON(tagsJSON, &slice.Tags); err != nil {
		return nil, err
	}
	if err := unmarshalSecurityJSON(trustBoundariesJSON, &slice.TrustBoundaries); err != nil {
		return nil, err
	}
	if err := unmarshalSecurityJSON(changedFilesJSON, &slice.ChangedFiles); err != nil {
		return nil, err
	}
	if err := unmarshalSecurityJSON(changedLineRangesJSON, &slice.ChangedLineRanges); err != nil {
		return nil, err
	}
	return &slice, nil
}

// ListReviewSlices lists review slices for a repository scan.
func (s *Store) ListReviewSlices(ctx context.Context, filter store.ReviewSliceFilter) ([]store.ReviewSlice, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	if filter.Limit <= 0 {
		filter.Limit = 100
	}

	query := strings.Builder{}
	query.WriteString(`SELECT id, namespace, repository_scan, source, title, summary, kind, confidence, status,
		entrypoints_json, owned_files_json, context_files_json, tests_json, tags_json, trust_boundaries_json,
		changed_files_json, changed_line_ranges_json, last_scan_run_id, last_reviewed_at, created_at, updated_at
		FROM security_review_slices WHERE namespace = ? AND repository_scan = ?`)
	args := []any{filter.Namespace, filter.RepositoryScan}
	if filter.Status != "" {
		query.WriteString(` AND status = ?`)
		args = append(args, filter.Status)
	}
	if filter.LastScanRunID != "" {
		query.WriteString(` AND last_scan_run_id = ?`)
		args = append(args, filter.LastScanRunID)
	}
	query.WriteString(` ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck

	var slices []store.ReviewSlice
	for rows.Next() {
		slice, err := scanReviewSlice(rows)
		if err != nil {
			return nil, "", err
		}
		slices = append(slices, *slice)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return slices, nextOffsetCursor(offset, len(slices), filter.Limit), nil
}

// GetReviewSlice returns one review slice.
func (s *Store) GetReviewSlice(ctx context.Context, namespace, repositoryScan, id string) (*store.ReviewSlice, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, namespace, repository_scan, source, title, summary, kind, confidence, status,
		        entrypoints_json, owned_files_json, context_files_json, tests_json, tags_json, trust_boundaries_json,
		        changed_files_json, changed_line_ranges_json, last_scan_run_id, last_reviewed_at, created_at, updated_at
		 FROM security_review_slices
		 WHERE namespace = ? AND repository_scan = ? AND id = ?`,
		namespace, repositoryScan, id,
	)
	slice, err := scanReviewSlice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return slice, nil
}

// UpdateReviewSliceStatus updates slice status and review timestamp.
func (s *Store) UpdateReviewSliceStatus(ctx context.Context, namespace, repositoryScan, id, lastScanRunID, status string) error {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE security_review_slices
		 SET status = ?, last_reviewed_at = CASE WHEN ? IN ('reviewed', 'completed') THEN ? ELSE last_reviewed_at END,
		     updated_at = ?
		 WHERE namespace = ? AND repository_scan = ? AND id = ? AND last_scan_run_id = ?`,
		status, status, now, now, namespace, repositoryScan, id, lastScanRunID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetLatestThreatModel returns the current threat model for a repository.
func (s *Store) GetLatestThreatModel(ctx context.Context, namespace, repositoryScan string) (*store.ThreatModel, error) {
	var model store.ThreatModel
	err := s.db.QueryRowContext(ctx,
		`SELECT namespace, repository_scan, version, content, source, generated_by_scan, created_at, updated_at
		 FROM security_threat_models
		 WHERE namespace = ? AND repository_scan = ?
		 ORDER BY version DESC
		 LIMIT 1`,
		namespace, repositoryScan,
	).Scan(
		&model.Namespace, &model.RepositoryScan, &model.Version, &model.Content, &model.Source,
		&model.GeneratedByScan, &model.CreatedAt, &model.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &model, nil
}

// SaveThreatModel stores the current threat model, replacing any older copies for the repository.
// When Version is zero, the revision number is incremented from the latest stored model.
func (s *Store) SaveThreatModel(ctx context.Context, model *store.ThreatModel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var latestVersion int64
	err = tx.QueryRowContext(ctx,
		`SELECT version FROM security_threat_models WHERE namespace = ? AND repository_scan = ? ORDER BY version DESC LIMIT 1`,
		model.Namespace, model.RepositoryScan,
	).Scan(&latestVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		latestVersion = 0
	case err != nil:
		return err
	}

	if model.Version == 0 {
		model.Version = latestVersion + 1
	}

	now := time.Now()
	if model.CreatedAt.IsZero() {
		model.CreatedAt = now
	}
	model.UpdatedAt = now

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM security_threat_models WHERE namespace = ? AND repository_scan = ?`,
		model.Namespace, model.RepositoryScan,
	); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO security_threat_models
		 (namespace, repository_scan, version, content, source, generated_by_scan, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		model.Namespace, model.RepositoryScan, model.Version, model.Content, model.Source,
		model.GeneratedByScan, model.CreatedAt, model.UpdatedAt,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func marshalEvidence(evidence []store.FindingEvidenceRef) (string, error) {
	if len(evidence) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalEvidence(payload string) ([]store.FindingEvidenceRef, error) {
	if strings.TrimSpace(payload) == "" {
		return nil, nil
	}
	var evidence []store.FindingEvidenceRef
	if err := json.Unmarshal([]byte(payload), &evidence); err != nil {
		return nil, err
	}
	return evidence, nil
}

// UpsertFinding inserts or updates a finding keyed by repository fingerprint.
func (s *Store) UpsertFinding(ctx context.Context, finding *store.Finding) error {
	if finding.ID == "" {
		finding.ID = finding.Fingerprint
	}

	evidenceJSON, err := marshalEvidence(finding.Evidence)
	if err != nil {
		return err
	}

	now := time.Now()
	if finding.CreatedAt.IsZero() {
		finding.CreatedAt = now
	}
	finding.UpdatedAt = now

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO security_findings
		 (id, namespace, repository_scan, scan_run_id, slice_id, fingerprint, title, category, summary, severity, confidence, triage,
		  validation_status, state, file_path, line, commit_sha, root_cause, reproduction, remediation, suggested_action,
		  why_tests_do_not_cover, suggested_regression_test, minimum_fix_scope, evidence_json, validation_json, patch_proposal_id,
		  pr_number, pr_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, repository_scan, fingerprint) DO UPDATE SET
		   scan_run_id = excluded.scan_run_id,
		   slice_id = excluded.slice_id,
		   title = excluded.title,
		   category = excluded.category,
		   summary = excluded.summary,
		   severity = excluded.severity,
		   confidence = excluded.confidence,
		   triage = excluded.triage,
		   validation_status = CASE
		     WHEN security_findings.validation_status = 'validated'
		       AND excluded.validation_status != 'validated'
		       THEN security_findings.validation_status
		     WHEN excluded.validation_status IN ('validated', 'failed', 'skipped', 'pending')
		       THEN excluded.validation_status
		     WHEN CASE security_findings.validation_status
		       WHEN 'validated' THEN 4
		       WHEN 'failed' THEN 3
		       WHEN 'skipped' THEN 3
		       WHEN 'pending' THEN 2
		       WHEN 'unvalidated' THEN 1
		       ELSE 0
		     END >= CASE excluded.validation_status
		       WHEN 'validated' THEN 4
		       WHEN 'failed' THEN 3
		       WHEN 'skipped' THEN 3
		       WHEN 'pending' THEN 2
		       WHEN 'unvalidated' THEN 1
		       ELSE 0
		     END THEN security_findings.validation_status
		     ELSE excluded.validation_status
		   END,
		   state = CASE
		     WHEN security_findings.state IN ('fixed', 'resolved', 'dismissed', 'suppressed', 'false_positive')
		       THEN security_findings.state
		     WHEN security_findings.state = 'patch_pending'
		       AND excluded.state = 'open'
		       THEN excluded.state
		     WHEN CASE security_findings.state
		       WHEN 'pr_open' THEN 4
		       WHEN 'patch_ready' THEN 3
		       WHEN 'patch_pending' THEN 2
		       WHEN 'open' THEN 1
		       ELSE 0
		     END >= CASE excluded.state
		       WHEN 'pr_open' THEN 4
		       WHEN 'patch_ready' THEN 3
		       WHEN 'patch_pending' THEN 2
		       WHEN 'open' THEN 1
		       ELSE 0
		     END THEN security_findings.state
		     ELSE excluded.state
		   END,
		   file_path = excluded.file_path,
		   line = excluded.line,
		   commit_sha = excluded.commit_sha,
		   root_cause = excluded.root_cause,
		   reproduction = excluded.reproduction,
		   remediation = excluded.remediation,
		   suggested_action = excluded.suggested_action,
		   why_tests_do_not_cover = excluded.why_tests_do_not_cover,
		   suggested_regression_test = excluded.suggested_regression_test,
		   minimum_fix_scope = excluded.minimum_fix_scope,
		   evidence_json = excluded.evidence_json,
		   validation_json = CASE
		     WHEN security_findings.validation_status = 'validated'
		       AND excluded.validation_status != 'validated'
		       THEN security_findings.validation_json
		     WHEN excluded.validation_status IN ('validated', 'failed', 'skipped', 'pending')
		       THEN excluded.validation_json
		     WHEN CASE security_findings.validation_status
		       WHEN 'validated' THEN 4
		       WHEN 'failed' THEN 3
		       WHEN 'skipped' THEN 3
		       WHEN 'pending' THEN 2
		       WHEN 'unvalidated' THEN 1
		       ELSE 0
		     END >= CASE excluded.validation_status
		       WHEN 'validated' THEN 4
		       WHEN 'failed' THEN 3
		       WHEN 'skipped' THEN 3
		       WHEN 'pending' THEN 2
		       WHEN 'unvalidated' THEN 1
		       ELSE 0
		     END THEN security_findings.validation_json
		     ELSE excluded.validation_json
		   END,
		   patch_proposal_id = CASE
		     WHEN excluded.patch_proposal_id IS NOT NULL AND excluded.patch_proposal_id != '' THEN excluded.patch_proposal_id
		     ELSE security_findings.patch_proposal_id
		   END,
		   pr_number = COALESCE(excluded.pr_number, security_findings.pr_number),
		   pr_url = CASE
		     WHEN excluded.pr_url IS NOT NULL AND excluded.pr_url != '' THEN excluded.pr_url
		     ELSE security_findings.pr_url
		   END,
		   updated_at = excluded.updated_at`,
		finding.ID, finding.Namespace, finding.RepositoryScan, finding.ScanRunID, finding.SliceID, finding.Fingerprint,
		finding.Title, finding.Category, finding.Summary, finding.Severity, finding.Confidence, finding.Triage,
		finding.ValidationStatus, finding.State, finding.FilePath, finding.Line, finding.CommitSHA, finding.RootCause,
		finding.Reproduction, finding.Remediation, finding.SuggestedAction, finding.WhyTestsDoNotAlreadyCoverThis,
		finding.SuggestedRegressionTest, finding.MinimumFixScope, evidenceJSON, finding.ValidationJSON, finding.PatchProposalID,
		finding.PRNumber, finding.PRURL, finding.CreatedAt, finding.UpdatedAt,
	)
	return err
}

func scanFinding(scanner interface {
	Scan(dest ...any) error
}) (*store.Finding, error) {
	var (
		finding      store.Finding
		evidenceJSON string
	)
	err := scanner.Scan(
		&finding.ID, &finding.Namespace, &finding.RepositoryScan, &finding.ScanRunID, &finding.SliceID, &finding.Fingerprint,
		&finding.Title, &finding.Category, &finding.Summary, &finding.Severity, &finding.Confidence, &finding.Triage,
		&finding.ValidationStatus, &finding.State, &finding.FilePath, &finding.Line, &finding.CommitSHA, &finding.RootCause,
		&finding.Reproduction, &finding.Remediation, &finding.SuggestedAction, &finding.WhyTestsDoNotAlreadyCoverThis,
		&finding.SuggestedRegressionTest, &finding.MinimumFixScope, &evidenceJSON, &finding.ValidationJSON, &finding.PatchProposalID,
		&finding.PRNumber, &finding.PRURL, &finding.CreatedAt, &finding.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	finding.Evidence, err = unmarshalEvidence(evidenceJSON)
	if err != nil {
		return nil, err
	}
	return &finding, nil
}

// GetFinding returns a finding by ID.
func (s *Store) GetFinding(ctx context.Context, namespace, id string) (*store.Finding, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, namespace, repository_scan, scan_run_id, slice_id, fingerprint, title, category, summary, severity,
		        confidence, triage, validation_status, state, file_path, line, commit_sha, root_cause, reproduction,
		        remediation, suggested_action, why_tests_do_not_cover, suggested_regression_test, minimum_fix_scope,
		        evidence_json, validation_json, patch_proposal_id, pr_number, pr_url, created_at, updated_at
		 FROM security_findings
		 WHERE namespace = ? AND id = ?`,
		namespace, id,
	)
	finding, err := scanFinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return finding, nil
}

// ListFindings lists findings for a repository scan with optional filtering.
func (s *Store) ListFindings(ctx context.Context, filter store.FindingFilter) ([]store.Finding, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	if filter.Limit <= 0 {
		filter.Limit = 50
	}

	query := strings.Builder{}
	query.WriteString(`SELECT id, namespace, repository_scan, scan_run_id, slice_id, fingerprint, title, category, summary, severity,
		confidence, triage, validation_status, state, file_path, line, commit_sha, root_cause, reproduction,
		remediation, suggested_action, why_tests_do_not_cover, suggested_regression_test, minimum_fix_scope,
		evidence_json, validation_json, patch_proposal_id, pr_number, pr_url, created_at, updated_at
		FROM security_findings WHERE namespace = ?`)
	args := []any{filter.Namespace}

	if filter.RepositoryScan != "" {
		query.WriteString(` AND repository_scan = ?`)
		args = append(args, filter.RepositoryScan)
	}
	if filter.SliceID != "" {
		query.WriteString(` AND slice_id = ?`)
		args = append(args, filter.SliceID)
	}
	if filter.Category != "" {
		query.WriteString(` AND category = ?`)
		args = append(args, filter.Category)
	}
	if filter.Severity != "" {
		query.WriteString(` AND severity = ?`)
		args = append(args, filter.Severity)
	}
	if filter.ValidationStatus != "" {
		query.WriteString(` AND validation_status = ?`)
		args = append(args, filter.ValidationStatus)
	}
	if filter.State != "" {
		query.WriteString(` AND state = ?`)
		args = append(args, filter.State)
	}

	if filter.Recommended {
		query.WriteString(` AND validation_status != 'failed' AND state NOT IN ('dismissed', 'suppressed', 'false_positive', 'fixed', 'resolved')`)
		query.WriteString(` ORDER BY
			CASE severity
				WHEN 'critical' THEN 4
				WHEN 'high' THEN 3
				WHEN 'medium' THEN 2
				WHEN 'low' THEN 1
				ELSE 0
			END DESC,
			CASE validation_status
				WHEN 'validated' THEN 3
				WHEN 'unvalidated' THEN 2
				WHEN 'skipped' THEN 1
				ELSE 0
			END DESC,
			CASE confidence
				WHEN 'high' THEN 3
				WHEN 'medium' THEN 2
				WHEN 'low' THEN 1
				ELSE 0
			END DESC,
			updated_at DESC, id DESC`)
	} else {
		query.WriteString(` ORDER BY updated_at DESC, id DESC`)
	}

	query.WriteString(` LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck

	var findings []store.Finding
	for rows.Next() {
		finding, err := scanFinding(rows)
		if err != nil {
			return nil, "", err
		}
		findings = append(findings, *finding)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	return findings, nextOffsetCursor(offset, len(findings), filter.Limit), nil
}

// GetFindingCounts returns current open finding counts by severity.
func (s *Store) GetFindingCounts(ctx context.Context, namespace, repositoryScan string) (store.FindingCounts, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN severity = 'critical' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN severity = 'high' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN severity = 'medium' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN severity = 'low' THEN 1 ELSE 0 END), 0)
		FROM security_findings
		WHERE namespace = ? AND repository_scan = ? AND state IN ('open', 'patch_pending', 'patch_ready', 'pr_open')`,
		namespace, repositoryScan,
	)
	var counts store.FindingCounts
	if err := row.Scan(&counts.Total, &counts.Critical, &counts.High, &counts.Medium, &counts.Low); err != nil {
		return store.FindingCounts{}, err
	}
	return counts, nil
}

// UpdateFindingState updates the user-visible finding state.
func (s *Store) UpdateFindingState(ctx context.Context, namespace, id, state string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE security_findings SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE namespace = ? AND id = ?`,
		state, namespace, id,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CreatePatchProposal inserts a new patch proposal.
func (s *Store) CreatePatchProposal(ctx context.Context, proposal *store.PatchProposal) error {
	now := time.Now()
	if proposal.CreatedAt.IsZero() {
		proposal.CreatedAt = now
	}
	proposal.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO security_patch_proposals
		 (id, namespace, repository_scan, finding_id, task_name, branch, diff_artifact, summary_artifact, status, pr_number, pr_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		proposal.ID, proposal.Namespace, proposal.RepositoryScan, proposal.FindingID, proposal.TaskName, proposal.Branch,
		proposal.DiffArtifact, proposal.SummaryArtifact, proposal.Status, proposal.PRNumber, proposal.PRURL, proposal.CreatedAt, proposal.UpdatedAt,
	)
	return err
}

// UpdatePatchProposal updates an existing patch proposal.
func (s *Store) UpdatePatchProposal(ctx context.Context, proposal *store.PatchProposal) error {
	proposal.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE security_patch_proposals
		 SET task_name = ?, branch = ?, diff_artifact = ?, summary_artifact = ?, status = ?, pr_number = ?, pr_url = ?, updated_at = ?
		 WHERE namespace = ? AND id = ?`,
		proposal.TaskName, proposal.Branch, proposal.DiffArtifact, proposal.SummaryArtifact, proposal.Status, proposal.PRNumber,
		proposal.PRURL, proposal.UpdatedAt, proposal.Namespace, proposal.ID,
	)
	return err
}

// ListPatchProposals lists patch proposals for a finding, newest first.
func (s *Store) ListPatchProposals(ctx context.Context, namespace, findingID string) ([]store.PatchProposal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, namespace, repository_scan, finding_id, task_name, branch, diff_artifact, summary_artifact,
		        status, pr_number, pr_url, created_at, updated_at
		 FROM security_patch_proposals
		 WHERE namespace = ? AND finding_id = ?
		 ORDER BY created_at DESC, id DESC`,
		namespace, findingID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var proposals []store.PatchProposal
	for rows.Next() {
		var proposal store.PatchProposal
		if err := rows.Scan(
			&proposal.ID, &proposal.Namespace, &proposal.RepositoryScan, &proposal.FindingID, &proposal.TaskName, &proposal.Branch,
			&proposal.DiffArtifact, &proposal.SummaryArtifact, &proposal.Status, &proposal.PRNumber, &proposal.PRURL,
			&proposal.CreatedAt, &proposal.UpdatedAt,
		); err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	return proposals, rows.Err()
}

// CreateDroppedFinding records a rejected finding diagnostic.
func (s *Store) CreateDroppedFinding(ctx context.Context, dropped *store.DroppedFinding) error {
	if dropped.ID == "" {
		dropped.ID = "drop_" + shortStoreHash(strings.Join([]string{
			dropped.Namespace,
			dropped.RepositoryScan,
			dropped.ScanRunID,
			dropped.TaskName,
			dropped.SliceID,
			dropped.Reason,
			dropped.SampleJSON,
			time.Now().Format(time.RFC3339Nano),
		}, "|"))
	}
	if dropped.CreatedAt.IsZero() {
		dropped.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO security_dropped_findings
		 (id, namespace, repository_scan, scan_run_id, task_name, slice_id, reason, layer, sample_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dropped.ID, dropped.Namespace, dropped.RepositoryScan, dropped.ScanRunID, dropped.TaskName,
		dropped.SliceID, dropped.Reason, dropped.Layer, dropped.SampleJSON, dropped.CreatedAt,
	)
	return err
}

// ListDroppedFindings lists rejected finding diagnostics.
func (s *Store) ListDroppedFindings(ctx context.Context, filter store.DroppedFindingFilter) ([]store.DroppedFinding, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	if filter.Limit <= 0 {
		filter.Limit = 50
	}

	query := strings.Builder{}
	query.WriteString(`SELECT id, namespace, repository_scan, scan_run_id, task_name, slice_id, reason, layer, sample_json, created_at
		FROM security_dropped_findings WHERE namespace = ?`)
	args := []any{filter.Namespace}
	if filter.RepositoryScan != "" {
		query.WriteString(` AND repository_scan = ?`)
		args = append(args, filter.RepositoryScan)
	}
	if filter.ScanRunID != "" {
		query.WriteString(` AND scan_run_id = ?`)
		args = append(args, filter.ScanRunID)
	}
	if filter.SliceID != "" {
		query.WriteString(` AND slice_id = ?`)
		args = append(args, filter.SliceID)
	}
	if filter.Layer != "" {
		query.WriteString(` AND layer = ?`)
		args = append(args, filter.Layer)
	}
	if filter.Reason != "" {
		query.WriteString(` AND reason = ?`)
		args = append(args, filter.Reason)
	}
	if filter.ReasonContains != "" {
		query.WriteString(` AND reason LIKE ?`)
		args = append(args, "%"+filter.ReasonContains+"%")
	}
	query.WriteString(` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck

	var dropped []store.DroppedFinding
	for rows.Next() {
		var item store.DroppedFinding
		if err := rows.Scan(
			&item.ID, &item.Namespace, &item.RepositoryScan, &item.ScanRunID, &item.TaskName,
			&item.SliceID, &item.Reason, &item.Layer, &item.SampleJSON, &item.CreatedAt,
		); err != nil {
			return nil, "", err
		}
		dropped = append(dropped, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return dropped, nextOffsetCursor(offset, len(dropped), filter.Limit), nil
}
