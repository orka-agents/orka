/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

// One decision per attempt: a pre-submission requeue of the same attempt that
// reaches the same decision is neither logged nor counted again.
func TestACPPersistentSessionReuseDecisionCountsOncePerAttempt(t *testing.T) {
	dispatcher := &ACPDispatcher{}
	task := persistentTestTask()
	counter := metrics.ACPPersistentSessionTotal.WithLabelValues(acpPersistentSessionDecisionMiss, acpPersistentReasonNoRecord)
	before := counterValue(t, counter)
	for range 5 {
		reuse := &acpPersistentSessionReuse{dispatcher: dispatcher, attempted: true}
		reuse.miss(acpPersistentSessionStageClaim, acpPersistentReasonNoRecord)
		reuse.log(context.Background(), task)
	}
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("five requeues of one attempt counted %v decisions, want 1", got)
	}
	next := task.DeepCopy()
	next.Status.Execution.Attempt = 2
	reuse := &acpPersistentSessionReuse{dispatcher: dispatcher, attempted: true}
	reuse.miss(acpPersistentSessionStageClaim, acpPersistentReasonNoRecord)
	reuse.log(context.Background(), next)
	if got := counterValue(t, counter) - before; got != 2 {
		t.Fatalf("a new attempt's decision was not counted: %v", got)
	}
	dispatcher.forgetPersistentReuseDecision(task)
	reuse = &acpPersistentSessionReuse{dispatcher: dispatcher, attempted: true}
	reuse.miss(acpPersistentSessionStageClaim, acpPersistentReasonNoRecord)
	reuse.log(context.Background(), task)
	if got := counterValue(t, counter) - before; got != 3 {
		t.Fatalf("an ended attempt's decision memory was not forgotten: %v", got)
	}
}

// The live stuck Session: its first turn kept nothing, and the second turn's
// Task was cancelled by its deadline while requeued before submission. That
// requeue had cleared the attempt's binding but left the attempt holding the
// Session lease and an open SessionTurn, so DELETE returned 409 forever. Here
// turn 1 also left a Resident record, which turn 2's lease acquisition claimed.
// Deletion must settle the abandoned lease and finish.
func TestSessionDeletionFinishesWhenTheLeaseHolderIsATerminalTask(t *testing.T) {
	ctx := context.Background()
	turn1 := persistentCompositeTask("stuck-turn-1", "44444444-0000-0000-0000-000000000001")
	turn2 := persistentCompositeTask("stuck-turn-2", "44444444-0000-0000-0000-000000000002")
	h := newPersistentCompositeHarness(t, turn1, turn2)
	reuseDigest := testControlDigestForDispatcher("reuse")
	h.keep(turn1, reuseDigest)
	fence := h.fence()
	attempt2 := h.reserve(turn2, fence)
	session2, err := h.prepare(turn2, fence, h.claimInput(fence, reuseDigest), &acpPersistentSessionReuse{attempted: true})
	require.NoError(t, err)
	require.NotNil(t, session2.ResidentClaimed)
	// The requeue before binding (the resident slot was refused): the
	// attempt returns to Reserved, unbound, still holding the lease and turn.
	require.NoError(t, h.dispatcher.requeuePreSubmissionTaskWithRuntimeBinding(ctx, turn2, attempt2, fence, errACPRuntimePoolNotAdmitting, &session2.Binding))
	attempt, err := h.controlStore.GetPromptAttempt(ctx, attempt2)
	require.NoError(t, err)
	require.Equal(t, store.PromptExecutionReserved, attempt.ExecutionState)
	require.Empty(t, attempt.SessionUID)
	// The deadline then cancelled it before runtime admission.
	require.NoError(t, h.dispatcher.transitionAttemptToCancelled(ctx, attempt2, fence, "timeout-before-admission",
		acpTaskTimeoutReason, "task deadline exceeded before runtime admission"))
	cancelled := &corev1alpha1.Task{}
	require.NoError(t, h.kube.Get(ctx, client.ObjectKeyFromObject(turn2), cancelled))
	cancelled.Status.Phase = corev1alpha1.TaskPhaseCancelled
	cancelled.Status.Execution.State = corev1alpha1.TaskExecutionStateCancelled
	cancelled.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeCancelled
	require.NoError(t, h.kube.Status().Update(ctx, cancelled))
	control := h.control()
	require.NotNil(t, control.Lease, "the stuck state holds turn 2's lease")
	require.Equal(t, string(turn2.UID), control.Lease.TaskUID)
	require.NotNil(t, control.ResidentRuntime)

	manager := NewSessionManager(h.sqlite)
	manager.SetACPSessionCleanup(h.controlStore, h.epochs)
	err = manager.DeleteSession(ctx, persistentCompositeNamespace, persistentCompositeSession)
	require.True(t, errors.Is(err, store.ErrConflict), "without lease settlement the stuck Session cannot be deleted: %v", err)

	manager.SetACPSessionLeaseRecovery(h.dispatcher.SettleAbandonedSessionLease)
	// The settlement finalizes turn 2's SessionTurn; deletion then waits only
	// for the outbox projector to deliver the turns' terminal projections.
	err = manager.DeleteSession(ctx, persistentCompositeNamespace, persistentCompositeSession)
	control = h.control()
	require.Nil(t, control.Lease, "deletion settled the terminal Task's lease (delete: %v)", err)
	require.Nil(t, control.ResidentRuntime, "the settlement retired the claimed resident record")
	projector := &ACPOutboxProjector{Client: h.kube, Store: h.controlStore, Epochs: h.epochs, WorkerID: "session-deletion-test"}
	require.NoError(t, projector.projectOnce(ctx))
	require.NoError(t, manager.DeleteSession(ctx, persistentCompositeNamespace, persistentCompositeSession))
	_, err = h.controlStore.GetSessionControl(ctx, persistentCompositeNamespace, persistentCompositeSession)
	require.ErrorIs(t, err, store.ErrNotFound, "the Session control and its resident record are gone")
}

// A Task still running keeps its lease: deletion stays a conflict.
func TestSessionDeletionKeepsTheLeaseOfARunningTurn(t *testing.T) {
	ctx := context.Background()
	turn := persistentCompositeTask("running-turn", "55555555-0000-0000-0000-000000000001")
	h := newPersistentCompositeHarness(t, turn)
	fence := h.fence()
	h.reserve(turn, fence)
	_, err := h.prepare(turn, fence, nil, nil)
	require.NoError(t, err)
	manager := NewSessionManager(h.sqlite)
	manager.SetACPSessionCleanup(h.controlStore, h.epochs)
	manager.SetACPSessionLeaseRecovery(h.dispatcher.SettleAbandonedSessionLease)
	err = manager.DeleteSession(ctx, persistentCompositeNamespace, persistentCompositeSession)
	require.True(t, errors.Is(err, store.ErrConflict), "a running turn's Session must not be deleted: %v", err)
	require.NotNil(t, h.control().Lease)
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, counter.Write(&metric))
	return metric.GetCounter().GetValue()
}
