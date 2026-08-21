package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/orka-agents/orka/internal/store"
)

// SaveResult inserts or replaces a task result.
func (s *Store) SaveResult(ctx context.Context, namespace, taskName string, data []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO results (namespace, task_name, data, staging_generation, content_size, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT(namespace, task_name) DO UPDATE SET
		   data = excluded.data,
		   task_uid = '', job_uid = '', pod_uid = '', task_attempt = 0,
		   producer_kind = 'legacy-unverified', runtime_session_id = '', turn_id = '', correlation_id = '',
		   submission_nonce_digest = '', staging_generation = results.staging_generation + 1,
		   content_size = excluded.content_size, content_sha256 = '',
		   accepted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP`,
		namespace, taskName, data, len(data),
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
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM results WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	)
	return err
}
