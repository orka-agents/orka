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

	"github.com/orka-agents/orka/internal/store"
)

// SaveArtifact inserts or replaces a task artifact. Data larger than
// store.MaxArtifactBytes is rejected with store.ErrTooLarge, and a write whose
// order stamp is not greater than the stored file's succeeds without changing
// it. The Task UID is not stored yet; artifacts are keyed by namespace, Task
// name and filename.
func (s *Store) SaveArtifact(ctx context.Context, request store.SaveArtifactRequest) error {
	return writeOperationErr(ctx, s, "SaveArtifact", func(ctx context.Context) error {
		return s.saveArtifactWrite(ctx, request)
	})
}

func (s *Store) saveArtifactWrite(ctx context.Context, request store.SaveArtifactRequest) error {
	stamp, err := writeStampFrom(ctx)
	if err != nil {
		return err
	}
	return s.withTaskDataFence(ctx, func(ctx context.Context) error {
		return s.saveArtifactUnfenced(ctx, request, stamp)
	})
}

func (s *Store) saveArtifactUnfenced(ctx context.Context, request store.SaveArtifactRequest, stamp writeStamp) error {
	if len(request.Data) > store.MaxArtifactBytes {
		return store.TooLargeError{
			What:  fmt.Sprintf("artifact %q", request.Filename),
			Size:  int64(len(request.Data)),
			Limit: store.MaxArtifactBytes,
		}
	}
	_, err := s.taskDataExecutor(ctx).ExecContext(ctx,
		`INSERT INTO artifacts (namespace, task_name, filename, content_type, size, data, created_at, write_epoch, write_seq)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?)
		 ON CONFLICT(namespace, task_name, filename) DO UPDATE SET
		   content_type = excluded.content_type, size = excluded.size, data = excluded.data,
		   created_at = excluded.created_at, write_epoch = excluded.write_epoch, write_seq = excluded.write_seq`+
			stamp.replaceCondition("artifacts"),
		request.Owner.Namespace, request.Owner.TaskName, request.Filename, request.ContentType, len(request.Data), request.Data,
		stamp.epoch, stamp.sequence,
	)
	return err
}

// GetArtifact retrieves artifact data and content type. Returns store.ErrNotFound if not found.
func (s *Store) GetArtifact(ctx context.Context, key store.ArtifactKey) ([]byte, string, error) {
	var data []byte
	var contentType string
	err := s.db.QueryRowContext(ctx,
		`SELECT data, content_type FROM artifacts WHERE namespace = ? AND task_name = ? AND filename = ?`,
		key.Owner.Namespace, key.Owner.TaskName, key.Filename,
	).Scan(&data, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", store.ErrNotFound
	}
	return data, contentType, err
}

// ListArtifacts returns metadata for all artifacts belonging to a task, ordered
// by filename in byte order (SQLite's default BINARY collation).
func (s *Store) ListArtifacts(ctx context.Context, owner store.ArtifactOwner) ([]store.ArtifactMetadata, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT filename, content_type, size, created_at FROM artifacts WHERE namespace = ? AND task_name = ? ORDER BY filename`,
		owner.Namespace, owner.TaskName,
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
		m.CreatedAt = m.CreatedAt.UTC()
		artifacts = append(artifacts, m)
	}
	return artifacts, rows.Err()
}

// DeleteArtifacts removes all artifacts for a task.
func (s *Store) DeleteArtifacts(ctx context.Context, owner store.ArtifactOwner) error {
	return writeOperationErr(ctx, s, "DeleteArtifacts", func(ctx context.Context) error {
		return s.deleteArtifactsWrite(ctx, owner)
	})
}

func (s *Store) deleteArtifactsWrite(ctx context.Context, owner store.ArtifactOwner) error {
	return s.deleteTaskData(ctx, owner.Namespace, owner.TaskName,
		`DELETE FROM artifacts WHERE namespace = ? AND task_name = ?`,
		owner.Namespace, owner.TaskName,
	)
}
