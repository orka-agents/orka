package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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

// withTaskDataTransaction runs a database-only operation in one write
// transaction that later task-data methods called with its context join. It
// is internal to the store: Orka never runs code inside it.
func (s *Store) withTaskDataTransaction(ctx context.Context, mutate func(context.Context) error) error {
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

// withTaskDataFence runs a fence-checking method. Without a fence in ctx it
// runs operation unchanged. With one, it checks the fence and runs operation
// in the same transaction, so cleanup cannot commit between them.
func (s *Store) withTaskDataFence(ctx context.Context, operation func(context.Context) error) error {
	fence, ok := store.TaskDataFenceFrom(ctx)
	if !ok {
		return operation(ctx)
	}
	if s.taskDataTx(ctx) != nil {
		if err := s.checkTaskDataFence(ctx, fence); err != nil {
			return err
		}
		return operation(ctx)
	}
	return s.withTaskDataTransaction(ctx, func(txCtx context.Context) error {
		if err := s.checkTaskDataFence(txCtx, fence); err != nil {
			return err
		}
		return operation(txCtx)
	})
}

// ResolveTaskDataFence returns the current fence without registering a Task.
func (s *Store) ResolveTaskDataFence(ctx context.Context, namespace, taskName string) (store.TaskDataFence, bool, error) {
	if err := validateTaskDataScope(namespace, taskName, false); err != nil {
		return store.TaskDataFence{}, false, err
	}
	fence := store.TaskDataFence{Namespace: namespace, TaskName: taskName}
	if taskName == "" {
		generation, err := s.taskDataCleanupGeneration(ctx, namespace, "")
		fence.Generation = generation
		return fence, false, err
	}
	var registered bool
	err := readInTransaction(ctx, s.db, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT generation FROM task_data_task_generations WHERE namespace = ? AND task_name = ?`, namespace, taskName,
		).Scan(&fence.Generation)
		if err == nil {
			registered = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return tx.QueryRowContext(ctx,
			`SELECT COALESCE((SELECT generation FROM task_data_cleanup_generations WHERE namespace = ?), 0)`, namespace,
		).Scan(&fence.Generation)
	})
	if err != nil {
		return store.TaskDataFence{}, false, err
	}
	return fence, registered, nil
}

// RegisterTaskDataFence registers a Task at the current namespace generation
// unless it is registered already.
func (s *Store) RegisterTaskDataFence(ctx context.Context, namespace, taskName string) (store.TaskDataFence, bool, error) {
	if err := validateTaskDataScope(namespace, taskName, true); err != nil {
		return store.TaskDataFence{}, false, err
	}
	fence := store.TaskDataFence{Namespace: namespace, TaskName: taskName}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO task_data_task_generations(namespace, task_name, generation)
		 VALUES (?, ?, COALESCE((SELECT generation FROM task_data_cleanup_generations WHERE namespace = ?), 0))
		 ON CONFLICT(namespace, task_name) DO NOTHING RETURNING generation`, namespace, taskName, namespace,
	).Scan(&fence.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		fence.Generation, err = s.taskDataCleanupGeneration(ctx, namespace, taskName)
		return fence, false, err
	}
	if err != nil {
		return store.TaskDataFence{}, false, err
	}
	return fence, true, nil
}

// ReleaseTaskDataFence removes a Task registration that still has the fence's
// generation. No data was written under a newer generation, so removing it
// does not need another cleanup generation; a later cleanup generation is
// preserved.
func (s *Store) ReleaseTaskDataFence(ctx context.Context, fence store.TaskDataFence) error {
	if err := validateTaskDataScope(fence.Namespace, fence.TaskName, true); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM task_data_task_generations WHERE namespace = ? AND task_name = ? AND generation = ?`,
		fence.Namespace, fence.TaskName, fence.Generation,
	)
	return err
}

// CheckTaskDataFence reports whether fence is still current.
func (s *Store) CheckTaskDataFence(ctx context.Context, fence store.TaskDataFence) error {
	return s.checkTaskDataFence(ctx, fence)
}

func (s *Store) checkTaskDataFence(ctx context.Context, fence store.TaskDataFence) error {
	if err := validateTaskDataScope(fence.Namespace, fence.TaskName, false); err != nil {
		return err
	}
	current, err := s.taskDataCleanupGeneration(ctx, fence.Namespace, fence.TaskName)
	if err != nil {
		return err
	}
	if current != fence.Generation {
		return store.ErrTaskDataCleanupChanged
	}
	for _, identity := range fence.Jobs {
		if err := s.checkTaskJobAuthority(ctx, identity); err != nil {
			return err
		}
	}
	return nil
}

func validateTaskDataScope(namespace, taskName string, requireTask bool) error {
	if strings.TrimSpace(namespace) == "" {
		return store.ValidationErrorf("task data fence requires a namespace")
	}
	if requireTask && strings.TrimSpace(taskName) == "" {
		return store.ValidationErrorf("task data registration requires a Task name")
	}
	return nil
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
