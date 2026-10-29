package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Results and artifacts are bound to the Task UID that wrote them. Rows
// written before schema version 3 have an empty task_uid and order stamp
// (0, 0); they belong to whichever Task holds their name until its first
// delete.

const (
	artifactOwnerTombstones = "artifact_owner_tombstones"
	resultOwnerTombstones   = "result_owner_tombstones"
	// ownerTombstoneRetention is how long a deleted Task UID stays refused
	// after its last delete. It equals the receipt retention, which outlives
	// every write the store can still accept that was issued before the
	// delete.
	ownerTombstoneRetention = receiptRetention
	// ownerTombstonePruneBatch bounds one pruning statement of the
	// maintenance loop.
	ownerTombstonePruneBatch = 1000
)

// taskOwnerSchemaStatements defines the deletion tombstones of result and
// artifact owners, and the indexes that address results and artifacts by
// Task UID. Their layout is frozen in migrations/169-to-170.sql.
func taskOwnerSchemaStatements() []string {
	return []string{
		`CREATE INDEX IF NOT EXISTS idx_results_task_uid ON results(namespace, task_uid)`,
		`CREATE INDEX IF NOT EXISTS idx_artifacts_task_uid ON artifacts(namespace, task_uid)`,
		`CREATE TABLE IF NOT EXISTS result_owner_tombstones (
 namespace TEXT NOT NULL,
 task_uid TEXT NOT NULL,
 task_name TEXT NOT NULL,
 deleted_at_ns INTEGER NOT NULL,
 PRIMARY KEY (namespace, task_uid)
)`,
		`CREATE INDEX IF NOT EXISTS idx_result_owner_tombstones_deleted_at
 ON result_owner_tombstones(deleted_at_ns)`,
		`CREATE TABLE IF NOT EXISTS artifact_owner_tombstones (
 namespace TEXT NOT NULL,
 task_uid TEXT NOT NULL,
 task_name TEXT NOT NULL,
 deleted_at_ns INTEGER NOT NULL,
 PRIMARY KEY (namespace, task_uid)
)`,
		`CREATE INDEX IF NOT EXISTS idx_artifact_owner_tombstones_deleted_at
 ON artifact_owner_tombstones(deleted_at_ns)`,
	}
}

// writeStamp is the order stamp of a whole-object replacement: the epoch and
// sequence of its write envelope. A write without an envelope, which only
// package tests make, is unstamped and replaces unconditionally.
type writeStamp struct {
	epoch    int64
	sequence int64
	stamped  bool
}

// writeStampFrom returns the order stamp of the write ctx carries.
func writeStampFrom(ctx context.Context) (writeStamp, error) {
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		return writeStamp{}, nil
	}
	if envelope.Sequence > math.MaxInt64 {
		return writeStamp{}, store.ValidationErrorf("write envelope sequence %d exceeds %d", envelope.Sequence, int64(math.MaxInt64))
	}
	return writeStamp{epoch: envelope.Fence.Epoch, sequence: int64(envelope.Sequence), stamped: true}, nil
}

// replaceCondition is the ON CONFLICT DO UPDATE condition that applies a
// replacement of a row in table only when the write's stamp is greater than
// the stored one. An unstamped write always applies.
func (w writeStamp) replaceCondition(table string) string {
	if !w.stamped {
		return ""
	}
	return fmt.Sprintf(" WHERE (%[1]s.write_epoch, %[1]s.write_seq) < (excluded.write_epoch, excluded.write_seq)", table)
}

// requireOwnerLive refuses a write for a Task UID whose results or artifacts
// were deleted.
func requireOwnerLive(ctx context.Context, q queryRower, tombstones, what, namespace, taskName, taskUID string) error {
	var deleted int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM `+tombstones+` WHERE namespace = ? AND task_uid = ?`, namespace, taskUID,
	).Scan(&deleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	default:
		return store.FencedErrorf("%s owner %s/%s (UID %s) was deleted", what, namespace, taskName, taskUID)
	}
}

// recordOwnerDeleted records, or refreshes, the deletion tombstone of a Task
// UID at the provider clock.
func (s *Store) recordOwnerDeleted(ctx context.Context, tx *sql.Tx, tombstones, namespace, taskName, taskUID string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO `+tombstones+` (namespace, task_uid, task_name, deleted_at_ns) VALUES (?, ?, ?, ?)
		 ON CONFLICT(namespace, task_uid) DO UPDATE SET deleted_at_ns = MAX(deleted_at_ns, excluded.deleted_at_ns)`,
		namespace, taskUID, taskName, s.now().UnixNano(),
	)
	return err
}

// pruneExpiredOwnerTombstones removes, in bounded batches, every result and
// artifact owner tombstone past its retention.
func (s *Store) pruneExpiredOwnerTombstones(ctx context.Context) error {
	cutoff := s.now().Add(-ownerTombstoneRetention).UnixNano()
	for _, table := range []string{resultOwnerTombstones, artifactOwnerTombstones} {
		for {
			result, err := s.db.ExecContext(ctx,
				`DELETE FROM `+table+` WHERE rowid IN (
				   SELECT rowid FROM `+table+` WHERE deleted_at_ns < ? ORDER BY deleted_at_ns LIMIT ?
				 )`,
				cutoff, ownerTombstonePruneBatch,
			)
			if err != nil {
				return fmt.Errorf("prune %s: %w", table, err)
			}
			removed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if removed < ownerTombstonePruneBatch {
				break
			}
		}
	}
	return nil
}
