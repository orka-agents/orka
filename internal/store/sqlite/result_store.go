package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

// SaveResult inserts or replaces a task result.
func (s *Store) SaveResult(ctx context.Context, namespace, taskName string, data []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO results (namespace, task_name, data, created_at, updated_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT(namespace, task_name) DO UPDATE SET data = excluded.data, updated_at = CURRENT_TIMESTAMP`,
		namespace, taskName, data,
	)
	return err
}

// GetResult retrieves a task result. Returns store.ErrNotFound if no result exists.
func (s *Store) GetResult(ctx context.Context, namespace, taskName string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM results WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return data, err
}

// DeleteResult removes a task result.
func (s *Store) DeleteResult(ctx context.Context, namespace, taskName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM prompt_result_receipts WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM results WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// SavePromptResultReceipt inserts one immutable attempt-bound result receipt.
func (s *Store) SavePromptResultReceipt(ctx context.Context, receipt store.PromptResultReceipt) error {
	receipt.AttemptID = strings.TrimSpace(receipt.AttemptID)
	receipt.Namespace = strings.TrimSpace(receipt.Namespace)
	receipt.TaskName = strings.TrimSpace(receipt.TaskName)
	receipt.OperationID = strings.TrimSpace(receipt.OperationID)
	if err := store.ValidateControlIdentifier("prompt result receipt attempt ID", receipt.AttemptID); err != nil {
		return err
	}
	if receipt.Namespace == "" || receipt.TaskName == "" {
		return store.ValidationErrorf("prompt result receipt namespace and task name are required")
	}
	if err := store.ValidateControlIdentifier("prompt result receipt operation ID", receipt.OperationID); err != nil {
		return err
	}
	if err := store.ValidateCanonicalDigest("prompt result receipt operation digest", receipt.OperationDigest); err != nil {
		return err
	}
	if receipt.Data == nil {
		return store.ValidationErrorf("prompt result receipt data is required")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO prompt_result_receipts
		 (attempt_id, namespace, task_name, operation_id, operation_digest, data)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(attempt_id) DO NOTHING`,
		receipt.AttemptID, receipt.Namespace, receipt.TaskName,
		receipt.OperationID, receipt.OperationDigest, receipt.Data,
	); err != nil {
		return err
	}
	persisted, err := s.GetPromptResultReceipt(ctx, receipt.AttemptID)
	if err != nil {
		return err
	}
	if persisted.Namespace != receipt.Namespace || persisted.TaskName != receipt.TaskName ||
		persisted.OperationID != receipt.OperationID || persisted.OperationDigest != receipt.OperationDigest ||
		!bytes.Equal(persisted.Data, receipt.Data) {
		return fmt.Errorf("%w: prompt result receipt %q was reused with different data", store.ErrDuplicateMismatch, receipt.AttemptID)
	}
	return nil
}

// GetPromptResultReceipt returns the immutable receipt for one PromptAttempt.
func (s *Store) GetPromptResultReceipt(ctx context.Context, attemptID string) (*store.PromptResultReceipt, error) {
	attemptID = strings.TrimSpace(attemptID)
	if err := store.ValidateControlIdentifier("prompt result receipt attempt ID", attemptID); err != nil {
		return nil, err
	}
	receipt := &store.PromptResultReceipt{AttemptID: attemptID}
	err := s.db.QueryRowContext(ctx,
		`SELECT namespace, task_name, operation_id, operation_digest, data
		 FROM prompt_result_receipts WHERE attempt_id = ?`,
		attemptID,
	).Scan(&receipt.Namespace, &receipt.TaskName, &receipt.OperationID, &receipt.OperationDigest, &receipt.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
