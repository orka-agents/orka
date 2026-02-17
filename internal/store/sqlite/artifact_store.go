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

	"github.com/sozercan/orka/internal/store"
)

const maxArtifactSize = 10 << 20 // 10MB

// SaveArtifact inserts or replaces a task artifact. Returns an error if data exceeds 10MB.
func (s *Store) SaveArtifact(ctx context.Context, namespace, taskName, filename, contentType string, data []byte) error {
	if len(data) > maxArtifactSize {
		return fmt.Errorf("artifact %q exceeds maximum size of 10MB", filename)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO artifacts (namespace, task_name, filename, content_type, size, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		namespace, taskName, filename, contentType, len(data), data,
	)
	return err
}

// GetArtifact retrieves artifact data and content type. Returns store.ErrNotFound if not found.
func (s *Store) GetArtifact(ctx context.Context, namespace, taskName, filename string) ([]byte, string, error) {
	var data []byte
	var contentType string
	err := s.db.QueryRowContext(ctx,
		`SELECT data, content_type FROM artifacts WHERE namespace = ? AND task_name = ? AND filename = ?`,
		namespace, taskName, filename,
	).Scan(&data, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", store.ErrNotFound
	}
	return data, contentType, err
}

// ListArtifacts returns metadata for all artifacts belonging to a task.
func (s *Store) ListArtifacts(ctx context.Context, namespace, taskName string) ([]store.ArtifactMetadata, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT filename, content_type, size, created_at FROM artifacts WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var artifacts []store.ArtifactMetadata
	for rows.Next() {
		var m store.ArtifactMetadata
		if err := rows.Scan(&m.Filename, &m.ContentType, &m.Size, &m.CreatedAt); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, m)
	}
	return artifacts, rows.Err()
}

// DeleteArtifacts removes all artifacts for a task.
func (s *Store) DeleteArtifacts(ctx context.Context, namespace, taskName string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM artifacts WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	)
	return err
}
