/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func resumedAttachedWorkspaceQueueFixture(t *testing.T) (*workspacev1alpha1.ExecutionWorkspace, *corev1alpha1.RuntimePool) {
	t.Helper()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
	workspace.Spec.AttachmentEpoch = 7
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef:   workspacev1alpha1.ObjectIdentityReference{Name: acpTestAttachedTask, UID: types.UID("attached-task-uid")},
		Epoch:     7,
		ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	workspace.Status.AttachedEpoch = 7
	meta.SetStatusCondition(&workspace.Status.Conditions, metav1.Condition{
		Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue,
		Reason: string(workspacev1alpha1.ReasonReady), ObservedGeneration: workspace.Generation,
	})
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Generation = 1
	pool.Spec.DesiredReplicas = 1
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	return workspace, pool
}

func TestACPExecutionWorkspaceAdapterKeepsBusyResumedAttachment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		reason     string
		generation int64
		wantReady  bool
	}{
		{name: "at capacity", reason: corev1alpha1.RuntimePoolReasonAtCapacity, generation: 1, wantReady: true},
		{name: "stale capacity observation", reason: corev1alpha1.RuntimePoolReasonAtCapacity, generation: 0},
		{name: "admission withdrawn", reason: corev1alpha1.RuntimePoolReasonAdmissionClosed, generation: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace, pool := resumedAttachedWorkspaceQueueFixture(t)
			pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
				Type: corev1alpha1.RuntimePoolConditionAdmissionReady, Status: metav1.ConditionFalse,
				Reason: test.reason, ObservedGeneration: test.generation,
			})
			c := acpAdapterTestClient(t, acpAdapterProvider(), workspace, pool)
			reconcileACPWorkspaceAdapter(t, c, workspace)
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatal(err)
			}
			if !test.wantReady {
				if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateProvisioning || current.Status.AttachedEpoch != 0 {
					t.Fatalf("workspace = %s epoch=%d, want Provisioning with no enforced attachment", current.Status.State, current.Status.AttachedEpoch)
				}
				return
			}
			if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateAttached || current.Status.AttachedEpoch != 7 {
				t.Fatalf("busy resumed workspace = %s epoch=%d, want its existing attachment preserved", current.Status.State, current.Status.AttachedEpoch)
			}
			r := &TaskReconciler{Client: c, APIReader: c}
			if err := r.verifyACPWorkspaceReadyForPool(ctx, pool, workspace.Name, string(workspace.UID), "attached-task-uid"); err != nil {
				t.Fatalf("busy resumed Task lost its workspace handshake: %v", err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
				t.Fatalf("busy resumed pool was deleted: %v", err)
			}
			if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
				t.Fatal("preserving the existing attachment must not open new RuntimeSession admission")
			}
		})
	}
}

func TestACPWorkspacePoolHandshakeWaitsForResumedRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace, pool := resumedAttachedWorkspaceQueueFixture(t)
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
	pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	c := acpAdapterTestClient(t, acpAdapterProvider(), workspace, pool)

	// The adapter withdraws attachment between the queue's initial readiness
	// check and its uncached post-materialization handshake.
	reconcileACPWorkspaceAdapter(t, c, workspace)
	r := &TaskReconciler{Client: c, APIReader: c}
	if err := r.verifyACPWorkspaceReadyForPool(ctx, pool, workspace.Name, string(workspace.UID), "attached-task-uid"); !errors.Is(err, errACPWorkspaceRecoveryPending) {
		t.Fatalf("recovering workspace handshake = %v, want an explicit recovery wait", err)
	}
	currentPool := &corev1alpha1.RuntimePool{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), currentPool); err != nil {
		t.Fatalf("waiting for recovery deleted the data-bearing RuntimePool: %v", err)
	}
	if currentPool.UID != pool.UID || !currentPool.DeletionTimestamp.IsZero() {
		t.Fatal("recovery must preserve the exact data-bearing RuntimePool")
	}
	base := currentPool.DeepCopy()
	currentPool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	currentPool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	if err := c.Status().Patch(ctx, currentPool, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	reconcileACPWorkspaceAdapter(t, c, workspace)
	if err := r.verifyACPWorkspaceReadyForPool(ctx, currentPool, workspace.Name, string(workspace.UID), "attached-task-uid"); err != nil {
		t.Fatalf("the recovered workspace could not continue: %v", err)
	}
}

func TestACPWorkspacePoolRecoveryDoesNotBypassRevocation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
	}{
		{name: "quarantined", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
		}},
		{name: "admission withdrawn", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.CoreAdmission.AdmittedGeneration = 0
		}},
		{name: "attachment replaced", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Attachment.TaskRef.UID = types.UID("different-task-uid")
		}},
		{name: "attachment expired", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Attachment.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Minute))
		}},
		{name: "revocation started", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceRevocationStartedAnnotation] = "7 2026-08-23T00:00:00Z"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace, pool := resumedAttachedWorkspaceQueueFixture(t)
			pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			c := acpAdapterTestClient(t, acpAdapterProvider(), workspace, pool)
			reconcileACPWorkspaceAdapter(t, c, workspace)
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatal(err)
			}
			base := current.DeepCopy()
			test.mutate(current)
			if err := c.Patch(ctx, current, client.MergeFrom(base)); err != nil {
				t.Fatal(err)
			}
			r := &TaskReconciler{Client: c, APIReader: c}
			if err := r.verifyACPWorkspaceReadyForPool(ctx, pool, workspace.Name, string(workspace.UID), "attached-task-uid"); err == nil {
				t.Fatal("revoked authority must reject the workspace handshake")
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
				t.Fatalf("revoked workspace pool survived: %v", err)
			}
		})
	}
}
