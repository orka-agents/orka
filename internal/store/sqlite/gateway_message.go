package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
)

const gatewayDeliveryKindMessage = protocol.DeliveryKindMessage

// EnqueueGatewayMessage serializes eligibility, dedupe, quota and insertion with
// terminal projection and cleanup. Reuse the authorized task-data writer rather
// than nesting BeginTx (the store uses a single SQLite connection).
func (s *Store) EnqueueGatewayMessage(ctx context.Context, request store.GatewayMessageEnqueue) (*store.GatewayDelivery, bool, error) {
	if err := validateGatewayMessageEnqueue(request); err != nil {
		return nil, false, err
	}
	if tx := s.taskDataTx(ctx); tx != nil {
		return enqueueGatewayMessageTx(ctx, tx, request)
	}
	var delivery *store.GatewayDelivery
	var created bool
	err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		var err error
		delivery, created, err = enqueueGatewayMessageTx(txCtx, s.taskDataTx(txCtx), request)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return delivery, created, nil
}

func validateGatewayMessageIdentity(namespace, namespaceUID, eventID, taskName, taskUID, requestID string) error {
	for name, value := range map[string]string{
		"namespace name": namespace, "namespace UID": namespaceUID, "event ID": eventID,
		"task name": taskName, "task UID": taskUID, "request ID": requestID,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || len(value) > protocol.MaxIdentityBytes ||
			!utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return store.ValidationErrorf("gateway message %s is invalid", name)
		}
	}
	return nil
}

func validateGatewayMessageEnqueue(request store.GatewayMessageEnqueue) error {
	if err := validateGatewayMessageIdentity(request.Namespace, request.NamespaceUID, request.EventID, request.TaskName, request.TaskUID, request.RequestID); err != nil {
		return err
	}
	if request.MaxMessages <= 0 || request.MaxAttempts <= 0 {
		return store.ValidationErrorf("gateway message limits must be positive")
	}
	if request.Now.IsZero() || !request.ExpiresAt.After(request.Now) {
		return store.ValidationErrorf("gateway message requires a current time and future expiry")
	}
	if strings.TrimSpace(request.Text) == "" {
		return store.ValidationErrorf("gateway message text is required")
	}
	return nil
}

func enqueueGatewayMessageTx(ctx context.Context, tx *sql.Tx, request store.GatewayMessageEnqueue) (*store.GatewayDelivery, bool, error) {
	event, err := getGatewayEventQuery(ctx, tx, request.Namespace, request.EventID)
	if err != nil {
		return nil, false, err
	}
	if event.NamespaceUID != request.NamespaceUID || event.TaskName != request.TaskName || event.TaskUID != request.TaskUID {
		return nil, false, store.ErrConflict
	}
	id := gatewayMessageDeliveryID(request.Namespace, request.NamespaceUID, event.ID, event.TaskUID, request.RequestID)
	replyTarget := strings.TrimSpace(event.ReplyTarget)
	if replyTarget == "" {
		replyTarget = event.ContextID
	}
	delivery := &store.GatewayDelivery{
		ID: id, IdempotencyID: id, Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
		GatewayUID: event.GatewayUID, GatewayGeneration: event.GatewayGeneration, GatewayName: event.GatewayName,
		BindingName: event.BindingName, EventID: event.ID, TaskName: event.TaskName, SessionName: event.SessionName,
		Kind: gatewayDeliveryKindMessage, State: store.GatewayDeliveryPending,
		AccountID: event.AccountID, ContextID: event.ContextID, ThreadID: event.ThreadID, ReplyTarget: replyTarget,
		Text: request.Text, MaxAttempts: request.MaxAttempts, NextAttemptAt: request.Now.UTC(), ExpiresAt: request.ExpiresAt.UTC(),
		TraceParent: event.TraceParent, TraceState: event.TraceState, CreatedAt: request.Now.UTC(), UpdatedAt: request.Now.UTC(),
	}
	if err := protocol.ValidateDeliveryRequest(&protocol.DeliveryRequest{
		ProtocolVersion: event.ProtocolVersion, DeliveryID: id, IdempotencyID: id, OriginatingEvent: event.ID,
		Kind: delivery.Kind, AccountID: delivery.AccountID, ContextID: delivery.ContextID, ReplyTarget: delivery.ReplyTarget, Text: delivery.Text,
	}); err != nil {
		return nil, false, store.ValidationErrorf("gateway message delivery is invalid: %s", err)
	}
	if existing, err := getGatewayDeliveryQuery(ctx, tx, request.Namespace, id); err == nil {
		if existing.Kind != gatewayDeliveryKindMessage || existing.EventID != event.ID || existing.Text != request.Text {
			return nil, false, store.ErrDuplicateMismatch
		}
		// A replay is a receipt lookup, even after closure or exhaustion; it
		// neither requeues the row nor consumes another lifetime quota slot.
		return existing, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	if request.ReplayOnly {
		return nil, false, store.ErrGatewayMessageReplayOnly
	}
	if event.State != store.GatewayEventTaskCreated || event.DeliveryID != "" || !event.ExpiresAt.After(request.Now) {
		return nil, false, store.ErrConflict
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions
		WHERE namespace = ? AND name = ? AND active_task = ? AND active_task_uid = ?)`,
		event.Namespace, event.SessionName, event.TaskName, event.TaskUID).Scan(&active); err != nil {
		return nil, false, err
	}
	if !active {
		return nil, false, store.ErrConflict
	}
	var accepted int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_deliveries delivery
		JOIN gateway_events event ON event.namespace = delivery.namespace AND event.id = delivery.event_id
		WHERE event.namespace = ? AND event.task_uid = ? AND delivery.kind = ?`,
		event.Namespace, event.TaskUID, gatewayDeliveryKindMessage).Scan(&accepted); err != nil {
		return nil, false, err
	}
	if accepted >= request.MaxMessages {
		return nil, false, store.ErrCapacity
	}
	return createGatewayDeliveryTx(ctx, tx, delivery)
}

// gatewayMessageDeliveryID is shared by admission and budget replay detection.
// The length-delimited identity includes event and Task incarnations, never text.
func gatewayMessageDeliveryID(namespace, namespaceUID, eventID, taskUID, requestID string) string {
	identity, _ := json.Marshal([]string{namespace, namespaceUID, eventID, taskUID, requestID})
	digest := sha256.Sum256(identity)
	return "gdm-" + hex.EncodeToString(digest[:])
}

// GetGatewayMessageBudget never admits, reserves, or consumes a message slot.
// Reuse an authorized writer when supplied, including its revocation fence.
func (s *Store) GetGatewayMessageBudget(ctx context.Context, request store.GatewayMessageBudgetQuery) (*store.GatewayMessageBudget, error) {
	if err := validateGatewayMessageIdentity(request.Namespace, request.NamespaceUID, request.EventID, request.TaskName, request.TaskUID, request.RequestID); err != nil {
		return nil, err
	}
	var q queryRower = s.db
	if tx := s.taskDataTx(ctx); tx != nil {
		q = tx
	}
	id := gatewayMessageDeliveryID(request.Namespace, request.NamespaceUID, request.EventID, request.TaskUID, request.RequestID)
	var budget store.GatewayMessageBudget
	// One statement keeps the identity, lifetime count and replay snapshot consistent.
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM gateway_deliveries d JOIN gateway_events e ON e.namespace = d.namespace AND e.id = d.event_id
		 WHERE e.namespace = event.namespace AND e.task_uid = event.task_uid AND d.kind = ?),
		EXISTS (SELECT 1 FROM gateway_deliveries d WHERE d.namespace = event.namespace AND d.id = ? AND d.event_id = event.id AND d.kind = ?)
		FROM gateway_events event WHERE event.namespace = ? AND event.namespace_uid = ? AND event.id = ? AND event.task_name = ? AND event.task_uid = ?`,
		gatewayDeliveryKindMessage, id, gatewayDeliveryKindMessage, request.Namespace, request.NamespaceUID, request.EventID, request.TaskName, request.TaskUID).Scan(&budget.Accepted, &budget.RequestExists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &budget, nil
}

// advanceGatewayDeliveryOrderTx makes created_at a per-event logical admission
// clock for messages and their terminal successor. It is persisted, assigned
// under the writer, and strictly advances despite tied/backwards wall clocks.
// Retention keeps the whole event's evidence until every row can be removed.
func advanceGatewayDeliveryOrderTx(ctx context.Context, tx *sql.Tx, delivery *store.GatewayDelivery) error {
	var latest time.Time
	err := tx.QueryRowContext(ctx, `SELECT created_at FROM gateway_deliveries
		WHERE namespace = ? AND event_id = ? AND (kind = ? OR ? = ?)
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		delivery.Namespace, delivery.EventID, gatewayDeliveryKindMessage, delivery.Kind, gatewayDeliveryKindMessage).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !delivery.CreatedAt.After(latest) {
		delivery.CreatedAt = latest.Add(time.Nanosecond)
	}
	return nil
}
