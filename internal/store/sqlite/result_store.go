package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sozercan/mercan/internal/store"
)

// SaveResult inserts or replaces a task result.
func (s *Store) SaveResult(ctx context.Context, namespace, taskName string, data []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO results (namespace, task_name, data, created_at, updated_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
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
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM results WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	)
	return err
}
