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
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

var _ store.AgentExecutionBindingReservationStore = (*Store)(nil)

// SetAgentExecutionBindingReservationGate creates or CAS-updates the one
// store-local admission gate for a backend. Repeating an already committed
// target is idempotent. A closed revision cannot be reopened; reopening
// requires a distinct authoritative control revision.
func (s *Store) SetAgentExecutionBindingReservationGate(
	ctx context.Context,
	gate store.AgentExecutionBindingReservationGate,
) (*store.AgentExecutionBindingReservationGate, error) {
	if err := gate.Revision.Validate(); err != nil {
		return nil, err
	}
	if gate.Version < 0 {
		return nil, store.ValidationErrorf("binding reservation gate version must not be negative")
	}
	gate.UpdatedAt = normalizeControlTime(gate.UpdatedAt)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin binding reservation gate mutation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := getAgentExecutionBindingReservationGate(ctx, tx, gate.Revision.Backend)
	if errors.Is(err, store.ErrNotFound) {
		if gate.Version != 0 {
			return nil, bindingReservationConflict(
				"%s gate does not exist; creation requires expected version 0", gate.Revision.Backend,
			)
		}
		gate.Version = 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_execution_binding_reservation_gates
			(backend, control_uid, control_generation, mode_revision, open, version, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(gate.Revision.Backend), gate.Revision.ControlUID, gate.Revision.ControlGeneration,
			gate.Revision.ModeRevision, gate.Open, gate.Version, gate.UpdatedAt); err != nil {
			if isSQLiteConstraintError(err) {
				return nil, bindingReservationConflict("%s gate was created concurrently", gate.Revision.Backend)
			}
			return nil, fmt.Errorf("create binding reservation gate: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit binding reservation gate creation: %w", err)
		}
		return &gate, nil
	}
	if err != nil {
		return nil, err
	}

	if current.Revision == gate.Revision && current.Open == gate.Open {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit binding reservation gate verification: %w", err)
		}
		return current, nil
	}
	if gate.Version != current.Version {
		return nil, bindingReservationConflict(
			"%s gate is version %d, expected %d", gate.Revision.Backend, current.Version, gate.Version,
		)
	}
	if !current.Open && gate.Open && current.Revision == gate.Revision {
		return nil, bindingReservationConflict(
			"closed %s gate revision %s cannot be reopened", gate.Revision.Backend, gate.Revision.CanonicalID(),
		)
	}

	result, err := tx.ExecContext(ctx, `UPDATE agent_execution_binding_reservation_gates
		SET control_uid = ?, control_generation = ?, mode_revision = ?, open = ?,
			version = version + 1, updated_at = ?
		WHERE backend = ? AND version = ?`,
		gate.Revision.ControlUID, gate.Revision.ControlGeneration, gate.Revision.ModeRevision,
		gate.Open, gate.UpdatedAt, string(gate.Revision.Backend), gate.Version)
	if err != nil {
		return nil, fmt.Errorf("update binding reservation gate: %w", err)
	}
	if err := rowsAffectedExactlyOne(result, "binding reservation gate"); err != nil {
		return nil, err
	}
	gate.Version++
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit binding reservation gate mutation: %w", err)
	}
	return &gate, nil
}

// GetAgentExecutionBindingReservationGate returns the current gate for one
// backend.
func (s *Store) GetAgentExecutionBindingReservationGate(
	ctx context.Context,
	backend store.AgentExecutionBackendKey,
) (*store.AgentExecutionBindingReservationGate, error) {
	if err := validateAgentExecutionBackend(backend); err != nil {
		return nil, err
	}
	return getAgentExecutionBindingReservationGate(ctx, s.db, backend)
}

// CreateAgentExecutionBindingReservation atomically verifies the exact open
// gate revision, inserts the reservation, and advances the backend watermark.
// An exact existing Task reservation is returned even after gate closure so a
// caller can recover an acknowledged-or-committed ambiguity without creating
// post-cutoff work.
func (s *Store) CreateAgentExecutionBindingReservation(
	ctx context.Context,
	reservation store.AgentExecutionBindingReservation,
) (*store.AgentExecutionBindingReservation, error) {
	normalized, err := normalizeAgentExecutionBindingReservationForCreate(reservation)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin binding reservation creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := getAgentExecutionBindingReservationByTaskUID(ctx, tx, normalized.TaskUID)
	switch {
	case err == nil:
		if !sameAgentExecutionBindingReservationCreation(existing, normalized) {
			return nil, bindingReservationConflict(
				"Task UID %q already has a different binding reservation", normalized.TaskUID,
			)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit binding reservation verification: %w", err)
		}
		return existing, nil
	case errors.Is(err, store.ErrNotFound):
	default:
		return nil, err
	}

	gate, err := getAgentExecutionBindingReservationGate(ctx, tx, normalized.Revision.Backend)
	if errors.Is(err, store.ErrNotFound) {
		return nil, bindingReservationConflict(
			"%s binding reservation gate is missing", normalized.Revision.Backend,
		)
	}
	if err != nil {
		return nil, err
	}
	if !gate.Open {
		return nil, bindingReservationConflict(
			"%s binding reservation gate is closed at revision %s",
			normalized.Revision.Backend, gate.Revision.CanonicalID(),
		)
	}
	if gate.Revision != normalized.Revision {
		return nil, bindingReservationConflict(
			"%s binding reservation gate revision %s does not match requested revision %s",
			normalized.Revision.Backend, gate.Revision.CanonicalID(), normalized.Revision.CanonicalID(),
		)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_execution_binding_reservations
		(id, task_namespace, task_name, task_uid, control_uid, control_generation, backend,
		 mode_revision, binding_digest, snapshot_digest, state, terminal_reason, version,
		 reserved_at, settled_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		normalized.ID, normalized.TaskNamespace, normalized.TaskName, normalized.TaskUID,
		normalized.Revision.ControlUID, normalized.Revision.ControlGeneration,
		string(normalized.Revision.Backend), normalized.Revision.ModeRevision,
		normalized.BindingDigest, normalized.SnapshotDigest, string(normalized.State),
		normalized.TerminalReason, normalized.Version, normalized.ReservedAt, normalized.UpdatedAt); err != nil {
		if isSQLiteConstraintError(err) {
			return nil, bindingReservationConflict(
				"Task UID %q acquired a binding reservation concurrently", normalized.TaskUID,
			)
		}
		return nil, fmt.Errorf("create binding reservation: %w", err)
	}
	if err := advanceAgentExecutionBindingReservationWatermark(ctx, tx, normalized.Revision.Backend); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit binding reservation creation: %w", err)
	}
	return &normalized, nil
}

// GetAgentExecutionBindingReservation returns one durable reservation by its
// canonical ID.
func (s *Store) GetAgentExecutionBindingReservation(
	ctx context.Context,
	id string,
) (*store.AgentExecutionBindingReservation, error) {
	if err := store.ValidateControlIdentifier("binding reservation ID", id); err != nil {
		return nil, err
	}
	return getAgentExecutionBindingReservationByID(ctx, s.db, id)
}

// SettleAgentExecutionBindingReservation terminalizes an open reservation and
// advances the mutation watermark in the same transaction. An exact terminal
// retry is idempotent even when it carries the pre-settlement version.
func (s *Store) SettleAgentExecutionBindingReservation(
	ctx context.Context,
	request store.SettleAgentExecutionBindingReservationRequest,
) (*store.AgentExecutionBindingReservation, error) {
	request, err := normalizeSettleAgentExecutionBindingReservationRequest(request)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin binding reservation settlement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := getAgentExecutionBindingReservationByID(ctx, tx, request.ID)
	if err != nil {
		return nil, err
	}
	if store.IsTerminalAgentExecutionBindingReservationState(current.State) {
		if current.State == request.TargetState &&
			current.BindingDigest == request.BindingDigest &&
			current.TerminalReason == request.TerminalReason {
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit binding reservation settlement verification: %w", err)
			}
			return current, nil
		}
		return nil, bindingReservationConflict(
			"binding reservation %q is already terminal in state %s", current.ID, current.State,
		)
	}
	if current.State != store.AgentExecutionBindingReservationOpen {
		return nil, bindingReservationConflict(
			"binding reservation %q has unsupported nonterminal state %s", current.ID, current.State,
		)
	}
	if current.Version != request.ExpectedVersion {
		return nil, bindingReservationConflict(
			"binding reservation %q is version %d, expected %d",
			current.ID, current.Version, request.ExpectedVersion,
		)
	}
	if current.BindingDigest != request.BindingDigest {
		return nil, bindingReservationConflict(
			"binding reservation %q binding digest does not match", current.ID,
		)
	}
	if request.SettledAt.Before(current.ReservedAt) {
		return nil, store.ValidationErrorf("binding reservation settlement time precedes reservation time")
	}

	result, err := tx.ExecContext(ctx, `UPDATE agent_execution_binding_reservations
		SET state = ?, terminal_reason = ?, version = version + 1, settled_at = ?, updated_at = ?
		WHERE id = ? AND version = ? AND state = 'Open'`,
		string(request.TargetState), request.TerminalReason, request.SettledAt, request.SettledAt,
		request.ID, request.ExpectedVersion)
	if err != nil {
		return nil, fmt.Errorf("settle binding reservation: %w", err)
	}
	if err := rowsAffectedExactlyOne(result, "binding reservation"); err != nil {
		return nil, err
	}
	if err := advanceAgentExecutionBindingReservationWatermark(ctx, tx, current.Revision.Backend); err != nil {
		return nil, err
	}

	current.State = request.TargetState
	current.TerminalReason = request.TerminalReason
	current.Version++
	current.SettledAt = &request.SettledAt
	current.UpdatedAt = request.SettledAt
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit binding reservation settlement: %w", err)
	}
	return current, nil
}

// ListAgentExecutionBindingReservations returns a revision-exact inventory
// and the per-backend mutation watermark from one SQLite read transaction.
func (s *Store) ListAgentExecutionBindingReservations(
	ctx context.Context,
	revision store.AgentExecutionControlRevision,
) (*store.AgentExecutionBindingReservationInventory, error) {
	if err := revision.Validate(); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin binding reservation inventory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, agentExecutionBindingReservationSelectSQL+`
		WHERE backend = ? AND control_uid = ? AND control_generation = ? AND mode_revision = ?
		ORDER BY reserved_at ASC, id ASC`,
		string(revision.Backend), revision.ControlUID, revision.ControlGeneration, revision.ModeRevision)
	if err != nil {
		return nil, fmt.Errorf("list binding reservations: %w", err)
	}
	reservations := make([]store.AgentExecutionBindingReservation, 0)
	openCount := 0
	for rows.Next() {
		reservation, err := scanAgentExecutionBindingReservation(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan binding reservation inventory: %w", err)
		}
		if reservation.State == store.AgentExecutionBindingReservationOpen {
			openCount++
		}
		reservations = append(reservations, *reservation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate binding reservation inventory: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close binding reservation inventory: %w", err)
	}

	var watermark int64
	err = tx.QueryRowContext(ctx, `SELECT watermark
		FROM agent_execution_binding_reservation_watermarks WHERE backend = ?`,
		string(revision.Backend)).Scan(&watermark)
	if errors.Is(err, sql.ErrNoRows) {
		watermark = 0
	} else if err != nil {
		return nil, fmt.Errorf("read binding reservation watermark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit binding reservation inventory: %w", err)
	}
	return &store.AgentExecutionBindingReservationInventory{
		Revision: revision, Reservations: reservations, Watermark: watermark, OpenCount: openCount,
	}, nil
}

func normalizeAgentExecutionBindingReservationForCreate(
	reservation store.AgentExecutionBindingReservation,
) (store.AgentExecutionBindingReservation, error) {
	if err := reservation.Validate(); err != nil {
		return store.AgentExecutionBindingReservation{}, err
	}
	canonicalID := reservation.CanonicalID()
	if reservation.ID == "" {
		reservation.ID = canonicalID
	} else if reservation.ID != canonicalID {
		return store.AgentExecutionBindingReservation{}, store.ValidationErrorf(
			"binding reservation ID must equal canonical ID %q", canonicalID,
		)
	}
	if reservation.State == "" {
		reservation.State = store.AgentExecutionBindingReservationOpen
	}
	if reservation.State != store.AgentExecutionBindingReservationOpen {
		return store.AgentExecutionBindingReservation{}, store.ValidationErrorf(
			"new binding reservation state must be %s", store.AgentExecutionBindingReservationOpen,
		)
	}
	if reservation.TerminalReason != "" || reservation.SettledAt != nil {
		return store.AgentExecutionBindingReservation{}, store.ValidationErrorf(
			"new binding reservation cannot contain terminal settlement data",
		)
	}
	if reservation.Version == 0 {
		reservation.Version = 1
	} else if reservation.Version != 1 {
		return store.AgentExecutionBindingReservation{}, store.ValidationErrorf(
			"new binding reservation version must be 1",
		)
	}
	reservation.ReservedAt = normalizeControlTime(reservation.ReservedAt)
	if reservation.UpdatedAt.IsZero() {
		reservation.UpdatedAt = reservation.ReservedAt
	} else {
		reservation.UpdatedAt = reservation.UpdatedAt.UTC()
		if !reservation.UpdatedAt.Equal(reservation.ReservedAt) {
			return store.AgentExecutionBindingReservation{}, store.ValidationErrorf(
				"new binding reservation update time must equal reservation time",
			)
		}
	}
	return reservation, nil
}

func normalizeSettleAgentExecutionBindingReservationRequest(
	request store.SettleAgentExecutionBindingReservationRequest,
) (store.SettleAgentExecutionBindingReservationRequest, error) {
	if err := store.ValidateControlIdentifier("binding reservation ID", request.ID); err != nil {
		return store.SettleAgentExecutionBindingReservationRequest{}, err
	}
	if request.ExpectedVersion < 1 {
		return store.SettleAgentExecutionBindingReservationRequest{}, store.ValidationErrorf(
			"binding reservation expected version must be positive",
		)
	}
	if !store.IsTerminalAgentExecutionBindingReservationState(request.TargetState) {
		return store.SettleAgentExecutionBindingReservationRequest{}, store.ValidationErrorf(
			"binding reservation settlement target %q must be terminal", request.TargetState,
		)
	}
	if err := store.ValidateCanonicalDigest("binding reservation settlement binding digest", request.BindingDigest); err != nil {
		return store.SettleAgentExecutionBindingReservationRequest{}, err
	}
	if err := store.ValidateControlReason("binding reservation terminal reason", request.TerminalReason); err != nil {
		return store.SettleAgentExecutionBindingReservationRequest{}, err
	}
	switch request.TargetState {
	case store.AgentExecutionBindingReservationBound:
		if request.TerminalReason != "" {
			return store.SettleAgentExecutionBindingReservationRequest{}, store.ValidationErrorf(
				"bound binding reservation cannot contain a terminal reason",
			)
		}
	case store.AgentExecutionBindingReservationRejected:
		if strings.TrimSpace(request.TerminalReason) == "" {
			return store.SettleAgentExecutionBindingReservationRequest{}, store.ValidationErrorf(
				"rejected binding reservation terminal reason is required",
			)
		}
	}
	request.SettledAt = normalizeControlTime(request.SettledAt)
	return request, nil
}

func sameAgentExecutionBindingReservationCreation(
	existing *store.AgentExecutionBindingReservation,
	candidate store.AgentExecutionBindingReservation,
) bool {
	return existing.ID == candidate.ID &&
		existing.TaskNamespace == candidate.TaskNamespace &&
		existing.TaskName == candidate.TaskName &&
		existing.TaskUID == candidate.TaskUID &&
		existing.Revision == candidate.Revision &&
		existing.BindingDigest == candidate.BindingDigest &&
		existing.SnapshotDigest == candidate.SnapshotDigest
}

func validateAgentExecutionBackend(backend store.AgentExecutionBackendKey) error {
	switch backend {
	case store.AgentExecutionBackendV1, store.AgentExecutionBackendV2:
		return nil
	default:
		return store.ValidationErrorf("agent execution backend %q is unsupported", backend)
	}
}

func getAgentExecutionBindingReservationGate(
	ctx context.Context,
	q controlQueryRower,
	backend store.AgentExecutionBackendKey,
) (*store.AgentExecutionBindingReservationGate, error) {
	gate := store.AgentExecutionBindingReservationGate{
		Revision: store.AgentExecutionControlRevision{Backend: backend},
	}
	err := q.QueryRowContext(ctx, `SELECT control_uid, control_generation, mode_revision, open, version, updated_at
		FROM agent_execution_binding_reservation_gates WHERE backend = ?`, string(backend)).Scan(
		&gate.Revision.ControlUID, &gate.Revision.ControlGeneration, &gate.Revision.ModeRevision,
		&gate.Open, &gate.Version, &gate.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get binding reservation gate: %w", err)
	}
	gate.UpdatedAt = gate.UpdatedAt.UTC()
	if err := validatePersistedAgentExecutionBindingReservationGate(gate); err != nil {
		return nil, fmt.Errorf("invalid persisted binding reservation gate: %w", err)
	}
	return &gate, nil
}

const agentExecutionBindingReservationSelectSQL = `SELECT
	id, task_namespace, task_name, task_uid, control_uid, control_generation, backend,
	mode_revision, binding_digest, snapshot_digest, state, terminal_reason, version,
	reserved_at, settled_at, updated_at
	FROM agent_execution_binding_reservations `

type agentExecutionBindingReservationScanner interface {
	Scan(dest ...any) error
}

func getAgentExecutionBindingReservationByID(
	ctx context.Context,
	q controlQueryRower,
	id string,
) (*store.AgentExecutionBindingReservation, error) {
	reservation, err := scanAgentExecutionBindingReservation(q.QueryRowContext(
		ctx, agentExecutionBindingReservationSelectSQL+`WHERE id = ?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get binding reservation: %w", err)
	}
	return reservation, nil
}

func getAgentExecutionBindingReservationByTaskUID(
	ctx context.Context,
	q controlQueryRower,
	taskUID string,
) (*store.AgentExecutionBindingReservation, error) {
	reservation, err := scanAgentExecutionBindingReservation(q.QueryRowContext(
		ctx, agentExecutionBindingReservationSelectSQL+`WHERE task_uid = ?`, taskUID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get binding reservation by Task UID: %w", err)
	}
	return reservation, nil
}

func scanAgentExecutionBindingReservation(
	scanner agentExecutionBindingReservationScanner,
) (*store.AgentExecutionBindingReservation, error) {
	var (
		reservation store.AgentExecutionBindingReservation
		backend     string
		state       string
		settledAt   sql.NullTime
	)
	if err := scanner.Scan(
		&reservation.ID, &reservation.TaskNamespace, &reservation.TaskName, &reservation.TaskUID,
		&reservation.Revision.ControlUID, &reservation.Revision.ControlGeneration, &backend,
		&reservation.Revision.ModeRevision, &reservation.BindingDigest, &reservation.SnapshotDigest,
		&state, &reservation.TerminalReason, &reservation.Version, &reservation.ReservedAt,
		&settledAt, &reservation.UpdatedAt,
	); err != nil {
		return nil, err
	}
	reservation.Revision.Backend = store.AgentExecutionBackendKey(backend)
	reservation.State = store.AgentExecutionBindingReservationState(state)
	reservation.ReservedAt = reservation.ReservedAt.UTC()
	reservation.UpdatedAt = reservation.UpdatedAt.UTC()
	if settledAt.Valid {
		normalized := settledAt.Time.UTC()
		reservation.SettledAt = &normalized
	}
	if err := validatePersistedAgentExecutionBindingReservation(reservation); err != nil {
		return nil, fmt.Errorf("invalid persisted binding reservation: %w", err)
	}
	return &reservation, nil
}

func validatePersistedAgentExecutionBindingReservationGate(
	gate store.AgentExecutionBindingReservationGate,
) error {
	if err := gate.Revision.Validate(); err != nil {
		return err
	}
	if gate.Version < 1 {
		return store.ValidationErrorf("binding reservation gate version must be positive")
	}
	if gate.UpdatedAt.IsZero() {
		return store.ValidationErrorf("binding reservation gate update time is required")
	}
	return nil
}

func validatePersistedAgentExecutionBindingReservation(
	reservation store.AgentExecutionBindingReservation,
) error {
	if err := reservation.Validate(); err != nil {
		return err
	}
	if reservation.ID != reservation.CanonicalID() {
		return store.ValidationErrorf("binding reservation ID is not canonical")
	}
	if !store.IsKnownAgentExecutionBindingReservationState(reservation.State) {
		return store.ValidationErrorf("binding reservation state %q is unsupported", reservation.State)
	}
	if reservation.Version < 1 {
		return store.ValidationErrorf("binding reservation version must be positive")
	}
	if reservation.ReservedAt.IsZero() || reservation.UpdatedAt.IsZero() {
		return store.ValidationErrorf("binding reservation timestamps are required")
	}
	if reservation.UpdatedAt.Before(reservation.ReservedAt) {
		return store.ValidationErrorf("binding reservation update time precedes reservation time")
	}
	if err := store.ValidateControlReason("binding reservation terminal reason", reservation.TerminalReason); err != nil {
		return err
	}
	switch reservation.State {
	case store.AgentExecutionBindingReservationOpen:
		if reservation.SettledAt != nil || reservation.TerminalReason != "" {
			return store.ValidationErrorf("open binding reservation contains terminal settlement data")
		}
	case store.AgentExecutionBindingReservationBound:
		if reservation.SettledAt == nil || reservation.TerminalReason != "" {
			return store.ValidationErrorf("bound binding reservation has invalid terminal settlement data")
		}
	case store.AgentExecutionBindingReservationRejected:
		if reservation.SettledAt == nil || strings.TrimSpace(reservation.TerminalReason) == "" {
			return store.ValidationErrorf("rejected binding reservation has invalid terminal settlement data")
		}
	}
	if reservation.SettledAt != nil {
		if reservation.SettledAt.Before(reservation.ReservedAt) {
			return store.ValidationErrorf("binding reservation settlement time precedes reservation time")
		}
		if !reservation.UpdatedAt.Equal(*reservation.SettledAt) {
			return store.ValidationErrorf("terminal binding reservation update time must equal settlement time")
		}
	}
	return nil
}

func advanceAgentExecutionBindingReservationWatermark(
	ctx context.Context,
	tx *sql.Tx,
	backend store.AgentExecutionBackendKey,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_execution_binding_reservation_watermarks
		(backend, watermark) VALUES (?, 1)
		ON CONFLICT(backend) DO UPDATE SET watermark = watermark + 1`, string(backend)); err != nil {
		return fmt.Errorf("advance binding reservation watermark: %w", err)
	}
	return nil
}

func bindingReservationConflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrConflict, fmt.Sprintf(format, args...))
}
