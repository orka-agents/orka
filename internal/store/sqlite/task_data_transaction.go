package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

type taskDataTransactionKey struct{}

type taskDataTransaction struct {
	db *sql.DB
	tx *sql.Tx
}

type taskDataExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// WithTaskDataTransaction acquires SQLite's writer lock before the callback
// checks Kubernetes authority. Task finalizer cleanup uses the same database,
// so it either precedes that check (which rejects a deleting/recreated Task) or
// follows the committed write and removes it before permitting name reuse.
func (s *Store) WithTaskDataTransaction(ctx context.Context, mutate func(context.Context) error) error {
	if ctx.Value(taskDataTransactionKey{}) != nil {
		return fmt.Errorf("nested task data transaction")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// BeginTx is deferred in SQLite. A write statement reserves the writer even
	// when its predicate matches no rows; no task data is changed here.
	if _, err := tx.ExecContext(ctx, `UPDATE results SET updated_at = updated_at WHERE 0`); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, taskDataTransactionKey{}, taskDataTransaction{db: s.db, tx: tx})
	if err := mutate(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) taskDataExecutor(ctx context.Context) taskDataExecutor {
	if tx := s.taskDataTx(ctx); tx != nil {
		return tx
	}
	return s.db
}

func (s *Store) taskDataTx(ctx context.Context) *sql.Tx {
	if transaction, ok := ctx.Value(taskDataTransactionKey{}).(taskDataTransaction); ok && transaction.db == s.db {
		return transaction.tx
	}
	return nil
}
