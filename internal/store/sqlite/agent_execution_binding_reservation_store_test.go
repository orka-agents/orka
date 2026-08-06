/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestAgentExecutionBindingReservationGateLinearizesClosure(t *testing.T) {
	ctx := context.Background()
	s := newCoexistenceTestStore(t)
	revision := bindingReservationTestRevision(store.AgentExecutionBackendV1, 1)
	baseTime := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

	if _, err := s.GetAgentExecutionBindingReservationGate(ctx, revision.Backend); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAgentExecutionBindingReservationGate before create error = %v, want ErrNotFound", err)
	}
	gate, err := s.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision, Open: true, UpdatedAt: baseTime,
	})
	if err != nil {
		t.Fatalf("create gate: %v", err)
	}
	if gate.Version != 1 || !gate.Open {
		t.Fatalf("created gate = %+v, want open version 1", gate)
	}

	candidate := bindingReservationTestCandidate(revision, "task-uid-1", baseTime.Add(time.Second))
	created, err := s.CreateAgentExecutionBindingReservation(ctx, candidate)
	if err != nil {
		t.Fatalf("create reservation: %v", err)
	}
	if created.ID != candidate.CanonicalID() || created.State != store.AgentExecutionBindingReservationOpen || created.Version != 1 {
		t.Fatalf("created reservation = %+v", created)
	}
	assertBindingReservationInventory(t, ctx, s, revision, 1, 1)

	closed, err := s.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision, Open: false, Version: gate.Version, UpdatedAt: baseTime.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("close gate: %v", err)
	}
	if closed.Version != 2 || closed.Open {
		t.Fatalf("closed gate = %+v, want closed version 2", closed)
	}

	late := bindingReservationTestCandidate(revision, "task-uid-late", baseTime.Add(3*time.Second))
	if _, err := s.CreateAgentExecutionBindingReservation(ctx, late); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("late reservation error = %v, want ErrConflict", err)
	}
	// Retrying the exact committed reservation remains idempotent after closure
	// and does not advance the mutation watermark.
	retried, err := s.CreateAgentExecutionBindingReservation(ctx, candidate)
	if err != nil {
		t.Fatalf("retry reservation after closure: %v", err)
	}
	if retried.ID != created.ID || retried.Version != created.Version {
		t.Fatalf("retried reservation = %+v, want original %+v", retried, created)
	}
	assertBindingReservationInventory(t, ctx, s, revision, 1, 1)

	settledAt := baseTime.Add(4 * time.Second)
	bound, err := s.SettleAgentExecutionBindingReservation(ctx, store.SettleAgentExecutionBindingReservationRequest{
		ID: created.ID, ExpectedVersion: created.Version, TargetState: store.AgentExecutionBindingReservationBound,
		BindingDigest: created.BindingDigest, SettledAt: settledAt,
	})
	if err != nil {
		t.Fatalf("settle reservation: %v", err)
	}
	if bound.State != store.AgentExecutionBindingReservationBound || bound.Version != 2 ||
		bound.SettledAt == nil || !bound.SettledAt.Equal(settledAt) {
		t.Fatalf("bound reservation = %+v", bound)
	}
	assertBindingReservationInventory(t, ctx, s, revision, 2, 0)

	// The pre-settlement request is an exact idempotent retry.
	retriedBound, err := s.SettleAgentExecutionBindingReservation(ctx, store.SettleAgentExecutionBindingReservationRequest{
		ID: created.ID, ExpectedVersion: created.Version, TargetState: store.AgentExecutionBindingReservationBound,
		BindingDigest: created.BindingDigest, SettledAt: settledAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("retry settlement: %v", err)
	}
	if retriedBound.Version != bound.Version || !retriedBound.SettledAt.Equal(settledAt) {
		t.Fatalf("retried settlement = %+v, want original settlement %+v", retriedBound, bound)
	}
	assertBindingReservationInventory(t, ctx, s, revision, 2, 0)

	// A closed admission revision is one-way; a new revision is required to
	// reopen the backend.
	if _, err := s.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision, Open: true, Version: closed.Version, UpdatedAt: baseTime.Add(5 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reopen closed revision error = %v, want ErrConflict", err)
	}
	// Repeating the already committed closed target is idempotent even with the
	// original pre-close expected version.
	retriedClosed, err := s.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision, Open: false, Version: gate.Version, UpdatedAt: baseTime.Add(6 * time.Second),
	})
	if err != nil {
		t.Fatalf("retry closed gate: %v", err)
	}
	if retriedClosed.Version != closed.Version || retriedClosed.Open {
		t.Fatalf("retried closed gate = %+v, want %+v", retriedClosed, closed)
	}
}

func TestAgentExecutionBindingReservationRejectsStaleRevisionAndTaskRebinding(t *testing.T) {
	ctx := context.Background()
	s := newCoexistenceTestStore(t)
	baseTime := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	revision := bindingReservationTestRevision(store.AgentExecutionBackendV2, 4)
	gate, err := s.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision, Open: true, UpdatedAt: baseTime,
	})
	if err != nil {
		t.Fatalf("create gate: %v", err)
	}

	staleRevision := revision
	staleRevision.ModeRevision--
	if _, err := s.CreateAgentExecutionBindingReservation(ctx,
		bindingReservationTestCandidate(staleRevision, "task-uid-stale", baseTime.Add(time.Second))); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revision reservation error = %v, want ErrConflict", err)
	}

	firstCandidate := bindingReservationTestCandidate(revision, "task-uid-shared", baseTime.Add(2*time.Second))
	first, err := s.CreateAgentExecutionBindingReservation(ctx, firstCandidate)
	if err != nil {
		t.Fatalf("create first reservation: %v", err)
	}

	conflicting := firstCandidate
	conflicting.TaskName = "different-task-name"
	conflicting.BindingDigest = store.CanonicalAgentExecutionSnapshotDigest([]byte("different-binding"))
	conflicting.ID = ""
	if _, err := s.CreateAgentExecutionBindingReservation(ctx, conflicting); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Task UID rebinding error = %v, want ErrConflict", err)
	}

	nextRevision := revision
	nextRevision.ControlGeneration++
	nextRevision.ModeRevision++
	nextGate, err := s.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: nextRevision, Open: true, Version: gate.Version, UpdatedAt: baseTime.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("advance gate revision: %v", err)
	}
	if nextGate.Version != 2 {
		t.Fatalf("advanced gate version = %d, want 2", nextGate.Version)
	}

	// Exact ambiguity recovery is allowed even after the current revision has
	// advanced, but neither a new old-revision Task nor a Task rebind is.
	if got, err := s.CreateAgentExecutionBindingReservation(ctx, firstCandidate); err != nil || got.ID != first.ID {
		t.Fatalf("retry old-revision reservation = %+v, %v", got, err)
	}
	if _, err := s.CreateAgentExecutionBindingReservation(ctx,
		bindingReservationTestCandidate(revision, "task-uid-old-revision", baseTime.Add(4*time.Second))); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("new old-revision reservation error = %v, want ErrConflict", err)
	}
	if _, err := s.CreateAgentExecutionBindingReservation(ctx,
		bindingReservationTestCandidate(nextRevision, first.TaskUID, baseTime.Add(4*time.Second))); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-revision Task rebind error = %v, want ErrConflict", err)
	}

	second, err := s.CreateAgentExecutionBindingReservation(ctx,
		bindingReservationTestCandidate(nextRevision, "task-uid-next", baseTime.Add(5*time.Second)))
	if err != nil {
		t.Fatalf("create next-revision reservation: %v", err)
	}
	if second.Revision != nextRevision {
		t.Fatalf("next reservation revision = %+v, want %+v", second.Revision, nextRevision)
	}
	// The watermark is backend-wide, so either revision inventory proves it
	// observed the same complete mutation frontier.
	assertBindingReservationInventory(t, ctx, s, revision, 2, 1)
	assertBindingReservationInventory(t, ctx, s, nextRevision, 2, 1)
}

func TestAgentExecutionBindingReservationRejectedSettlementIsFenced(t *testing.T) {
	ctx := context.Background()
	s := newCoexistenceTestStore(t)
	baseTime := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	revision := bindingReservationTestRevision(store.AgentExecutionBackendV1, 7)
	if _, err := s.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision, Open: true, UpdatedAt: baseTime,
	}); err != nil {
		t.Fatalf("create gate: %v", err)
	}
	created, err := s.CreateAgentExecutionBindingReservation(ctx,
		bindingReservationTestCandidate(revision, "task-uid-rejected", baseTime.Add(time.Second)))
	if err != nil {
		t.Fatalf("create reservation: %v", err)
	}

	if _, err := s.SettleAgentExecutionBindingReservation(ctx, store.SettleAgentExecutionBindingReservationRequest{
		ID: created.ID, ExpectedVersion: created.Version + 1,
		TargetState:   store.AgentExecutionBindingReservationRejected,
		BindingDigest: created.BindingDigest, TerminalReason: "binding CAS was rejected",
		SettledAt: baseTime.Add(2 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale settlement error = %v, want ErrConflict", err)
	}
	if _, err := s.SettleAgentExecutionBindingReservation(ctx, store.SettleAgentExecutionBindingReservationRequest{
		ID: created.ID, ExpectedVersion: created.Version,
		TargetState:   store.AgentExecutionBindingReservationRejected,
		BindingDigest: created.BindingDigest, SettledAt: baseTime.Add(2 * time.Second),
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("reasonless rejection error = %v, want ErrValidation", err)
	}

	rejected, err := s.SettleAgentExecutionBindingReservation(ctx, store.SettleAgentExecutionBindingReservationRequest{
		ID: created.ID, ExpectedVersion: created.Version,
		TargetState:   store.AgentExecutionBindingReservationRejected,
		BindingDigest: created.BindingDigest, TerminalReason: "binding CAS was rejected",
		SettledAt: baseTime.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("reject reservation: %v", err)
	}
	if rejected.State != store.AgentExecutionBindingReservationRejected || rejected.TerminalReason == "" {
		t.Fatalf("rejected reservation = %+v", rejected)
	}

	if _, err := s.SettleAgentExecutionBindingReservation(ctx, store.SettleAgentExecutionBindingReservationRequest{
		ID: created.ID, ExpectedVersion: rejected.Version,
		TargetState:   store.AgentExecutionBindingReservationBound,
		BindingDigest: created.BindingDigest, SettledAt: baseTime.Add(3 * time.Second),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("terminal state rewrite error = %v, want ErrConflict", err)
	}
	got, err := s.GetAgentExecutionBindingReservation(ctx, created.ID)
	if err != nil {
		t.Fatalf("get rejected reservation: %v", err)
	}
	if got.State != rejected.State || got.Version != rejected.Version {
		t.Fatalf("stored reservation = %+v, want %+v", got, rejected)
	}
	assertBindingReservationInventory(t, ctx, s, revision, 2, 0)
}

func bindingReservationTestRevision(
	backend store.AgentExecutionBackendKey,
	modeRevision int64,
) store.AgentExecutionControlRevision {
	return store.AgentExecutionControlRevision{
		ControlUID: "control-uid", ControlGeneration: modeRevision, Backend: backend, ModeRevision: modeRevision,
	}
}

func bindingReservationTestCandidate(
	revision store.AgentExecutionControlRevision,
	taskUID string,
	reservedAt time.Time,
) store.AgentExecutionBindingReservation {
	return store.AgentExecutionBindingReservation{
		TaskNamespace: "tenant-a",
		TaskName:      "task-" + taskUID,
		TaskUID:       taskUID,
		Revision:      revision,
		BindingDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte("binding-" + taskUID)),
		SnapshotDigest: store.CanonicalAgentExecutionSnapshotDigest(
			[]byte("snapshot-" + taskUID),
		),
		ReservedAt: reservedAt,
	}
}

func assertBindingReservationInventory(
	t *testing.T,
	ctx context.Context,
	s *Store,
	revision store.AgentExecutionControlRevision,
	wantWatermark int64,
	wantOpen int,
) {
	t.Helper()
	inventory, err := s.ListAgentExecutionBindingReservations(ctx, revision)
	if err != nil {
		t.Fatalf("ListAgentExecutionBindingReservations(%s): %v", revision.CanonicalID(), err)
	}
	if inventory.Watermark != wantWatermark || inventory.OpenCount != wantOpen ||
		len(inventory.Reservations) != 1 {
		t.Fatalf("inventory = %+v, want watermark=%d open=%d reservations=%d",
			inventory, wantWatermark, wantOpen, 1)
	}
}
