/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Execution snapshots arrive sealed by the storage client. SQLite stores the
// nonce and ciphertext as opaque bytes and never holds the snapshot key.

var _ storagev1.SealedAgentExecutionSnapshotStore = (*Store)(nil)

// CreateSealedAgentExecutionSnapshot implements
// storagev1.SealedAgentExecutionSnapshotStore.
func (s *Store) CreateSealedAgentExecutionSnapshot(ctx context.Context, snapshot storagev1.SealedAgentExecutionSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO agent_execution_snapshots
		(task_uid, digest, schema_version, nonce, ciphertext, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_uid, digest) DO NOTHING`,
		snapshot.Key.TaskUID, snapshot.Key.Digest, snapshot.SchemaVersion, snapshot.Nonce, snapshot.Ciphertext, snapshot.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("persist agent execution snapshot: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("persist agent execution snapshot: %w", err)
	}
	if inserted == 0 {
		return store.ConflictErrorf("agent execution snapshot %s already exists", snapshot.Key.ID())
	}
	return nil
}

// GetSealedAgentExecutionSnapshot implements
// storagev1.SealedAgentExecutionSnapshotStore.
func (s *Store) GetSealedAgentExecutionSnapshot(ctx context.Context, key store.AgentExecutionSnapshotKey) (*storagev1.SealedAgentExecutionSnapshot, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	snapshot := storagev1.SealedAgentExecutionSnapshot{Key: key}
	err := s.db.QueryRowContext(ctx, `SELECT schema_version, nonce, ciphertext, created_at
		FROM agent_execution_snapshots WHERE task_uid = ? AND digest = ?`, key.TaskUID, key.Digest).
		Scan(&snapshot.SchemaVersion, &snapshot.Nonce, &snapshot.Ciphertext, &snapshot.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get agent execution snapshot: %w", err)
	}
	snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	return &snapshot, nil
}

// ListSealedAgentExecutionSnapshots implements
// storagev1.SealedAgentExecutionSnapshotStore.
func (s *Store) ListSealedAgentExecutionSnapshots(ctx context.Context, cursor string, limit int) (*storagev1.SealedAgentExecutionSnapshotPage, error) {
	if err := validateSealedPageLimit(limit); err != nil {
		return nil, err
	}
	afterTaskUID, afterDigest, err := decodeKeysetCursor(cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_uid, digest, schema_version, nonce, ciphertext, created_at
		FROM agent_execution_snapshots
		WHERE ? = '' OR task_uid > ? OR (task_uid = ? AND digest > ?)
		ORDER BY task_uid ASC, digest ASC
		LIMIT ?`, cursor, afterTaskUID, afterTaskUID, afterDigest, limit)
	if err != nil {
		return nil, fmt.Errorf("list sealed agent execution snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := &storagev1.SealedAgentExecutionSnapshotPage{Snapshots: make([]storagev1.SealedAgentExecutionSnapshot, 0)}
	for rows.Next() {
		var snapshot storagev1.SealedAgentExecutionSnapshot
		if err := rows.Scan(&snapshot.Key.TaskUID, &snapshot.Key.Digest, &snapshot.SchemaVersion,
			&snapshot.Nonce, &snapshot.Ciphertext, &snapshot.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan sealed agent execution snapshot: %w", err)
		}
		snapshot.CreatedAt = snapshot.CreatedAt.UTC()
		page.Snapshots = append(page.Snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sealed agent execution snapshots: %w", err)
	}
	if len(page.Snapshots) == limit {
		// Probe for a following record by key only, so a full page never
		// reads one more record's sealed bytes.
		last := page.Snapshots[limit-1].Key
		var more bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_execution_snapshots
			WHERE task_uid > ? OR (task_uid = ? AND digest > ?))`, last.TaskUID, last.TaskUID, last.Digest).Scan(&more); err != nil {
			return nil, fmt.Errorf("list sealed agent execution snapshots: %w", err)
		}
		if more {
			page.NextCursor = encodeKeysetCursor(last.TaskUID, last.Digest)
		}
	}
	return page, nil
}

func validateSealedPageLimit(limit int) error {
	if limit < 1 || limit > storagev1.MaxSealedRecordPageSize {
		return store.ValidationErrorf("page limit must be between 1 and %d", storagev1.MaxSealedRecordPageSize)
	}
	return nil
}

// encodeKeysetCursor and decodeKeysetCursor carry the last two-part key of a
// page. The encoding is opaque to callers.
func encodeKeysetCursor(first, second string) string {
	encoded, _ := json.Marshal([2]string{first, second})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeKeysetCursor(cursor string) (string, string, error) {
	if cursor == "" {
		return "", "", nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", store.ValidationErrorf("invalid cursor")
	}
	var key [2]string
	if err := json.Unmarshal(encoded, &key); err != nil || key[0] == "" {
		return "", "", store.ValidationErrorf("invalid cursor")
	}
	return key[0], key[1], nil
}

// ListAgentExecutionSnapshotMetadataBefore implements
// store.AgentExecutionSnapshotLifecycleStore. The cutoff is strict so a
// retention pass can use one stable timestamp without collecting records
// created exactly at that boundary.
func (s *Store) ListAgentExecutionSnapshotMetadataBefore(
	ctx context.Context,
	cutoff time.Time,
) ([]store.AgentExecutionSnapshotMetadata, error) {
	if cutoff.IsZero() {
		return nil, store.ValidationErrorf("snapshot metadata cutoff is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_uid, digest, schema_version, created_at
		FROM agent_execution_snapshots
		WHERE created_at < ?
		ORDER BY created_at ASC, task_uid ASC, digest ASC`, cutoff.UTC())
	if err != nil {
		return nil, fmt.Errorf("list agent execution snapshot metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()

	metadata := make([]store.AgentExecutionSnapshotMetadata, 0)
	for rows.Next() {
		var item store.AgentExecutionSnapshotMetadata
		if err := rows.Scan(&item.Key.TaskUID, &item.Key.Digest, &item.SchemaVersion, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan agent execution snapshot metadata: %w", err)
		}
		item.CreatedAt = item.CreatedAt.UTC()
		if err := item.Validate(); err != nil {
			return nil, fmt.Errorf("validate stored agent execution snapshot metadata: %w", err)
		}
		metadata = append(metadata, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent execution snapshot metadata: %w", err)
	}
	return metadata, nil
}

// CountAgentExecutionSnapshotReferences implements
// store.AgentExecutionSnapshotLifecycleStore. All counts are read by one SQL
// statement so the result is one consistent SQLite snapshot.
func (s *Store) CountAgentExecutionSnapshotReferences(
	ctx context.Context,
	key store.AgentExecutionSnapshotKey,
) (store.AgentExecutionSnapshotReferenceCounts, error) {
	if err := key.Validate(); err != nil {
		return store.AgentExecutionSnapshotReferenceCounts{}, err
	}
	var counts store.AgentExecutionSnapshotReferenceCounts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM harness_v1_attempts
			WHERE task_uid = ? AND snapshot_digest = ?),
		(SELECT COUNT(*) FROM prompt_attempts
			WHERE task_uid = ? AND snapshot_digest = ?),
		(SELECT COUNT(*) FROM session_turns
			WHERE task_uid = ?)`,
		key.TaskUID, key.Digest,
		key.TaskUID, key.Digest,
		key.TaskUID,
	).Scan(
		&counts.HarnessV1Attempts,
		&counts.PromptAttempts,
		&counts.SessionTurns,
	)
	if err != nil {
		return store.AgentExecutionSnapshotReferenceCounts{}, fmt.Errorf("count agent execution snapshot references: %w", err)
	}
	return counts, nil
}

// DeleteAgentExecutionSnapshot implements
// store.AgentExecutionSnapshotLifecycleStore. It deliberately does not alter
// the Task-scoped broad deletion method below or perform lifecycle policy.
func (s *Store) DeleteAgentExecutionSnapshot(ctx context.Context, key store.AgentExecutionSnapshotKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_execution_snapshots
		WHERE task_uid = ? AND digest = ?`, key.TaskUID, key.Digest); err != nil {
		return fmt.Errorf("delete agent execution snapshot: %w", err)
	}
	return nil
}

// DeleteAgentExecutionSnapshots implements
// storagev1.SealedAgentExecutionSnapshotStore.
func (s *Store) DeleteAgentExecutionSnapshots(ctx context.Context, taskUID string) error {
	if strings.TrimSpace(taskUID) == "" {
		return store.ValidationErrorf("snapshot task UID is required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_execution_snapshots WHERE task_uid = ?`, taskUID); err != nil {
		return fmt.Errorf("delete agent execution snapshots: %w", err)
	}
	return nil
}
