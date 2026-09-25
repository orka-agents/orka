package sqlite

import (
	"context"

	"github.com/orka-agents/orka/internal/store"
)

func (s *Store) RevokeTaskJob(ctx context.Context, identity store.TaskJobIdentity) error {
	if identity.Namespace == "" || identity.TaskUID == "" || identity.JobUID == "" {
		return store.ValidationErrorf("task Job identity is incomplete")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_job_revocations(namespace, task_uid, job_uid) VALUES (?, ?, ?)
		 ON CONFLICT(namespace, task_uid, job_uid) DO NOTHING`,
		identity.Namespace, identity.TaskUID, identity.JobUID,
	)
	return err
}

// DeleteTaskJobRevocations is the finalizer-only reclamation step. A fresh
// namespace generation replaces the removed Task generation, so in-flight
// authorization cannot reuse its old generation after reclamation or name reuse.
func (s *Store) DeleteTaskJobRevocations(ctx context.Context, namespace, taskName, taskUID string) error {
	if namespace == "" || taskName == "" || taskUID == "" {
		return store.ValidationErrorf("task identity is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := advanceTaskDataCleanupGeneration(ctx, tx, namespace); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_job_revocations WHERE namespace = ? AND task_uid = ?`, namespace, taskUID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_data_task_generations WHERE namespace = ? AND task_name = ?`, namespace, taskName); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CheckTaskJobAuthority(ctx context.Context, identity store.TaskJobIdentity) error {
	if identity.Namespace == "" || identity.TaskUID == "" || identity.JobUID == "" {
		return store.ValidationErrorf("task Job identity is incomplete")
	}
	var revoked bool
	if err := s.taskDataExecutor(ctx).QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM task_job_revocations WHERE namespace = ? AND task_uid = ? AND job_uid = ?)`,
		identity.Namespace, identity.TaskUID, identity.JobUID,
	).Scan(&revoked); err != nil {
		return err
	}
	if revoked {
		return store.ErrTaskJobRevoked
	}
	return nil
}
