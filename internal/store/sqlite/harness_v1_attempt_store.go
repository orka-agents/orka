/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// CreateHarnessV1Attempt implements store.HarnessV1AttemptStore.
func (s *Store) CreateHarnessV1Attempt(ctx context.Context, attempt *store.HarnessV1Attempt) error {
	if err := attempt.Validate(); err != nil {
		return err
	}
	key := store.HarnessV1AttemptKey{Namespace: attempt.Namespace, TaskUID: attempt.TaskUID, Attempt: attempt.Attempt}

	existing, err := s.GetHarnessV1Attempt(ctx, key)
	switch {
	case err == nil:
		if existing.State == store.HarnessV1AttemptPrepared &&
			existing.BindingDigest == attempt.BindingDigest &&
			existing.SnapshotDigest == attempt.SnapshotDigest &&
			existing.RequestDigest == attempt.RequestDigest &&
			existing.TurnID == attempt.TurnID {
			return nil
		}
		return fmt.Errorf("%w: harness v1 attempt %s already exists with different content",
			store.ErrDuplicateMismatch, key.CanonicalID())
	case errors.Is(err, store.ErrNotFound):
	default:
		return err
	}

	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO harness_v1_attempts
		(id, namespace, task_name, task_uid, attempt, binding_digest, snapshot_digest, request_digest,
		 turn_id, runtime_session_id, correlation_id, backend, backend_endpoint,
		 auth_secret_name, auth_secret_key, auth_secret_uid, auth_secret_resource_version,
		 state, last_event_seq, terminal_receipt_digest, terminal_reason, duplicate_safe, retry_class,
		 controller_epoch_name, controller_epoch, last_operation_id, last_operation_digest,
		 version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', ?, ?, ?, ?, '', '', 1, ?, ?)
		ON CONFLICT(namespace, task_uid, attempt) DO NOTHING`,
		key.CanonicalID(), attempt.Namespace, attempt.TaskName, attempt.TaskUID, attempt.Attempt,
		attempt.BindingDigest, attempt.SnapshotDigest, attempt.RequestDigest,
		attempt.TurnID, attempt.RuntimeSessionID, attempt.CorrelationID, attempt.Backend, attempt.BackendEndpoint,
		attempt.AuthSecretName, attempt.AuthSecretKey, attempt.AuthSecretUID, attempt.AuthSecretResourceVersion,
		string(store.HarnessV1AttemptPrepared), attempt.DuplicateSafe, string(attempt.RetryClass),
		attempt.ControllerEpochName, attempt.ControllerEpoch, now, now)
	if err != nil {
		return fmt.Errorf("create harness v1 attempt: %w", err)
	}
	return nil
}

// GetHarnessV1Attempt implements store.HarnessV1AttemptStore.
func (s *Store) GetHarnessV1Attempt(ctx context.Context, key store.HarnessV1AttemptKey) (*store.HarnessV1Attempt, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	attempt, err := scanHarnessV1Attempt(s.db.QueryRowContext(ctx, harnessV1AttemptSelectSQL+`
		WHERE namespace = ? AND task_uid = ? AND attempt = ?`, key.Namespace, key.TaskUID, key.Attempt))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get harness v1 attempt: %w", err)
	}
	return attempt, nil
}

// ListHarnessV1AttemptsByTask implements store.HarnessV1AttemptStore.
func (s *Store) ListHarnessV1AttemptsByTask(ctx context.Context, namespace, taskUID string) ([]store.HarnessV1Attempt, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(taskUID) == "" {
		return nil, store.ValidationErrorf("harness v1 attempt namespace and task UID are required")
	}
	rows, err := s.db.QueryContext(ctx, harnessV1AttemptSelectSQL+`
		WHERE namespace = ? AND task_uid = ? ORDER BY attempt ASC`, namespace, taskUID)
	if err != nil {
		return nil, fmt.Errorf("list harness v1 attempts: %w", err)
	}
	return collectHarnessV1Attempts(rows)
}

// ListActiveHarnessV1Attempts implements store.HarnessV1AttemptStore.
func (s *Store) ListActiveHarnessV1Attempts(ctx context.Context) ([]store.HarnessV1Attempt, error) {
	rows, err := s.db.QueryContext(ctx, harnessV1AttemptSelectSQL+`
		WHERE state NOT IN ('Rejected','Succeeded','Failed','Cancelled','OutcomeUnknown')
		ORDER BY updated_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list active harness v1 attempts: %w", err)
	}
	return collectHarnessV1Attempts(rows)
}

// TransitionHarnessV1Attempt implements store.HarnessV1AttemptStore.
func (s *Store) TransitionHarnessV1Attempt(ctx context.Context, transition store.HarnessV1AttemptTransition) (*store.HarnessV1Attempt, error) {
	if err := transition.Key.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(transition.OperationID) == "" {
		return nil, store.ValidationErrorf("harness v1 attempt transition operation ID is required")
	}
	if err := store.ValidateCanonicalDigest("harness v1 attempt operation digest", transition.OperationDigest); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin harness v1 attempt transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := scanHarnessV1Attempt(tx.QueryRowContext(ctx, harnessV1AttemptSelectSQL+`
		WHERE namespace = ? AND task_uid = ? AND attempt = ?`,
		transition.Key.Namespace, transition.Key.TaskUID, transition.Key.Attempt))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read harness v1 attempt: %w", err)
	}

	// Idempotent replay of an already-applied operation.
	if current.LastOperationID == transition.OperationID {
		if current.LastOperationDigest != transition.OperationDigest || current.State != transition.TargetState {
			return nil, fmt.Errorf("%w: harness v1 attempt %s operation %s was applied with different content",
				store.ErrConflict, transition.Key.CanonicalID(), transition.OperationID)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit harness v1 attempt replay: %w", err)
		}
		return current, nil
	}

	if current.Version != transition.ExpectedVersion {
		return nil, fmt.Errorf("%w: harness v1 attempt %s version %d does not match expected %d",
			store.ErrConflict, transition.Key.CanonicalID(), current.Version, transition.ExpectedVersion)
	}
	if current.State != transition.ExpectedState {
		return nil, fmt.Errorf("%w: harness v1 attempt %s state %s does not match expected %s",
			store.ErrConflict, transition.Key.CanonicalID(), current.State, transition.ExpectedState)
	}
	if err := store.ValidateHarnessV1AttemptTransition(current.State, transition.TargetState); err != nil {
		return nil, err
	}

	updated := *current
	updated.State = transition.TargetState
	updated.Version = current.Version + 1
	updated.LastOperationID = transition.OperationID
	updated.LastOperationDigest = transition.OperationDigest
	if transition.Fence.Name != "" {
		updated.ControllerEpochName = transition.Fence.Name
		updated.ControllerEpoch = transition.Fence.Epoch
	}
	applyHarnessV1AttemptUpdates(&updated, transition.Updates)
	if transition.TargetState == store.HarnessV1AttemptOutcomeUnknown && updated.TerminalReason == "" {
		return nil, store.ValidationErrorf("harness v1 attempt OutcomeUnknown requires a terminal reason")
	}
	updated.UpdatedAt = time.Now().UTC()

	var cancelRequestedAt any
	if updated.CancelRequestedAt != nil {
		cancelRequestedAt = updated.CancelRequestedAt.UTC()
	}
	result, err := tx.ExecContext(ctx, `UPDATE harness_v1_attempts SET
			state = ?, runtime_session_id = ?, correlation_id = ?, backend_endpoint = ?,
			last_event_seq = ?, cancel_requested_at = ?, terminal_receipt_digest = ?, terminal_reason = ?,
			controller_epoch_name = ?, controller_epoch = ?, last_operation_id = ?, last_operation_digest = ?,
			version = ?, updated_at = ?
		WHERE namespace = ? AND task_uid = ? AND attempt = ? AND version = ?`,
		string(updated.State), updated.RuntimeSessionID, updated.CorrelationID, updated.BackendEndpoint,
		updated.LastEventSeq, cancelRequestedAt, updated.TerminalReceiptDigest, updated.TerminalReason,
		updated.ControllerEpochName, updated.ControllerEpoch, updated.LastOperationID, updated.LastOperationDigest,
		updated.Version, updated.UpdatedAt,
		transition.Key.Namespace, transition.Key.TaskUID, transition.Key.Attempt, current.Version)
	if err != nil {
		return nil, fmt.Errorf("transition harness v1 attempt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("transition harness v1 attempt rows: %w", err)
	}
	if affected != 1 {
		return nil, fmt.Errorf("%w: harness v1 attempt %s was concurrently modified",
			store.ErrConflict, transition.Key.CanonicalID())
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit harness v1 attempt transition: %w", err)
	}
	return &updated, nil
}

func applyHarnessV1AttemptUpdates(attempt *store.HarnessV1Attempt, updates store.HarnessV1AttemptUpdates) {
	if updates.RuntimeSessionID != nil {
		attempt.RuntimeSessionID = *updates.RuntimeSessionID
	}
	if updates.CorrelationID != nil {
		attempt.CorrelationID = *updates.CorrelationID
	}
	if updates.BackendEndpoint != nil {
		attempt.BackendEndpoint = *updates.BackendEndpoint
	}
	if updates.LastEventSeq != nil {
		attempt.LastEventSeq = *updates.LastEventSeq
	}
	if updates.CancelRequestedAt != nil {
		requestedAt := updates.CancelRequestedAt.UTC()
		attempt.CancelRequestedAt = &requestedAt
	}
	if updates.TerminalReceiptDigest != nil {
		attempt.TerminalReceiptDigest = *updates.TerminalReceiptDigest
	}
	if updates.TerminalReason != nil {
		attempt.TerminalReason = *updates.TerminalReason
	}
}

const harnessV1AttemptSelectSQL = `SELECT namespace, task_name, task_uid, attempt,
	binding_digest, snapshot_digest, request_digest, turn_id, runtime_session_id, correlation_id,
	backend, backend_endpoint, auth_secret_name, auth_secret_key, auth_secret_uid, auth_secret_resource_version,
	state, last_event_seq, cancel_requested_at, terminal_receipt_digest, terminal_reason,
	duplicate_safe, retry_class, controller_epoch_name, controller_epoch,
	last_operation_id, last_operation_digest, version, created_at, updated_at
	FROM harness_v1_attempts`

func scanHarnessV1Attempt(row sessionLineageScanner) (*store.HarnessV1Attempt, error) {
	var (
		attempt           store.HarnessV1Attempt
		state             string
		retryClass        string
		cancelRequestedAt sql.NullTime
	)
	if err := row.Scan(&attempt.Namespace, &attempt.TaskName, &attempt.TaskUID, &attempt.Attempt,
		&attempt.BindingDigest, &attempt.SnapshotDigest, &attempt.RequestDigest,
		&attempt.TurnID, &attempt.RuntimeSessionID, &attempt.CorrelationID,
		&attempt.Backend, &attempt.BackendEndpoint,
		&attempt.AuthSecretName, &attempt.AuthSecretKey, &attempt.AuthSecretUID, &attempt.AuthSecretResourceVersion,
		&state, &attempt.LastEventSeq, &cancelRequestedAt, &attempt.TerminalReceiptDigest, &attempt.TerminalReason,
		&attempt.DuplicateSafe, &retryClass, &attempt.ControllerEpochName, &attempt.ControllerEpoch,
		&attempt.LastOperationID, &attempt.LastOperationDigest,
		&attempt.Version, &attempt.CreatedAt, &attempt.UpdatedAt); err != nil {
		return nil, err
	}
	attempt.State = store.HarnessV1AttemptState(state)
	attempt.RetryClass = store.HarnessV1AttemptRetryClass(retryClass)
	if cancelRequestedAt.Valid {
		requestedAt := cancelRequestedAt.Time.UTC()
		attempt.CancelRequestedAt = &requestedAt
	}
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.UpdatedAt = attempt.UpdatedAt.UTC()
	return &attempt, nil
}

func collectHarnessV1Attempts(rows *sql.Rows) ([]store.HarnessV1Attempt, error) {
	defer func() { _ = rows.Close() }()
	var attempts []store.HarnessV1Attempt
	for rows.Next() {
		attempt, err := scanHarnessV1Attempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan harness v1 attempt: %w", err)
		}
		attempts = append(attempts, *attempt)
	}
	return attempts, rows.Err()
}
