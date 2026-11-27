package controller

import (
	"context"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func requestSpeculativeResume(t *testing.T) (*speculativeTaskFixture, *corev1alpha1.Task, *corev1alpha1.RuntimePool) {
	t.Helper()
	ctx := context.Background()
	f := newSpeculativeTaskFixture(t)
	pool := f.suspendAfterFirstTurnWith(t, speculativeTestRevision)
	continuation := f.continuation(t, "resume-continuation")
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, continuation, f.plan); err != nil || ready {
		t.Fatalf("resume request = (%v, %v), want pending", ready, err)
	}
	return f, f.reloadTask(t, continuation), pool
}

func TestSpeculativeResumeIsRequestedWithoutFlipping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, continuation, pool := requestSpeculativeResume(t)
	requested := f.workspace(t)
	if requested.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended until the RuntimePool publishes", requested.Spec.DesiredState)
	}
	if !acpWorkspaceSpeculativeResumePending(requested, time.Now()) ||
		!acpWorkspaceResumeDemandBelongsToTask(requested.Annotations[acpWorkspaceResumeRequestedAnnotation], string(continuation.UID)) ||
		requested.Annotations[acpWorkspaceResumedLineageAnnotation] != booleanTrueValue {
		t.Fatal("the resume request did not record demand, lineage, and an open start in one write")
	}
	intent, staged := parseACPWorkspaceAttachmentIntent(requested)
	if !staged || intent.TaskUID != continuation.UID || intent.Epoch != requested.Spec.AttachmentEpoch+1 {
		t.Fatalf("staged attachment = %q, want the continuation at the next epoch", requested.Annotations[acpWorkspaceAttachmentIntentAnnotation])
	}

	f.start(t, continuation, false, pool.UID)
	if active, _ := f.pool(t); active.Spec.DesiredReplicas != 1 {
		t.Fatal("the pending resume did not activate its pool")
	}
	demand := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.APIReader}
	if result, err := demand.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("demand reconcile = (%v, %v), want a held pool with a deadline requeue", result, err)
	}
	if active, _ := f.pool(t); active.Spec.DesiredReplicas != 1 {
		t.Fatal("the pending resume's pool was scaled down")
	}

	rival := f.continuation(t, "rival-continuation")
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, rival, f.plan); err != nil || ready {
		t.Fatalf("rival ensure = (%v, %v), want waiting", ready, err)
	}
	if after := f.workspace(t); after.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended ||
		!acpWorkspaceResumeDemandBelongsToTask(after.Annotations[acpWorkspaceResumeRequestedAnnotation], string(continuation.UID)) {
		t.Fatal("a rival disturbed the pending speculative resume")
	}

	installed := f.installStagedAttachment(t, true)
	if installed == nil || installed.TaskRef.UID != continuation.UID {
		t.Fatalf("installed attachment = %#v, want the continuation", installed)
	}
	if resumed := f.workspace(t); resumed.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatal("the publication did not flip the workspace")
	}
}

func TestCancelledSpeculativeResumeReleasesEverythingItStaged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, continuation, pool := requestSpeculativeResume(t)
	f.start(t, continuation, false, pool.UID)
	intent, _ := parseACPWorkspaceAttachmentIntent(f.workspace(t))

	f.settleUntilDone(t, continuation)
	settled := f.workspace(t)
	if settled.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended || settled.Spec.Attachment != nil {
		t.Fatal("settlement resumed or attached the workspace")
	}
	for _, key := range []string{acpWorkspaceSpeculativeStartAnnotation, acpWorkspaceAttachmentIntentAnnotation} {
		if _, present := settled.Annotations[key]; present {
			t.Fatalf("settlement kept %s", key)
		}
	}
	if acpWorkspaceResumeDemandBelongsToTask(settled.Annotations[acpWorkspaceResumeRequestedAnnotation], string(continuation.UID)) {
		t.Fatal("settlement kept the cancelled Task's demand")
	}
	if acpWorkspaceSpeculativeResumePending(settled, time.Now()) {
		t.Fatal("the RuntimePool could still resume for the cancelled Task")
	}
	if err := f.r.Get(ctx, types.NamespacedName{Namespace: f.key.Namespace, Name: intent.SecretName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("staged Secret = %v, want deleted", err)
	}
	if err := f.r.Get(ctx, types.NamespacedName{Namespace: f.key.Namespace, Name: attachmentLeaseName(f.key.Name)}, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("staged Lease = %v, want released", err)
	}
	demand := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.APIReader}
	if _, err := demand.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
		t.Fatal(err)
	}
	if active, _ := f.pool(t); active.Spec.DesiredReplicas != 0 {
		t.Fatal("the cancelled resume kept its pool")
	}
	rival := f.continuation(t, "rival-after-cancel")
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, rival, f.plan); err != nil || ready {
		t.Fatalf("rival ensure = (%v, %v)", ready, err)
	}
	if !acpWorkspaceResumeDemandBelongsToTask(f.workspace(t).Annotations[acpWorkspaceResumeRequestedAnnotation], string(rival.UID)) {
		t.Fatal("the rival could not request its resume")
	}
}

func TestExpiredSpeculativeResumeFallsBackToTheTasksOwnFlipAndAttach(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, continuation, _ := requestSpeculativeResume(t)
	staged, _ := parseACPWorkspaceAttachmentIntent(f.workspace(t))
	f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(time.Now().Add(-time.Second), continuation.UID)
	})
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, continuation, f.plan); err != nil || ready {
		t.Fatalf("fallback = (%v, %v), want the Task's own flip", ready, err)
	}
	flipped := f.workspace(t)
	if flipped.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatal("an expired speculative resume did not fall back to the Task's flip")
	}
	admitTestACPWorkspace(t, f.r, flipped)
	f.patchStatus(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStatePending
	})
	if _, ready := attachTestACPWorkspace(t, f.r, continuation, f.plan, f.key.Name); !ready {
		t.Fatal("the requester could not attach after the fallback flip")
	}
	attached := f.workspace(t)
	if attached.Spec.Attachment == nil || attached.Spec.Attachment.Epoch != staged.Epoch ||
		attached.Spec.Attachment.TokenSHA256 != staged.TokenSHA256 {
		t.Fatalf("attachment = %#v, want the staged attachment recovered", attached.Spec.Attachment)
	}
	if _, present := attached.Annotations[acpWorkspaceAttachmentIntentAnnotation]; present {
		t.Fatal("the attachment kept the staged intent")
	}
}

func TestInterruptedSpeculativeReleaseIsRetriedFromTheLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, continuation, _ := requestSpeculativeResume(t)
	intent, staged := parseACPWorkspaceAttachmentIntent(f.workspace(t))
	if !staged {
		t.Fatal("no staged attachment")
	}
	f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		delete(w.Annotations, acpWorkspaceAttachmentIntentAnnotation)
		delete(w.Annotations, acpWorkspaceSpeculativeStartAnnotation)
		delete(w.Annotations, acpWorkspaceResumeRequestedAnnotation)
	})
	f.settleUntilDone(t, continuation)
	if err := f.r.Get(ctx, types.NamespacedName{Namespace: f.key.Namespace, Name: intent.SecretName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("staged Secret = %v, want deleted", err)
	}
	if err := f.r.Get(ctx, types.NamespacedName{Namespace: f.key.Namespace, Name: attachmentLeaseName(f.key.Name)}, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("staged Lease = %v, want released so successors are not blocked", err)
	}
	rival := f.continuation(t, "successor-after-crash")
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, rival, f.plan); err != nil || ready {
		t.Fatalf("successor ensure = (%v, %v)", ready, err)
	}
	if _, staged := parseACPWorkspaceAttachmentIntent(f.workspace(t)); !staged {
		t.Fatal("the successor could not stage its attachment")
	}
}
