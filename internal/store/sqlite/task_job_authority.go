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

// DeleteTaskJobRevocations is the finalizer-only reclamation step. Advancing
// the Task and namespace cleanup generations atomically with removal prevents
// an in-flight authorization from treating a reclaimed revocation as active.
func (s *Store) DeleteTaskJobRevocations(ctx context.Context, namespace, taskName, taskUID string) error {
	if namespace == "" || taskName == "" || taskUID == "" {
		return store.ValidationErrorf("task identity is incomplete")
	}
	return s.deleteTaskData(ctx, namespace, taskName,
		`DELETE FROM task_job_revocations WHERE namespace = ? AND task_uid = ?`, namespace, taskUID,
	)
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
