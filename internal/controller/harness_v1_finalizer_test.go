/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestHarnessV1DeletionReclaimsCurrentBindingLineage(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	bindingDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("current-execution-binding"))
	task := harnessV1FinalizerTask(
		"current-binding-terminal",
		"current-binding-terminal-uid",
		bindingDigest,
	)
	key := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, bindingDigest, true)

	r := newHarnessV1FinalizerReconciler(durable, fence)
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("harness v1 deletion: %v", err)
	}
	if !ready {
		t.Fatal("terminal current lineage did not become deletion-ready")
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current attempt after reclamation = %v, want ErrNotFound", err)
	}
}

func TestHarnessV1DeletionWaitsForTerminalAttempt(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	bindingDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("active-execution-binding"))
	task := harnessV1FinalizerTask("active-binding", "active-binding-uid", bindingDigest)
	key := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, bindingDigest, false)

	r := newHarnessV1FinalizerReconciler(durable, fence)
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("active harness v1 deletion: %v", err)
	}
	if ready {
		t.Fatal("active attempt became deletion-ready")
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
		t.Fatalf("active attempt was mutated: %v", err)
	}
}

func TestHarnessV1DeletionPreservesSessionOutboxBarrier(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	bindingDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("session-binding"))
	task := harnessV1FinalizerTask("session-binding", "session-binding-uid", bindingDigest)
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session", Append: true}
	key := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, bindingDigest, true)

	r := newHarnessV1FinalizerReconciler(durable, fence)
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("harness v1 Session barrier: %v", err)
	}
	if ready {
		t.Fatal("missing finalized SessionTurn/outbox projection allowed deletion")
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
		t.Fatalf("Session-barrier attempt was reclaimed: %v", err)
	}
}

func TestHarnessV1DeletionRequiresCurrentBindingDigest(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	task := harnessV1FinalizerTask(
		"binding-mismatch",
		"binding-mismatch-uid",
		store.CanonicalAgentExecutionSnapshotDigest([]byte("current-execution-binding")),
	)
	key := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task,
		store.CanonicalAgentExecutionSnapshotDigest([]byte("different-execution-binding")), true)

	r := newHarnessV1FinalizerReconciler(durable, fence)
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err == nil || !strings.Contains(err.Error(), "binding changed") {
		t.Fatalf("binding mismatch = ready %t, err %v", ready, err)
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
		t.Fatalf("mismatched attempt was reclaimed: %v", err)
	}
}

func TestHarnessV1DeletionRetainsAttemptUntilWrapperSettlementAcknowledged(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	bindingDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("wrapper-settlement-binding"))
	task := harnessV1FinalizerTask("wrapper-settlement", "wrapper-settlement-uid", bindingDigest)
	key, terminal := createHarnessV1FinalizerLedgerAttempt(t, ctx, durable, fence, task, bindingDigest)

	ackErr := errors.New("lost wrapper settlement acknowledgement response")
	acknowledger := &recordingHarnessV1SettlementAcknowledger{failures: 1, err: ackErr}
	r := newHarnessV1FinalizerReconciler(durable, fence)
	r.HarnessV1SettlementAcknowledger = acknowledger

	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if ready || !errors.Is(err, ackErr) {
		t.Fatalf("first deletion readiness = %t, %v, want false and acknowledgement error", ready, err)
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
		t.Fatalf("attempt was reclaimed before wrapper acknowledgement: %v", err)
	}

	ready, err = r.harnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("retry harness v1 deletion: %v", err)
	}
	if !ready {
		t.Fatal("acknowledged wrapper attempt did not become deletion-ready")
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("acknowledged attempt after reclamation = %v, want ErrNotFound", err)
	}
	if len(acknowledger.calls) != 2 {
		t.Fatalf("wrapper settlement acknowledgement calls = %d, want 2", len(acknowledger.calls))
	}
	for i := range acknowledger.calls {
		call := acknowledger.calls[i]
		if call.taskUID != task.UID || call.attempt != terminal.Attempt || call.turnID != terminal.TurnID ||
			call.requestDigest != terminal.RequestDigest || call.terminalReceiptDigest != terminal.TerminalReceiptDigest {
			t.Fatalf("wrapper settlement acknowledgement call %d = %+v, want attempt %+v", i+1, call, terminal)
		}
	}
}

func newHarnessV1FinalizerReconciler(
	durable *sqlite.Store,
	fence store.ControllerEpochFence,
) *TaskReconciler {
	return &TaskReconciler{
		HarnessV1Attempts:               durable,
		HarnessV1SettlementAcknowledger: &recordingHarnessV1SettlementAcknowledger{},
		ControllerEpochManager:          readyHarnessV1TestEpochManager(durable, fence),
	}
}

func newHarnessV1FinalizerStore(t *testing.T) (*sqlite.Store, store.ControllerEpochFence) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "harness-v1-finalizer-test")
	return durable, seedHarnessV1AttemptEpoch(t, durable)
}

func harnessV1FinalizerTask(
	name string,
	uid types.UID,
	bindingDigest string,
) *corev1alpha1.Task {
	snapshotDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("snapshot-" + name))
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
			SchemaVersion:   1,
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
			Backend:         corev1alpha1.AgentExecutionBackendHarnessWrapper,
			BindingDigest:   bindingDigest,
			Task: corev1alpha1.AgentExecutionBindingTaskRef{
				UID: uid, BoundSpecGeneration: 1,
			},
			Snapshot: corev1alpha1.AgentExecutionSnapshotRef{
				ID: string(uid) + "/" + snapshotDigest, Digest: snapshotDigest,
				SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
			},
		}},
	}
}

func createHarnessV1FinalizerAttempt(
	t *testing.T,
	ctx context.Context,
	durable *sqlite.Store,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	bindingDigest string,
	terminal bool,
) store.HarnessV1AttemptKey {
	t.Helper()
	const number int32 = 1
	key := store.HarnessV1AttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: number}
	attempt := &store.HarnessV1Attempt{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), Attempt: number,
		BindingDigest:  bindingDigest,
		SnapshotDigest: store.CanonicalAgentExecutionSnapshotDigest(fmt.Appendf(nil, "snapshot-%s-%d", task.UID, number)),
		RequestDigest:  store.CanonicalAgentExecutionSnapshotDigest(fmt.Appendf(nil, "request-%s-%d", task.UID, number)),
		TurnID:         fmt.Sprintf("turn-%s-%d", task.UID, number),
		Backend:        string(corev1alpha1.AgentExecutionBackendHarnessWrapper),
		State:          store.HarnessV1AttemptPrepared,
		RetryClass:     store.HarnessV1RetryClassNone,
	}
	if err := durable.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		t.Fatal(err)
	}
	if !terminal {
		return key
	}
	reason := "Terminal"
	operation := fmt.Sprintf("terminal-%s-%d", task.UID, number)
	if _, err := durable.TransitionHarnessV1Attempt(ctx, store.HarnessV1AttemptTransition{
		Key: key, ExpectedVersion: 1, ExpectedState: store.HarnessV1AttemptPrepared,
		TargetState: store.HarnessV1AttemptRejected, OperationID: operation,
		OperationDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte(operation)), Fence: fence,
		Updates: store.HarnessV1AttemptUpdates{TerminalReason: &reason},
	}); err != nil {
		t.Fatal(err)
	}
	return key
}

func createHarnessV1FinalizerLedgerAttempt(
	t *testing.T,
	ctx context.Context,
	durable *sqlite.Store,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	bindingDigest string,
) (store.HarnessV1AttemptKey, *store.HarnessV1Attempt) {
	t.Helper()
	const number int32 = 1
	key := store.HarnessV1AttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: number}
	attempt := &store.HarnessV1Attempt{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), Attempt: number,
		BindingDigest:  bindingDigest,
		SnapshotDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte("wrapper-settlement-snapshot")),
		RequestDigest:  store.CanonicalAgentExecutionSnapshotDigest([]byte("wrapper-settlement-request")),
		TurnID:         "wrapper-settlement-turn",
		Backend:        string(corev1alpha1.AgentExecutionBackendHarnessWrapper),
		State:          store.HarnessV1AttemptPrepared,
		RetryClass:     store.HarnessV1RetryClassNone,
	}
	if err := durable.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		t.Fatal(err)
	}
	transitions := []struct {
		from    store.HarnessV1AttemptState
		to      store.HarnessV1AttemptState
		updates store.HarnessV1AttemptUpdates
	}{
		{from: store.HarnessV1AttemptPrepared, to: store.HarnessV1AttemptSubmitting},
		{from: store.HarnessV1AttemptSubmitting, to: store.HarnessV1AttemptAccepted},
		{
			from: store.HarnessV1AttemptAccepted,
			to:   store.HarnessV1AttemptSucceeded,
			updates: store.HarnessV1AttemptUpdates{
				TerminalReason:        new("Succeeded"),
				TerminalReceiptDigest: new(store.CanonicalAgentExecutionSnapshotDigest([]byte("wrapper-settlement-receipt"))),
			},
		},
	}
	current, err := durable.GetHarnessV1Attempt(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for i := range transitions {
		transition := transitions[i]
		operation := fmt.Sprintf("wrapper-settlement-%d", i+1)
		updated, err := durable.TransitionHarnessV1Attempt(ctx, store.HarnessV1AttemptTransition{
			Key: key, ExpectedVersion: current.Version, ExpectedState: transition.from,
			TargetState: transition.to, OperationID: operation,
			OperationDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte(operation)),
			Fence:           fence, Updates: transition.updates,
		})
		if err != nil {
			t.Fatal(err)
		}
		current = updated
	}
	return key, current
}

type harnessV1SettlementAcknowledgementCall struct {
	taskUID               types.UID
	attempt               int32
	turnID                string
	requestDigest         string
	terminalReceiptDigest string
}

type recordingHarnessV1SettlementAcknowledger struct {
	failures int
	err      error
	calls    []harnessV1SettlementAcknowledgementCall
}

func (r *recordingHarnessV1SettlementAcknowledger) AcknowledgeHarnessV1Settlement(
	_ context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
) error {
	r.calls = append(r.calls, harnessV1SettlementAcknowledgementCall{
		taskUID: task.UID, attempt: attempt.Attempt, turnID: attempt.TurnID,
		requestDigest: attempt.RequestDigest, terminalReceiptDigest: attempt.TerminalReceiptDigest,
	})
	if r.failures > 0 {
		r.failures--
		return r.err
	}
	return nil
}
