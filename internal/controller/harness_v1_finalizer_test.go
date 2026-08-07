/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestHarnessV1LegacyCleanupDeletionUsesHistoricalBindingLineage(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	task := harnessV1LegacyCleanupFinalizerTask(
		"legacy-cleanup-terminal",
		"legacy-cleanup-terminal-uid",
		store.CanonicalAgentExecutionSnapshotDigest([]byte("synthetic-cleanup-binding")),
	)
	historicalDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("historical-execution-binding"))
	key := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, 1, historicalDigest, true)

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("legacy cleanup deletion: %v", err)
	}
	if !ready {
		t.Fatal("terminal historical lineage did not become deletion-ready")
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("historical attempt after reclamation = %v, want ErrNotFound", err)
	}
}

func TestHarnessV1LegacyCleanupDeletionRejectsMixedHistoricalLineages(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	task := harnessV1LegacyCleanupFinalizerTask(
		"legacy-cleanup-mixed",
		"legacy-cleanup-mixed-uid",
		store.CanonicalAgentExecutionSnapshotDigest([]byte("synthetic-cleanup-binding")),
	)
	first := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, 1,
		store.CanonicalAgentExecutionSnapshotDigest([]byte("historical-binding-a")), false)
	second := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, 2,
		store.CanonicalAgentExecutionSnapshotDigest([]byte("historical-binding-b")), false)

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err == nil || !strings.Contains(err.Error(), "multiple binding lineages") {
		t.Fatalf("mixed-lineage deletion = ready %t, err %v", ready, err)
	}
	for _, key := range []store.HarnessV1AttemptKey{first, second} {
		if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
			t.Fatalf("mixed-lineage attempt %s was mutated: %v", key.CanonicalID(), err)
		}
	}
}

func TestHarnessV1LegacyCleanupDeletionPreservesSessionOutboxBarrier(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	task := harnessV1LegacyCleanupFinalizerTask(
		"legacy-cleanup-session",
		"legacy-cleanup-session-uid",
		store.CanonicalAgentExecutionSnapshotDigest([]byte("synthetic-cleanup-binding")),
	)
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "legacy-session", Append: true}
	key := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, 1,
		store.CanonicalAgentExecutionSnapshotDigest([]byte("historical-session-binding")), true)

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("legacy cleanup Session barrier: %v", err)
	}
	if ready {
		t.Fatal("missing finalized SessionTurn/outbox projection allowed deletion")
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
		t.Fatalf("Session-barrier attempt was reclaimed: %v", err)
	}
}

func TestHarnessV1ExecuteDeletionStillRequiresCurrentBindingDigest(t *testing.T) {
	ctx := context.Background()
	durable, fence := newHarnessV1FinalizerStore(t)
	task := harnessV1LegacyCleanupFinalizerTask(
		"execute-binding-mismatch",
		"execute-binding-mismatch-uid",
		store.CanonicalAgentExecutionSnapshotDigest([]byte("current-execute-binding")),
	)
	task.Status.AgentExecutionBinding.Mode = corev1alpha1.AgentExecutionBindingModeExecute
	task.Status.AgentExecutionBinding.Provenance = corev1alpha1.AgentExecutionProvenanceNewlyBound
	task.Status.AgentExecutionBinding.MigrationInventoryID = ""
	key := createHarnessV1FinalizerAttempt(t, ctx, durable, fence, task, 1,
		store.CanonicalAgentExecutionSnapshotDigest([]byte("different-execute-binding")), true)

	r := &TaskReconciler{
		HarnessV1Attempts:      durable,
		ControllerEpochManager: readyHarnessV1TestEpochManager(durable, fence),
	}
	ready, err := r.harnessV1TaskDeletionReady(ctx, task)
	if err == nil || !strings.Contains(err.Error(), "binding changed") {
		t.Fatalf("execute binding mismatch = ready %t, err %v", ready, err)
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); err != nil {
		t.Fatalf("mismatched execute attempt was reclaimed: %v", err)
	}
}

func TestHarnessV1LegacyCleanupDeletionReclaimsOnlyInventoriedRuntimeWithoutAttempt(t *testing.T) {
	for _, test := range []struct {
		name            string
		changeEvidence  bool
		wantEvidenceErr bool
	}{
		{name: "exact inventory"},
		{name: "changed inventory", changeEvidence: true, wantEvidenceErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			durable, fence := newHarnessV1FinalizerStore(t)
			cipher, err := sqlite.NewAgentExecutionSnapshotCipher(
				bytes.Repeat([]byte{0x6a}, sqlite.AgentExecutionSnapshotKeyBytes),
			)
			if err != nil {
				t.Fatal(err)
			}
			durable.SetAgentExecutionSnapshotCipher(cipher)
			task := harnessV1LegacyCleanupFinalizerTask(
				"legacy-runtime-only-"+strings.ReplaceAll(test.name, " ", "-"),
				types.UID("legacy-runtime-only-"+strings.ReplaceAll(test.name, " ", "-")+"-uid"),
				store.CanonicalAgentExecutionSnapshotDigest([]byte("synthetic-runtime-only-binding-"+test.name)),
			)
			runtimeSession := &harness.RuntimeSession{
				ID: harness.RuntimeSessionID("runtime-only-" + strings.ReplaceAll(test.name, " ", "-")),
				Owner: harness.RuntimeSessionOwner{
					Namespace: task.Namespace, SessionName: "legacy-chat", ActiveTask: task.Name,
					Provider: harness.ProviderKindKubernetesService,
				},
				State: harness.RuntimeSessionStatePending, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
				CreatedAt: time.Date(2026, 8, 6, 21, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 8, 6, 21, 0, 0, 0, time.UTC),
			}
			if err := durable.CreateRuntimeSession(ctx, runtimeSession); err != nil {
				t.Fatal(err)
			}
			evidenced := *runtimeSession
			if test.changeEvidence {
				evidenced.State = harness.RuntimeSessionStateReady
			}
			persistHarnessV1LegacyCleanupSnapshot(t, ctx, durable, task, evidenced)

			r := &TaskReconciler{
				HarnessV1Attempts:       durable,
				AgentExecutionSnapshots: durable,
				ControllerEpochManager:  readyHarnessV1TestEpochManager(durable, fence),
			}
			ready, err := r.harnessV1TaskDeletionReady(ctx, task)
			if test.wantEvidenceErr {
				if err == nil || !strings.Contains(err.Error(), "do not match the encrypted migration inventory") {
					t.Fatalf("changed runtime evidence = ready %t, err %v", ready, err)
				}
				if _, getErr := durable.GetRuntimeSession(ctx, task.Namespace, runtimeSession.ID); getErr != nil {
					t.Fatalf("changed runtime row was reclaimed: %v", getErr)
				}
				return
			}
			if err != nil || !ready {
				t.Fatalf("exact runtime inventory = ready %t, err %v", ready, err)
			}
			if _, err := durable.GetRuntimeSession(ctx, task.Namespace, runtimeSession.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("inventoried runtime row after reclamation = %v, want ErrNotFound", err)
			}
			ready, err = r.harnessV1TaskDeletionReady(ctx, task)
			if err != nil || !ready {
				t.Fatalf("idempotent runtime-only retry = ready %t, err %v", ready, err)
			}
		})
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

func harnessV1LegacyCleanupFinalizerTask(
	name string,
	uid types.UID,
	bindingDigest string,
) *corev1alpha1.Task {
	snapshotDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("placeholder-cleanup-snapshot-" + name))
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
			SchemaVersion: 1, Mode: corev1alpha1.AgentExecutionBindingModeCleanupOnly,
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
			Backend:         corev1alpha1.AgentExecutionBackendHarnessWrapper,
			Provenance:      corev1alpha1.AgentExecutionProvenanceLegacyCleanupOnly,
			BindingDigest:   bindingDigest, MigrationInventoryID: "sealed-inventory-finalizer-test",
			Task: corev1alpha1.AgentExecutionBindingTaskRef{UID: uid, BoundSpecGeneration: 1},
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
	number int32,
	bindingDigest string,
	terminal bool,
) store.HarnessV1AttemptKey {
	t.Helper()
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
	reason := "LegacyCleanupOnly"
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

func persistHarnessV1LegacyCleanupSnapshot(
	t *testing.T,
	ctx context.Context,
	durable *sqlite.Store,
	task *corev1alpha1.Task,
	runtimeSession harness.RuntimeSession,
) {
	t.Helper()
	evidence := []agentExecutionClassificationEvidenceItem{classificationEvidenceItem(
		"LegacyRuntimeSession", task.Namespace, string(runtimeSession.ID), "", "", runtimeSession,
	)}
	sortClassificationEvidence(evidence)
	body, err := json.Marshal(legacyCleanupSnapshotBody{
		SchemaVersion:      store.AgentExecutionSnapshotSchemaVersion,
		MigrationInventory: task.Status.AgentExecutionBinding.MigrationInventoryID,
		TaskNamespace:      task.Namespace, TaskName: task.Name, TaskUID: string(task.UID),
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
		V1Evidence:      evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := store.CanonicalAgentExecutionSnapshotDigest(body)
	task.Status.AgentExecutionBinding.Snapshot = corev1alpha1.AgentExecutionSnapshotRef{
		ID: string(task.UID) + "/" + digest, Digest: digest,
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
	}
	if err := durable.PersistAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshot{
		TaskUID: string(task.UID), Digest: digest,
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion, Body: body,
		CreatedAt: time.Date(2026, 8, 6, 21, 1, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}
