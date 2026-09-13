package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

var _ store.GatewayTaskCleanupReceiptStore = (*Store)(nil)

func gatewayTaskCleanupSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS gateway_task_cleanup_receipts (
			namespace     TEXT NOT NULL,
			namespace_uid TEXT NOT NULL,
			gateway_name  TEXT NOT NULL,
			gateway_uid   TEXT NOT NULL,
			binding_name  TEXT NOT NULL,
			binding_uid   TEXT NOT NULL,
			event_id      TEXT NOT NULL,
			task_name     TEXT NOT NULL,
			task_uid      TEXT NOT NULL,
			session_name  TEXT NOT NULL,
			compacted_at  TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, task_uid)
		)`,
	}
}

// GetGatewayTaskCleanupReceipt does not consult bounded deduplication history or
// current Gateway/Binding objects. The original Task UID remains authoritative
// when their names, or an external event ID, are reused.
func (s *Store) GetGatewayTaskCleanupReceipt(ctx context.Context, namespace, taskName, taskUID string) (*store.GatewayTaskCleanupReceipt, error) {
	for field, value := range map[string]string{"Task namespace": namespace, "Task name": taskName, "Task UID": taskUID} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return nil, err
		}
	}
	return getGatewayTaskCleanupReceiptQuery(ctx, s.db, namespace, taskName, taskUID)
}

func getGatewayTaskCleanupReceiptQuery(ctx context.Context, q queryRower, namespace, taskName, taskUID string) (*store.GatewayTaskCleanupReceipt, error) {
	receipt := &store.GatewayTaskCleanupReceipt{}
	err := q.QueryRowContext(ctx, `SELECT namespace, namespace_uid, gateway_name, gateway_uid,
		binding_name, binding_uid, event_id, task_name, task_uid, session_name, compacted_at
		FROM gateway_task_cleanup_receipts WHERE namespace = ? AND task_uid = ?`, namespace, taskUID).Scan(
		&receipt.Namespace, &receipt.NamespaceUID, &receipt.GatewayName, &receipt.GatewayUID,
		&receipt.BindingName, &receipt.BindingUID, &receipt.EventID, &receipt.TaskName, &receipt.TaskUID,
		&receipt.SessionName, &receipt.CompactedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read Gateway Task cleanup receipt: %w", err)
	}
	receipt.CompactedAt = receipt.CompactedAt.UTC()
	if err := receipt.Validate(namespace, taskName, taskUID); err != nil {
		return nil, err
	}
	return receipt, nil
}

// archiveGatewayTaskCleanupReceiptTx shares the event-compaction transaction.
// It never reconstructs ownership from an expired tombstone or Task annotations.
func archiveGatewayTaskCleanupReceiptTx(ctx context.Context, tx *sql.Tx, event *store.GatewayEvent, now time.Time) error {
	if event.TaskName == "" || event.TaskUID == "" {
		return nil // No exact Task was linked to this event.
	}
	receipt := store.GatewayTaskCleanupReceipt{
		Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
		GatewayName: event.GatewayName, GatewayUID: event.GatewayUID,
		BindingName: event.BindingName, BindingUID: event.BindingUID,
		EventID: event.ID, TaskName: event.TaskName, TaskUID: event.TaskUID,
		SessionName: event.SessionName, CompactedAt: now.UTC(),
	}
	if err := receipt.Validate(event.Namespace, event.TaskName, event.TaskUID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO gateway_task_cleanup_receipts (
		namespace, namespace_uid, gateway_name, gateway_uid, binding_name, binding_uid,
		event_id, task_name, task_uid, session_name, compacted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(namespace, task_uid) DO NOTHING`,
		receipt.Namespace, receipt.NamespaceUID, receipt.GatewayName, receipt.GatewayUID,
		receipt.BindingName, receipt.BindingUID, receipt.EventID, receipt.TaskName, receipt.TaskUID,
		receipt.SessionName, receipt.CompactedAt,
	)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 1 {
		return nil
	}
	existing, err := getGatewayTaskCleanupReceiptQuery(ctx, tx, receipt.Namespace, receipt.TaskName, receipt.TaskUID)
	if err != nil {
		return err
	}
	// Keep the original compaction time on an exact retry. Every ownership field
	// must match; a newer admission must have its own Task UID and receipt.
	identity := *existing
	identity.CompactedAt = receipt.CompactedAt
	if identity != receipt {
		return store.ConflictErrorf("Gateway Task cleanup receipt belongs to another event or owner")
	}
	return nil
}
