package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
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

// WithTaskDataTransaction serializes a database-only callback with cleanup.
// Network authorization belongs in WithAuthorizedTaskDataTransaction.
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

// WithAuthorizedTaskDataTransaction fences authorization using a durable
// cleanup generation. Kubernetes latency does not reserve a SQLite connection
// or writer. Only cleanup, rather than unrelated writes, invalidates the proof.
func (s *Store) WithAuthorizedTaskDataTransaction(ctx context.Context, namespace, taskName string, authorize, access func(context.Context) error) (accessErr error) {
	generation, registered, err := s.prepareTaskDataGeneration(ctx, namespace, taskName, authorize)
	if err != nil {
		return err
	}
	if registered {
		defer func() {
			if accessErr == nil {
				return
			}
			// Failed or cancelled requests must not leave provisional rows behind.
			// No data changed, so removing this unchanged registration does not
			// need another cleanup epoch. Preserve any later cleanup generation.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, err := s.db.ExecContext(cleanupCtx,
				`DELETE FROM task_data_task_generations WHERE namespace = ? AND task_name = ? AND generation = ?`,
				namespace, taskName, generation,
			)
			accessErr = errors.Join(accessErr, err)
		}()
	}
	if err := authorize(ctx); err != nil {
		return err
	}
	return s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		current, err := s.taskDataCleanupGeneration(txCtx, namespace, taskName)
		if err != nil {
			return err
		}
		if current != generation {
			return store.ErrTaskDataCleanupChanged
		}
		if taskName != "" {
			// Register a Task only after successful live authorization. Its row
			// avoids retries on unrelated cleanup and is reclaimed by the finalizer.
			if _, err := s.taskDataExecutor(txCtx).ExecContext(txCtx,
				`INSERT INTO task_data_task_generations(namespace, task_name, generation) VALUES (?, ?, ?)
				 ON CONFLICT(namespace, task_name) DO NOTHING`, namespace, taskName, current,
			); err != nil {
				return err
			}
		}
		return access(txCtx)
	})
}

func (s *Store) prepareTaskDataGeneration(ctx context.Context, namespace, taskName string, authorize func(context.Context) error) (int64, bool, error) {
	if taskName == "" {
		generation, err := s.taskDataCleanupGeneration(ctx, namespace, taskName)
		return generation, false, err
	}
	var generation int64
	err := s.db.QueryRowContext(ctx,
		`SELECT generation FROM task_data_task_generations WHERE namespace = ? AND task_name = ?`, namespace, taskName,
	).Scan(&generation)
	if !errors.Is(err, sql.ErrNoRows) {
		return generation, false, err
	}
	// Authenticate before allocating a row, then discard this proof. A fresh
	// authorization after registration gets a stable Task fence even when
	// unrelated cleanup happens during every Kubernetes lookup.
	if err := authorize(ctx); err != nil {
		return 0, false, err
	}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO task_data_task_generations(namespace, task_name, generation)
		 VALUES (?, ?, COALESCE((SELECT generation FROM task_data_cleanup_generations WHERE namespace = ?), 0))
		 ON CONFLICT(namespace, task_name) DO NOTHING RETURNING generation`, namespace, taskName, namespace,
	).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		generation, err = s.taskDataCleanupGeneration(ctx, namespace, taskName)
		return generation, false, err
	}
	return generation, err == nil, err
}

func (s *Store) taskDataCleanupGeneration(ctx context.Context, namespace, taskName string) (int64, error) {
	var generation int64
	if taskName != "" {
		// Missing rows inherit the namespace epoch instead of resetting to zero.
		// Reclaiming and recreating a Task row cannot repeat an old generation.
		err := s.taskDataExecutor(ctx).QueryRowContext(ctx,
			`SELECT COALESCE(
			 (SELECT generation FROM task_data_task_generations WHERE namespace = ? AND task_name = ?),
			 (SELECT generation FROM task_data_cleanup_generations WHERE namespace = ?), 0)`,
			namespace, taskName, namespace,
		).Scan(&generation)
		return generation, err
	}
	err := s.taskDataExecutor(ctx).QueryRowContext(ctx,
		`SELECT COALESCE((SELECT generation FROM task_data_cleanup_generations WHERE namespace = ?), 0)`, namespace,
	).Scan(&generation)
	return generation, err
}

func advanceTaskDataCleanupGeneration(ctx context.Context, tx *sql.Tx, namespace string, taskNames ...string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO task_data_cleanup_generations(namespace, generation) VALUES (?, 1)
		 ON CONFLICT(namespace) DO UPDATE SET generation = generation + 1`, namespace,
	)
	if err != nil {
		return err
	}
	for _, taskName := range taskNames {
		if taskName == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_data_task_generations(namespace, task_name, generation)
			 SELECT namespace, ?, generation FROM task_data_cleanup_generations WHERE namespace = ?
			 ON CONFLICT(namespace, task_name) DO UPDATE SET generation = excluded.generation`, taskName, namespace,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deleteTaskData(ctx context.Context, namespace, taskName, statement string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := advanceTaskDataCleanupGeneration(ctx, tx, namespace, taskName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
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
