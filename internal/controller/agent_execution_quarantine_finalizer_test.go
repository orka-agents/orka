/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAdjudicatedQuarantineCleanupRoutesConsumesOnlyExactAppliedResolution(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 23, 30, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(true, true)
	adjudication := adjudicationTestTaskRecord(
		t,
		namespace,
		task,
		corev1alpha1.AgentExecutionAdjudicationCleanupBoth,
		now,
	)
	reconciler, kubeClient := newAdjudicationTestReconciler(
		t,
		now,
		namespace,
		task,
		adjudication,
	)
	finalizer := &TaskReconciler{Client: kubeClient, APIReader: kubeClient}

	routes, consumed, err := finalizer.adjudicatedQuarantineCleanupRoutes(ctx, task)
	if err != nil {
		t.Fatalf("resolve quarantine without resolution: %v", err)
	}
	if consumed || routes.harnessV1 || routes.harnessV2 {
		t.Fatalf("unresolved quarantine routes = %#v consumed=%t", routes, consumed)
	}

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("prepare adjudication: %v", err)
	}
	applying := getAdjudicationForTest(t, kubeClient, request.NamespacedName)
	ref, err := newAgentExecutionResolutionRef(applying, now)
	if err != nil {
		t.Fatal(err)
	}
	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	currentTask.Status.AgentExecutionResolutionRef = ref
	if err := kubeClient.Status().Update(ctx, currentTask); err != nil {
		t.Fatalf("append crash-boundary resolution reference: %v", err)
	}

	routes, consumed, err = finalizer.adjudicatedQuarantineCleanupRoutes(ctx, currentTask)
	if err != nil {
		t.Fatalf("resolve Applying adjudication: %v", err)
	}
	if consumed || routes.harnessV1 || routes.harnessV2 {
		t.Fatalf("Applying adjudication was consumed: routes=%#v consumed=%t", routes, consumed)
	}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("finish Applied adjudication: %v", err)
	}
	appliedTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	routes, consumed, err = finalizer.adjudicatedQuarantineCleanupRoutes(ctx, appliedTask)
	if err != nil {
		t.Fatalf("resolve Applied adjudication: %v", err)
	}
	if !consumed || !routes.harnessV1 || !routes.harnessV2 {
		t.Fatalf("Applied CleanupBoth routes = %#v consumed=%t", routes, consumed)
	}
}

func TestAdjudicatedQuarantineCleanupRoutesRejectsMismatchedAppliedReference(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 23, 45, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(true, false)
	adjudication := adjudicationTestTaskRecord(
		t,
		namespace,
		task,
		corev1alpha1.AgentExecutionAdjudicationCleanupV1,
		now,
	)
	reconciler, kubeClient := newAdjudicationTestReconciler(
		t,
		now,
		namespace,
		task,
		adjudication,
	)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(adjudication)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("prepare adjudication: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("apply adjudication: %v", err)
	}

	currentTask := getAdjudicationTaskForTest(t, kubeClient, task.Namespace, task.Name)
	currentTask.Status.AgentExecutionResolutionRef.Action = corev1alpha1.AgentExecutionAdjudicationCleanupBoth
	digest, err := canonicalAgentExecutionResolutionRefDigest(
		currentTask.Namespace,
		currentTask.Status.AgentExecutionResolutionRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	currentTask.Status.AgentExecutionResolutionRef.ResolutionDigest = digest
	finalizer := &TaskReconciler{Client: kubeClient, APIReader: kubeClient}
	if _, consumed, err := finalizer.adjudicatedQuarantineCleanupRoutes(ctx, currentTask); err == nil {
		t.Fatalf("mismatched Applied reference error = nil, consumed=%t", consumed)
	}
}

func TestAgentExecutionAdjudicationReservedActionsAreRejected(t *testing.T) {
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	namespace := adjudicationTestNamespace()
	task := adjudicationTestQuarantinedTask(true, false)
	reconciler := &AgentExecutionAdjudicationReconciler{}
	for _, action := range []corev1alpha1.AgentExecutionAdjudicationAction{
		corev1alpha1.AgentExecutionAdjudicationConfirmV1Outcome,
		corev1alpha1.AgentExecutionAdjudicationConfirmV2Outcome,
		corev1alpha1.AgentExecutionAdjudicationMarkNoExecution,
		corev1alpha1.AgentExecutionAdjudicationAbandonOutcomeUnknown,
		corev1alpha1.AgentExecutionAdjudicationBootstrapNewLineage,
	} {
		t.Run(string(action), func(t *testing.T) {
			adjudication := adjudicationTestTaskRecord(t, namespace, task, action, now)
			reason := reconciler.staticRejection(adjudication)
			if reason == "" || !strings.Contains(reason, "unsupported until") {
				t.Fatalf("staticRejection(%s) = %q, want explicit unsupported rejection", action, reason)
			}
		})
	}
}

func TestAdjudicatedHarnessV1DeletionRetainsActiveAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task := adjudicationTestQuarantinedTask(true, false)
	task.Name = "quarantined-active-v1"
	task.UID = types.UID("quarantined-active-v1-uid")
	r := newUnitReconciler(newTestScheme(), task)

	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "quarantined-v1-finalizer-test")
	controlStore, err := storekube.NewComposite(r.Client, task.Namespace, durable)
	if err != nil {
		t.Fatal(err)
	}
	epochs := NewControllerEpochManager(controlStore, "quarantined-v1-finalizer").WithMirror(durable)
	epochCtx, cancelEpoch := context.WithCancel(ctx)
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("stop epoch manager: %v", err)
		}
	}()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bindingDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("quarantined-v1-binding"))
	attempt := &store.HarnessV1Attempt{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), Attempt: 1,
		BindingDigest:  bindingDigest,
		SnapshotDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte("quarantined-v1-snapshot")),
		RequestDigest:  store.CanonicalAgentExecutionSnapshotDigest([]byte("quarantined-v1-request")),
		TurnID:         "quarantined-v1-turn", State: store.HarnessV1AttemptPrepared,
		RetryClass: store.HarnessV1RetryClassNone,
	}
	if err := durable.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		t.Fatal(err)
	}
	r.HarnessV1Attempts = durable
	r.ControllerEpochManager = epochs

	ready, err := r.adjudicatedHarnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("active adjudicated v1 deletion: %v", err)
	}
	if ready {
		t.Fatal("active adjudicated v1 attempt unexpectedly allowed Task deletion")
	}
	key := store.HarnessV1AttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1}
	persisted, err := durable.GetHarnessV1Attempt(ctx, key)
	if err != nil {
		t.Fatalf("active attempt was reclaimed: %v", err)
	}

	reason := "BackendDisabled"
	if _, err := durable.TransitionHarnessV1Attempt(ctx, store.HarnessV1AttemptTransition{
		Key: key, ExpectedVersion: persisted.Version, ExpectedState: store.HarnessV1AttemptPrepared,
		TargetState: store.HarnessV1AttemptRejected, OperationID: "terminal-before-adjudicated-cleanup",
		OperationDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte("terminal-before-adjudicated-cleanup")),
		Fence:           fence, Updates: store.HarnessV1AttemptUpdates{TerminalReason: &reason},
	}); err != nil {
		t.Fatal(err)
	}
	ready, err = r.adjudicatedHarnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("terminal adjudicated v1 deletion: %v", err)
	}
	if !ready {
		t.Fatal("terminal adjudicated v1 attempt did not allow reclamation")
	}
	if _, err := durable.GetHarnessV1Attempt(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reclaimed attempt error = %v, want not found", err)
	}
}

func TestAdjudicatedHarnessV1DeletionReclaimsInventoriedRuntimeWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	task := adjudicationTestQuarantinedTask(true, false)
	task.Name = "quarantined-runtime-only-v1"
	task.UID = types.UID("quarantined-runtime-only-v1-uid")
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "quarantined-runtime-only-v1-test")
	runtimeSession := &harness.RuntimeSession{
		ID: "legacy-runtime-only-v1",
		Owner: harness.RuntimeSessionOwner{
			Namespace: task.Namespace, SessionName: "legacy-chat", ActiveTask: task.Name,
			Provider: harness.ProviderKindKubernetesService,
		},
		State: harness.RuntimeSessionStatePending, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: time.Date(2026, 8, 6, 23, 50, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 6, 23, 50, 0, 0, time.UTC),
	}
	if err := durable.CreateRuntimeSession(ctx, runtimeSession); err != nil {
		t.Fatal(err)
	}
	evidence := []agentExecutionClassificationEvidenceItem{classificationEvidenceItem(
		"LegacyRuntimeSession", task.Namespace, string(runtimeSession.ID), "", "", *runtimeSession,
	)}
	sortClassificationEvidence(evidence)
	task.Status.AgentExecutionQuarantine.V1EvidenceDigest = evidenceItemsDigest(evidence)

	r := &TaskReconciler{HarnessV1Attempts: durable}
	ready, err := r.adjudicatedHarnessV1TaskDeletionReady(ctx, task)
	if err != nil {
		t.Fatalf("runtime-only adjudicated v1 deletion: %v", err)
	}
	if !ready {
		t.Fatal("runtime-only adjudicated v1 deletion did not become ready")
	}
	if _, err := durable.GetRuntimeSession(ctx, task.Namespace, runtimeSession.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy runtime row error = %v, want not found", err)
	}
	ready, err = r.adjudicatedHarnessV1TaskDeletionReady(ctx, task)
	if err != nil || !ready {
		t.Fatalf("runtime-only adjudicated v1 retry = ready %t, err %v", ready, err)
	}
}
