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

// SaveResult stores a Task result under its UID. Data larger than
// store.MaxResultBytes is rejected with store.ErrTooLarge, a deleted UID is
// refused with a FencedError, and a write whose order stamp is not greater
// than the stored result's succeeds without changing it. The write replaces a
// result stored for the Task name without a UID.
func (s *Store) SaveResult(ctx context.Context, key store.ResultKey, data []byte) error {
	return writeOperationErr(ctx, s, "SaveResult", func(ctx context.Context) error {
		return s.saveResultWrite(ctx, key, data)
	})
}

func (s *Store) saveResultWrite(ctx context.Context, key store.ResultKey, data []byte) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if len(data) > store.MaxResultBytes {
		return store.TooLargeError{What: "result", Size: int64(len(data)), Limit: store.MaxResultBytes}
	}
	stamp, err := writeStampFrom(ctx)
	if err != nil {
		return err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireOwnerLive(ctx, tx, resultOwnerTombstones, "result", key.Namespace, key.TaskName, key.TaskUID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO results (namespace, task_name, task_uid, data, created_at, updated_at, write_epoch, write_seq)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, ?)
		 ON CONFLICT(namespace, task_name, task_uid) DO UPDATE SET
		   data = excluded.data, updated_at = excluded.updated_at,
		   write_epoch = excluded.write_epoch, write_seq = excluded.write_seq`+
			stamp.replaceCondition("results"),
		key.Namespace, key.TaskName, key.TaskUID, data, stamp.epoch, stamp.sequence,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM results WHERE namespace = ? AND task_name = ? AND task_uid = ''`,
		key.Namespace, key.TaskName,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// GetResult retrieves the result of the key's UID, or else the result stored
// for the Task name without a UID. It returns store.ErrNotFound if there is
// neither or the UID was deleted.
func (s *Store) GetResult(ctx context.Context, key store.ResultKey) ([]byte, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	var data []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM results
		 WHERE namespace = ? AND task_name = ? AND task_uid IN (?, '')
		   AND NOT EXISTS (SELECT 1 FROM result_owner_tombstones t WHERE t.namespace = ? AND t.task_uid = ?)
		 ORDER BY task_uid DESC LIMIT 1`,
		key.Namespace, key.TaskName, key.TaskUID, key.Namespace, key.TaskUID,
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return data, err
}

// DeleteResult removes the result and prompt result receipts of the key's
// UID, and those stored for the Task name without a UID, and records a
// tombstone that refuses later writes for the UID.
func (s *Store) DeleteResult(ctx context.Context, key store.ResultKey) error {
	return writeOperationErr(ctx, s, "DeleteResult", func(ctx context.Context) error {
		return s.deleteResultWrite(ctx, key)
	})
}

func (s *Store) deleteResultWrite(ctx context.Context, key store.ResultKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := advanceTaskDataCleanupGeneration(ctx, tx.Tx, key.Namespace, key.TaskName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM prompt_result_receipts WHERE namespace = ? AND (task_uid = ? OR (task_uid = '' AND task_name = ?))`,
		key.Namespace, key.TaskUID, key.TaskName,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM results WHERE namespace = ? AND (task_uid = ? OR (task_uid = '' AND task_name = ?))`,
		key.Namespace, key.TaskUID, key.TaskName,
	); err != nil {
		return err
	}
	if err := s.recordOwnerDeleted(ctx, tx.Tx, resultOwnerTombstones, key.Namespace, key.TaskName, key.TaskUID); err != nil {
		return err
	}
	return tx.Commit()
}

// SavePromptResultReceipt inserts one immutable attempt-bound result receipt.
func (s *Store) SavePromptResultReceipt(ctx context.Context, receipt store.PromptResultReceipt) error {
	return writeOperationErr(ctx, s, "SavePromptResultReceipt", func(ctx context.Context) error {
		return s.savePromptResultReceiptWrite(ctx, receipt)
	})
}

func (s *Store) savePromptResultReceiptWrite(ctx context.Context, receipt store.PromptResultReceipt) error {
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
	if err := store.ValidateControlIdentifier("prompt result receipt Task UID", receipt.TaskUID); err != nil {
		return err
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
	if len(receipt.Data) > store.MaxResultBytes {
		return store.TooLargeError{What: "prompt result receipt data", Size: int64(len(receipt.Data)), Limit: store.MaxResultBytes}
	}
	// The tombstone check, the insert and the comparison share one
	// transaction, so the receipt compared is the one stored when this call
	// committed.
	return s.writeTransaction(ctx, func(q taskDataExecutor) error {
		if err := requireOwnerLive(ctx, q, resultOwnerTombstones, "result", receipt.Namespace, receipt.TaskName, receipt.TaskUID); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO prompt_result_receipts
		 (attempt_id, namespace, task_name, task_uid, operation_id, operation_digest, data)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(attempt_id) DO NOTHING`,
			receipt.AttemptID, receipt.Namespace, receipt.TaskName, receipt.TaskUID,
			receipt.OperationID, receipt.OperationDigest, receipt.Data,
		); err != nil {
			return err
		}
		persisted, err := getPromptResultReceipt(ctx, q, receipt.AttemptID)
		if err != nil {
			return err
		}
		// A receipt saved before receipts recorded a UID belongs to the Task
		// that holds its name, so a repeat with that name and a UID matches it.
		sameTask := persisted.TaskUID == receipt.TaskUID || persisted.TaskUID == ""
		if persisted.Namespace != receipt.Namespace || persisted.TaskName != receipt.TaskName || !sameTask ||
			persisted.OperationID != receipt.OperationID || persisted.OperationDigest != receipt.OperationDigest ||
			!bytes.Equal(persisted.Data, receipt.Data) {
			return fmt.Errorf("%w: prompt result receipt %q was reused with different data", store.ErrDuplicateMismatch, receipt.AttemptID)
		}
		return nil
	})
}

// GetPromptResultReceipt returns the immutable receipt for one PromptAttempt.
func (s *Store) GetPromptResultReceipt(ctx context.Context, attemptID string) (*store.PromptResultReceipt, error) {
	attemptID = strings.TrimSpace(attemptID)
	if err := store.ValidateControlIdentifier("prompt result receipt attempt ID", attemptID); err != nil {
		return nil, err
	}
	return getPromptResultReceipt(ctx, s.taskDataExecutor(ctx), attemptID)
}

func getPromptResultReceipt(ctx context.Context, q queryRower, attemptID string) (*store.PromptResultReceipt, error) {
	receipt := &store.PromptResultReceipt{AttemptID: attemptID}
	err := q.QueryRowContext(ctx,
		`SELECT namespace, task_name, task_uid, operation_id, operation_digest, data
		 FROM prompt_result_receipts WHERE attempt_id = ?`,
		attemptID,
	).Scan(&receipt.Namespace, &receipt.TaskName, &receipt.TaskUID, &receipt.OperationID, &receipt.OperationDigest, &receipt.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
