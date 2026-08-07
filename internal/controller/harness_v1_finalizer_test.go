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

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
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

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
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

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
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

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err == nil || !strings.Contains(err.Error(), "binding changed") {
		t.Fatalf("binding mismatch = ready %t, err %v", ready, err)
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
		t.Fatalf("mismatched attempt was reclaimed: %v", err)
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
