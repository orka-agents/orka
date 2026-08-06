package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func normalizeOutputProvenance(provenance *store.OutputProvenance, data []byte) error {
	if provenance == nil {
		return store.ValidationErrorf("output provenance is required")
	}
	if strings.TrimSpace(provenance.TaskUID) == "" {
		return store.ValidationErrorf("output taskUID is required")
	}
	if provenance.TaskAttempt < 0 {
		return store.ValidationErrorf("output taskAttempt must be non-negative")
	}
	switch provenance.ProducerKind {
	case store.OutputProducerKubernetesWorker:
		if strings.TrimSpace(provenance.JobUID) == "" || strings.TrimSpace(provenance.PodUID) == "" {
			return store.ValidationErrorf("kubernetes output requires jobUID and podUID")
		}
	case store.OutputProducerHarnessWrapper:
		if strings.TrimSpace(provenance.PodUID) == "" || strings.TrimSpace(provenance.RuntimeSessionID) == "" ||
			strings.TrimSpace(provenance.TurnID) == "" || strings.TrimSpace(provenance.CorrelationID) == "" {
			return store.ValidationErrorf("harness output requires pod and turn identity")
		}
	case store.OutputProducerControllerHarness:
		if strings.TrimSpace(provenance.RuntimeSessionID) == "" || strings.TrimSpace(provenance.TurnID) == "" ||
			strings.TrimSpace(provenance.CorrelationID) == "" {
			return store.ValidationErrorf("controller harness output requires turn identity")
		}
	case store.OutputProducerController:
		// Controller-owned artifacts are bound by Task UID/attempt and never model-supplied.
	default:
		return store.ValidationErrorf("unsupported output producerKind %q", provenance.ProducerKind)
	}
	if strings.TrimSpace(provenance.SubmissionNonceDigest) == "" {
		return store.ValidationErrorf("output submissionNonceDigest is required")
	}
	digest := sha256.Sum256(data)
	provenance.ContentSHA256 = "sha256:" + hex.EncodeToString(digest[:])
	provenance.ContentSize = int64(len(data))
	provenance.AcceptedAt = time.Now().UTC()
	return nil
}

func sameOutputWriter(current, next store.OutputProvenance) bool {
	return current.TaskUID == next.TaskUID && current.TaskAttempt == next.TaskAttempt &&
		current.JobUID == next.JobUID && current.PodUID == next.PodUID &&
		current.ProducerKind == next.ProducerKind &&
		current.RuntimeSessionID == next.RuntimeSessionID && current.TurnID == next.TurnID &&
		current.CorrelationID == next.CorrelationID &&
		current.SubmissionNonceDigest == next.SubmissionNonceDigest
}

func outputWriterCanReplace(current, next store.OutputProvenance) bool {
	if sameOutputWriter(current, next) {
		return true
	}
	if current.TaskUID == "" {
		return true
	}
	return current.TaskUID == next.TaskUID && next.TaskAttempt > current.TaskAttempt
}

func scanOutputProvenance(scanner interface{ Scan(...any) error }) (store.OutputProvenance, error) {
	var provenance store.OutputProvenance
	err := scanner.Scan(
		&provenance.TaskUID, &provenance.JobUID, &provenance.PodUID, &provenance.TaskAttempt,
		&provenance.ProducerKind, &provenance.RuntimeSessionID, &provenance.TurnID, &provenance.CorrelationID,
		&provenance.SubmissionNonceDigest, &provenance.StagingGeneration, &provenance.ContentSize,
		&provenance.ContentSHA256, &provenance.AcceptedAt,
	)
	return provenance, err
}

func (s *Store) SaveBoundResult(ctx context.Context, result *store.BoundResult) error {
	if result == nil || strings.TrimSpace(result.Namespace) == "" || strings.TrimSpace(result.TaskName) == "" {
		return store.ValidationErrorf("bound result namespace and taskName are required")
	}
	if err := normalizeOutputProvenance(&result.Provenance, result.Data); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanOutputProvenance(tx.QueryRowContext(ctx, `SELECT task_uid, job_uid, pod_uid, task_attempt,
		producer_kind, runtime_session_id, turn_id, correlation_id, submission_nonce_digest, staging_generation,
		content_size, content_sha256, accepted_at FROM results WHERE namespace = ? AND task_name = ?`, result.Namespace, result.TaskName))
	switch {
	case err == nil:
		if !outputWriterCanReplace(current, result.Provenance) {
			return fmt.Errorf("%w: result belongs to a different task attempt or writer", store.ErrConflict)
		}
		result.Provenance.StagingGeneration = current.StagingGeneration + 1
	case errors.Is(err, sql.ErrNoRows):
		result.Provenance.StagingGeneration = 1
	default:
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO results
		(namespace, task_name, data, task_uid, job_uid, pod_uid, task_attempt, producer_kind,
		 runtime_session_id, turn_id, correlation_id, submission_nonce_digest, staging_generation,
		 content_size, content_sha256, accepted_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(namespace, task_name) DO UPDATE SET data = excluded.data, task_uid = excluded.task_uid,
		 job_uid = excluded.job_uid, pod_uid = excluded.pod_uid, task_attempt = excluded.task_attempt,
		 producer_kind = excluded.producer_kind, runtime_session_id = excluded.runtime_session_id,
		 turn_id = excluded.turn_id, correlation_id = excluded.correlation_id,
		 submission_nonce_digest = excluded.submission_nonce_digest, staging_generation = excluded.staging_generation,
		 content_size = excluded.content_size, content_sha256 = excluded.content_sha256,
		 accepted_at = excluded.accepted_at, updated_at = CURRENT_TIMESTAMP`,
		result.Namespace, result.TaskName, result.Data, result.Provenance.TaskUID, result.Provenance.JobUID,
		result.Provenance.PodUID, result.Provenance.TaskAttempt, result.Provenance.ProducerKind,
		result.Provenance.RuntimeSessionID, result.Provenance.TurnID, result.Provenance.CorrelationID,
		result.Provenance.SubmissionNonceDigest, result.Provenance.StagingGeneration, result.Provenance.ContentSize,
		result.Provenance.ContentSHA256, result.Provenance.AcceptedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetBoundResult(ctx context.Context, namespace, taskName, taskUID string, attempt int64) (*store.BoundResult, error) {
	var data []byte
	row := s.db.QueryRowContext(ctx, `SELECT data, task_uid, job_uid, pod_uid, task_attempt, producer_kind,
		runtime_session_id, turn_id, correlation_id, submission_nonce_digest, staging_generation,
		content_size, content_sha256, accepted_at FROM results WHERE namespace = ? AND task_name = ?`, namespace, taskName)
	var provenance store.OutputProvenance
	err := row.Scan(&data, &provenance.TaskUID, &provenance.JobUID, &provenance.PodUID, &provenance.TaskAttempt,
		&provenance.ProducerKind, &provenance.RuntimeSessionID, &provenance.TurnID, &provenance.CorrelationID,
		&provenance.SubmissionNonceDigest, &provenance.StagingGeneration, &provenance.ContentSize,
		&provenance.ContentSHA256, &provenance.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if provenance.TaskUID == "" {
		return nil, fmt.Errorf("%w: result has legacy-unverified provenance", store.ErrConflict)
	}
	if provenance.TaskUID != taskUID || provenance.TaskAttempt != attempt {
		if provenance.TaskUID == taskUID && provenance.TaskAttempt < attempt {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("%w: result belongs to a different task attempt", store.ErrConflict)
	}
	return &store.BoundResult{Namespace: namespace, TaskName: taskName, Data: data, Provenance: provenance}, nil
}

func (s *Store) SaveBoundArtifact(ctx context.Context, artifact *store.BoundArtifact) error {
	if artifact == nil || strings.TrimSpace(artifact.Namespace) == "" || strings.TrimSpace(artifact.TaskName) == "" ||
		strings.TrimSpace(artifact.Filename) == "" {
		return store.ValidationErrorf("bound artifact namespace, taskName, and filename are required")
	}
	if len(artifact.Data) > maxArtifactSize {
		return fmt.Errorf("artifact %q exceeds maximum size of 10MB", artifact.Filename)
	}
	if err := normalizeOutputProvenance(&artifact.Provenance, artifact.Data); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanOutputProvenance(tx.QueryRowContext(ctx, `SELECT task_uid, job_uid, pod_uid, task_attempt,
		producer_kind, runtime_session_id, turn_id, correlation_id, submission_nonce_digest, staging_generation,
		content_size, content_sha256, accepted_at FROM artifacts WHERE namespace = ? AND task_name = ? AND filename = ?`,
		artifact.Namespace, artifact.TaskName, artifact.Filename))
	switch {
	case err == nil:
		if !outputWriterCanReplace(current, artifact.Provenance) {
			return fmt.Errorf("%w: artifact belongs to a different task attempt or writer", store.ErrConflict)
		}
		artifact.Provenance.StagingGeneration = current.StagingGeneration + 1
	case errors.Is(err, sql.ErrNoRows):
		artifact.Provenance.StagingGeneration = 1
	default:
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO artifacts
		(namespace, task_name, filename, content_type, size, data, task_uid, job_uid, pod_uid, task_attempt,
		 producer_kind, runtime_session_id, turn_id, correlation_id, submission_nonce_digest, staging_generation,
		 content_size, content_sha256, accepted_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(namespace, task_name, filename) DO UPDATE SET content_type = excluded.content_type,
		 size = excluded.size, data = excluded.data, task_uid = excluded.task_uid, job_uid = excluded.job_uid,
		 pod_uid = excluded.pod_uid, task_attempt = excluded.task_attempt, producer_kind = excluded.producer_kind,
		 runtime_session_id = excluded.runtime_session_id, turn_id = excluded.turn_id,
		 correlation_id = excluded.correlation_id, submission_nonce_digest = excluded.submission_nonce_digest,
		 staging_generation = excluded.staging_generation, content_size = excluded.content_size,
		 content_sha256 = excluded.content_sha256, accepted_at = excluded.accepted_at`,
		artifact.Namespace, artifact.TaskName, artifact.Filename, artifact.ContentType, len(artifact.Data), artifact.Data,
		artifact.Provenance.TaskUID, artifact.Provenance.JobUID, artifact.Provenance.PodUID,
		artifact.Provenance.TaskAttempt, artifact.Provenance.ProducerKind, artifact.Provenance.RuntimeSessionID,
		artifact.Provenance.TurnID, artifact.Provenance.CorrelationID, artifact.Provenance.SubmissionNonceDigest,
		artifact.Provenance.StagingGeneration, artifact.Provenance.ContentSize, artifact.Provenance.ContentSHA256,
		artifact.Provenance.AcceptedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetBoundArtifact(ctx context.Context, namespace, taskName, filename, taskUID string, attempt int64) (*store.BoundArtifact, error) {
	var artifact store.BoundArtifact
	artifact.Namespace, artifact.TaskName, artifact.Filename = namespace, taskName, filename
	err := s.db.QueryRowContext(ctx, `SELECT content_type, data, task_uid, job_uid, pod_uid, task_attempt,
		producer_kind, runtime_session_id, turn_id, correlation_id, submission_nonce_digest, staging_generation,
		content_size, content_sha256, accepted_at FROM artifacts WHERE namespace = ? AND task_name = ? AND filename = ?`,
		namespace, taskName, filename).Scan(&artifact.ContentType, &artifact.Data, &artifact.Provenance.TaskUID,
		&artifact.Provenance.JobUID, &artifact.Provenance.PodUID, &artifact.Provenance.TaskAttempt,
		&artifact.Provenance.ProducerKind, &artifact.Provenance.RuntimeSessionID, &artifact.Provenance.TurnID,
		&artifact.Provenance.CorrelationID, &artifact.Provenance.SubmissionNonceDigest,
		&artifact.Provenance.StagingGeneration, &artifact.Provenance.ContentSize,
		&artifact.Provenance.ContentSHA256, &artifact.Provenance.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if artifact.Provenance.TaskUID == "" {
		return nil, fmt.Errorf("%w: artifact has legacy-unverified provenance", store.ErrConflict)
	}
	if artifact.Provenance.TaskUID != taskUID || artifact.Provenance.TaskAttempt != attempt {
		if artifact.Provenance.TaskUID == taskUID && artifact.Provenance.TaskAttempt < attempt {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("%w: artifact belongs to a different task attempt", store.ErrConflict)
	}
	return &artifact, nil
}

func (s *Store) ListBoundArtifacts(ctx context.Context, namespace, taskName, taskUID string, attempt int64) ([]store.ArtifactMetadata, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT filename, content_type, size, created_at, task_uid, task_attempt
		FROM artifacts WHERE namespace = ? AND task_name = ? ORDER BY filename ASC`, namespace, taskName)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	items := make([]store.ArtifactMetadata, 0)
	for rows.Next() {
		var item store.ArtifactMetadata
		var rowTaskUID string
		var rowAttempt int64
		if err := rows.Scan(&item.Filename, &item.ContentType, &item.Size, &item.CreatedAt, &rowTaskUID, &rowAttempt); err != nil {
			return nil, err
		}
		if rowTaskUID == "" || rowTaskUID != taskUID || rowAttempt != attempt {
			continue
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
