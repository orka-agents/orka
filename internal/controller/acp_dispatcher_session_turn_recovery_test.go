/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// sessionTurnLookupStore serves exactly the GetSessionTurn lookup the
// turn-aware recovery decision performs.
const acpTestTurnRecoverySessionUID = "session-uid-1"

const acpTestTurnRecoveryPromptID = "prompt-1"

type sessionTurnLookupStore struct {
	store.DurableControlStore
	turns map[string]*store.SessionTurn
}

func (s *sessionTurnLookupStore) GetSessionTurn(_ context.Context, id string) (*store.SessionTurn, error) {
	turn, ok := s.turns[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return turn, nil
}

// A Finalizing Task whose succeeded, delivery-terminal attempt still has an
// open SessionTurn must be picked up for terminal recovery; inline settle
// finalization can silently skip, and without this check artifact retirement
// blocks on "SessionTurn is not finalized" until the deadline fails the Task.
func TestSessionTurnRequiresTerminalRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	attempt := &store.PromptAttempt{
		ID: "attempt-1",
		Key: store.PromptAttemptKey{
			Namespace: runtimePoolDefaultControllerNamespace, TaskUID: "task-uid-1", Attempt: 1, PromptID: acpTestTurnRecoveryPromptID,
		},
		SessionUID: acpTestTurnRecoverySessionUID, SessionLeaseGeneration: 1,
		ExecutionState: store.PromptExecutionSucceeded,
		DeliveryState:  store.PromptDeliveryNotRequested,
	}
	key := store.SessionTurnKey{
		SessionUID: acpTestTurnRecoverySessionUID, LeaseGeneration: 1,
		TaskUID: "task-uid-1", Attempt: 1, PromptID: acpTestTurnRecoveryPromptID,
	}
	turnID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{}
	task.Status.Phase = corev1alpha1.TaskPhaseFinalizing

	openStore := &sessionTurnLookupStore{turns: map[string]*store.SessionTurn{
		turnID: {ID: turnID, State: store.SessionTurnOpen},
	}}
	d := &ACPDispatcher{Store: openStore}
	if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, task, attempt); err != nil || !needs {
		t.Fatalf("open turn on a Finalizing Task = (%v, %v), want recovery", needs, err)
	}

	openStore.turns[turnID].State = store.SessionTurnFinalized
	if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, task, attempt); err != nil || needs {
		t.Fatalf("finalized turn = (%v, %v), want no recovery", needs, err)
	}

	openStore.turns[turnID].State = store.SessionTurnOpen
	// A settled terminal phase with an open turn also recovers: a finalizer
	// that silently skipped its missing in-memory turn still terminalizes
	// the Task, so the settled phase is never proof of a finalized turn.
	for _, phase := range []corev1alpha1.TaskPhase{
		corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled,
	} {
		settled := task.DeepCopy()
		settled.Status.Phase = phase
		if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, settled, attempt); err != nil || !needs {
			t.Fatalf("settled phase %s with an open turn = (%v, %v), want recovery", phase, needs, err)
		}
	}
	pending := task.DeepCopy()
	pending.Status.Phase = corev1alpha1.TaskPhaseRunning
	if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, pending, attempt); err != nil || needs {
		t.Fatalf("non-terminal phase = (%v, %v), want no lookup-driven recovery", needs, err)
	}

	unbound := *attempt
	unbound.SessionUID, unbound.SessionLeaseGeneration = "", 0
	if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, task, &unbound); err != nil || needs {
		t.Fatalf("session-unbound attempt = (%v, %v), want no recovery", needs, err)
	}

	// Every non-success terminal state with an open turn also recovers: their
	// finalizers can equally lose the in-memory turn, and delivery is not a
	// prerequisite for Failed, Cancelled, or OutcomeUnknown attempts.
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionFailed, store.PromptExecutionCancelled, store.PromptExecutionOutcomeUnknown,
	} {
		terminal := *attempt
		terminal.ExecutionState = state
		terminal.DeliveryState = store.PromptDeliveryNotRequested
		if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, task, &terminal); err != nil || !needs {
			t.Fatalf("open turn with terminal state %s = (%v, %v), want recovery", state, needs, err)
		}
	}

	// A succeeded attempt without terminal delivery still belongs to
	// publication recovery, never to turn recovery.
	undelivered := *attempt
	undelivered.DeliveryState = store.PromptDeliveryPublishing
	if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, task, &undelivered); err != nil || needs {
		t.Fatalf("undelivered succeeded attempt = (%v, %v), want publication recovery to own it", needs, err)
	}

	// A non-terminal execution state never triggers turn recovery.
	running := *attempt
	running.ExecutionState = store.PromptExecutionRunning
	if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, task, &running); err != nil || needs {
		t.Fatalf("running attempt = (%v, %v), want no recovery", needs, err)
	}

	d.Store = &sessionTurnLookupStore{turns: map[string]*store.SessionTurn{}}
	if needs, err := d.sessionTurnRequiresTerminalRecovery(ctx, task, attempt); err != nil || needs {
		t.Fatalf("missing turn record = (%v, %v), want no recovery", needs, err)
	}
}
