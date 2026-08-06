/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"context"
	"fmt"
	"time"
)

// AgentExecutionBackendKey identifies one independently drained execution
// backend without importing Kubernetes API types into the durable store.
type AgentExecutionBackendKey string

const (
	AgentExecutionBackendV1 AgentExecutionBackendKey = "v1"
	AgentExecutionBackendV2 AgentExecutionBackendKey = "v2"
)

// AgentExecutionControlRevision is the exact admission revision under which a
// binding reservation may be created.
type AgentExecutionControlRevision struct {
	ControlUID        string
	ControlGeneration int64
	Backend           AgentExecutionBackendKey
	ModeRevision      int64
}

func (r AgentExecutionControlRevision) Validate() error {
	if err := ValidateControlIdentifier("agent execution control UID", r.ControlUID); err != nil {
		return err
	}
	if r.ControlGeneration < 1 {
		return ValidationErrorf("agent execution control generation must be positive")
	}
	if r.ModeRevision < 1 {
		return ValidationErrorf("agent execution mode revision must be positive")
	}
	switch r.Backend {
	case AgentExecutionBackendV1, AgentExecutionBackendV2:
		return nil
	default:
		return ValidationErrorf("agent execution backend %q is unsupported", r.Backend)
	}
}

func (r AgentExecutionControlRevision) CanonicalID() string {
	return CanonicalControlID(
		"agent-execution-control-revision", r.ControlUID,
		fmt.Sprintf("%d", r.ControlGeneration), string(r.Backend), fmt.Sprintf("%d", r.ModeRevision),
	)
}

// AgentExecutionBindingReservationGate is the store-local linearization gate.
// Only the control reconciler changes it. Reservation creation checks it in
// the same SQLite transaction that inserts the reservation.
type AgentExecutionBindingReservationGate struct {
	Revision AgentExecutionControlRevision
	Open     bool
	// Version is the exact current version expected by a mutation. Creation
	// uses zero; a successful mutation returns the incremented version.
	Version   int64
	UpdatedAt time.Time
}

// AgentExecutionBindingReservationState is terminal except for Open.
type AgentExecutionBindingReservationState string

const (
	AgentExecutionBindingReservationOpen     AgentExecutionBindingReservationState = "Open"
	AgentExecutionBindingReservationBound    AgentExecutionBindingReservationState = "Bound"
	AgentExecutionBindingReservationRejected AgentExecutionBindingReservationState = "Rejected"
)

func IsTerminalAgentExecutionBindingReservationState(state AgentExecutionBindingReservationState) bool {
	return state == AgentExecutionBindingReservationBound || state == AgentExecutionBindingReservationRejected
}

func IsKnownAgentExecutionBindingReservationState(state AgentExecutionBindingReservationState) bool {
	switch state {
	case AgentExecutionBindingReservationOpen, AgentExecutionBindingReservationBound,
		AgentExecutionBindingReservationRejected:
		return true
	default:
		return false
	}
}

// AgentExecutionBindingReservation durably proves that one Task entered the
// binding CAS before admission for its exact control revision closed.
type AgentExecutionBindingReservation struct {
	ID             string
	TaskNamespace  string
	TaskName       string
	TaskUID        string
	Revision       AgentExecutionControlRevision
	BindingDigest  string
	SnapshotDigest string
	State          AgentExecutionBindingReservationState
	TerminalReason string
	Version        int64
	ReservedAt     time.Time
	SettledAt      *time.Time
	UpdatedAt      time.Time
}

func (r AgentExecutionBindingReservation) Validate() error {
	if err := ValidateControlIdentifier("binding reservation Task namespace", r.TaskNamespace); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("binding reservation Task name", r.TaskName); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("binding reservation Task UID", r.TaskUID); err != nil {
		return err
	}
	if err := r.Revision.Validate(); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("binding reservation binding digest", r.BindingDigest); err != nil {
		return err
	}
	return ValidateCanonicalDigest("binding reservation snapshot digest", r.SnapshotDigest)
}

func (r AgentExecutionBindingReservation) CanonicalID() string {
	return CanonicalControlID("agent-execution-binding-reservation", r.TaskUID, r.Revision.CanonicalID())
}

type SettleAgentExecutionBindingReservationRequest struct {
	ID              string
	ExpectedVersion int64
	TargetState     AgentExecutionBindingReservationState
	BindingDigest   string
	TerminalReason  string
	SettledAt       time.Time
}

// AgentExecutionBindingReservationInventory is a stable, sorted revision
// inventory plus a mutation watermark used by repeated closure proof passes.
type AgentExecutionBindingReservationInventory struct {
	Revision     AgentExecutionControlRevision
	Reservations []AgentExecutionBindingReservation
	Watermark    int64
	OpenCount    int
}

// AgentExecutionBindingReservationStore persists the serialized admission
// gate and every reservation created under it.
type AgentExecutionBindingReservationStore interface {
	SetAgentExecutionBindingReservationGate(context.Context, AgentExecutionBindingReservationGate) (*AgentExecutionBindingReservationGate, error)
	GetAgentExecutionBindingReservationGate(context.Context, AgentExecutionBackendKey) (*AgentExecutionBindingReservationGate, error)
	CreateAgentExecutionBindingReservation(context.Context, AgentExecutionBindingReservation) (*AgentExecutionBindingReservation, error)
	GetAgentExecutionBindingReservation(context.Context, string) (*AgentExecutionBindingReservation, error)
	SettleAgentExecutionBindingReservation(context.Context, SettleAgentExecutionBindingReservationRequest) (*AgentExecutionBindingReservation, error)
	ListAgentExecutionBindingReservations(context.Context, AgentExecutionControlRevision) (*AgentExecutionBindingReservationInventory, error)
}
