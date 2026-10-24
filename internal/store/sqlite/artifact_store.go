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

// SaveArtifact stores a Task artifact under its owner's UID. Data larger than
// store.MaxArtifactBytes is rejected with store.ErrTooLarge, a deleted owner
// UID is refused with a FencedError, and a write whose order stamp is not
// greater than the stored file's succeeds without changing it. The write
// replaces a file of the same filename stored for the Task name without a UID.
func (s *Store) SaveArtifact(ctx context.Context, request store.SaveArtifactRequest) error {
	return writeOperationErr(ctx, s, "SaveArtifact", func(ctx context.Context) error {
		return s.saveArtifactWrite(ctx, request)
	})
}

func (s *Store) saveArtifactWrite(ctx context.Context, request store.SaveArtifactRequest) error {
	owner := request.Owner
	if err := owner.Validate(); err != nil {
		return err
	}
	if len(request.Data) > store.MaxArtifactBytes {
		return store.TooLargeError{
			What:  fmt.Sprintf("artifact %q", request.Filename),
			Size:  int64(len(request.Data)),
			Limit: store.MaxArtifactBytes,
		}
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
	if err := requireOwnerLive(ctx, tx, artifactOwnerTombstones, "artifact", owner.Namespace, owner.TaskName, owner.TaskUID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artifacts (namespace, task_name, task_uid, filename, content_type, size, data, created_at, write_epoch, write_seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?)
		 ON CONFLICT(namespace, task_name, task_uid, filename) DO UPDATE SET
		   content_type = excluded.content_type, size = excluded.size, data = excluded.data,
		   created_at = excluded.created_at, write_epoch = excluded.write_epoch, write_seq = excluded.write_seq`+
			stamp.replaceCondition("artifacts"),
		owner.Namespace, owner.TaskName, owner.TaskUID, request.Filename, request.ContentType, len(request.Data), request.Data,
		stamp.epoch, stamp.sequence,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM artifacts WHERE namespace = ? AND task_name = ? AND task_uid = '' AND filename = ?`,
		owner.Namespace, owner.TaskName, request.Filename,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// GetArtifact retrieves artifact data and content type: the owner UID's file,
// or else the file of that filename stored for the Task name without a UID.
// It returns store.ErrNotFound if there is neither or the owner UID was
// deleted.
func (s *Store) GetArtifact(ctx context.Context, key store.ArtifactKey) ([]byte, string, error) {
	owner := key.Owner
	if err := owner.Validate(); err != nil {
		return nil, "", err
	}
	var data []byte
	var contentType string
	err := s.db.QueryRowContext(ctx,
		`SELECT data, content_type FROM artifacts
		 WHERE namespace = ? AND task_name = ? AND task_uid IN (?, '') AND filename = ?
		   AND NOT EXISTS (SELECT 1 FROM artifact_owner_tombstones t WHERE t.namespace = ? AND t.task_uid = ?)
		 ORDER BY task_uid DESC LIMIT 1`,
		owner.Namespace, owner.TaskName, owner.TaskUID, key.Filename, owner.Namespace, owner.TaskUID,
	).Scan(&data, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", store.ErrNotFound
	}
	return data, contentType, err
}

// ListArtifacts returns metadata for the owner UID's artifacts and for the
// files stored for the Task name without a UID whose filename the UID has not
// stored, ordered by filename in byte order (SQLite's default BINARY
// collation). A deleted owner UID has none.
func (s *Store) ListArtifacts(ctx context.Context, owner store.ArtifactOwner) ([]store.ArtifactMetadata, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.filename, a.content_type, a.size, a.created_at FROM artifacts a
		 WHERE a.namespace = ? AND a.task_name = ?
		   AND (a.task_uid = ? OR (a.task_uid = '' AND NOT EXISTS (
		     SELECT 1 FROM artifacts u
		     WHERE u.namespace = a.namespace AND u.task_name = a.task_name AND u.task_uid = ? AND u.filename = a.filename)))
		   AND NOT EXISTS (SELECT 1 FROM artifact_owner_tombstones t WHERE t.namespace = ? AND t.task_uid = ?)
		 ORDER BY a.filename`,
		owner.Namespace, owner.TaskName, owner.TaskUID, owner.TaskUID, owner.Namespace, owner.TaskUID,
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

// DeleteArtifacts removes the owner UID's artifacts and the files stored for
// the Task name without a UID, and records a tombstone that refuses later
// writes for the UID.
func (s *Store) DeleteArtifacts(ctx context.Context, owner store.ArtifactOwner) error {
	return writeOperationErr(ctx, s, "DeleteArtifacts", func(ctx context.Context) error {
		return s.deleteArtifactsWrite(ctx, owner)
	})
}

func (s *Store) deleteArtifactsWrite(ctx context.Context, owner store.ArtifactOwner) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := advanceTaskDataCleanupGeneration(ctx, tx.Tx, owner.Namespace, owner.TaskName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM artifacts WHERE namespace = ? AND (task_uid = ? OR (task_uid = '' AND task_name = ?))`,
		owner.Namespace, owner.TaskUID, owner.TaskName,
	); err != nil {
		return err
	}
	if err := s.recordOwnerDeleted(ctx, tx.Tx, artifactOwnerTombstones, owner.Namespace, owner.TaskName, owner.TaskUID); err != nil {
		return err
	}
	return tx.Commit()
}
