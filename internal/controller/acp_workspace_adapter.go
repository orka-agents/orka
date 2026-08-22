/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const acpWorkspaceAdapterRequeue = 2 * time.Second

// ACPExecutionWorkspaceAdapterReconciler is the in-tree status adapter for
// class-backed ACP execution workspaces. It owns exactly the adapter side of
// the generic contract: sanitized provider-observed state, the enforced
// attachment epoch, deletion progress, and the terminal cleanup disposition.
// The physical workload stays owned by the RuntimePool backends; this adapter
// drives them only through the linked RuntimePool object and never touches
// provider-native resources directly.
type ACPExecutionWorkspaceAdapterReconciler struct {
	client.Client
	APIReader client.Reader
}

//nolint:gocyclo // The adapter state machine keeps every desired-state branch auditable in one place.
func (r *ACPExecutionWorkspaceAdapterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owned, exact, err := r.workspaceOwnership(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !owned {
		return ctrl.Result{}, nil
	}

	maintenance := !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	if maintenance {
		return r.reconcileMaintenance(ctx, workspace)
	}
	// Attachment revocation must converge even while re-admission for the
	// bumped generation is still pending, mirroring the core controller's
	// revocation bypass; otherwise detach and re-admission deadlock.
	if exact && workspaceNeedsAttachmentRevocation(workspace) {
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
			status.AttachedEpoch = 0
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse,
				Reason:             string(workspacev1alpha1.ReasonAttachmentRevoked),
				Message:            "attachment intent was revoked; the enforced epoch is cleared",
				ObservedGeneration: workspace.Generation,
			})
		})
	}
	// Normal lifecycle requires the exact provider binding and a current core
	// admission; anything else stays unserved and fails closed.
	if !exact || !workspaceCurrentlyAdmittedByCore(workspace) {
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady {
			logf.FromContext(ctx).Info("ACP workspace adapter holding a resume request",
				"workspace", workspace.Name, "generation", workspace.Generation,
				"exact", exact, "coreAdmitted", workspaceCurrentlyAdmittedByCore(workspace))
			// Core admission is normally observed through a workspace event,
			// but a resume must never strand on a missed one.
			return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, nil
		}
		return ctrl.Result{}, nil
	}

	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		if pool, foreign, poolErr := r.linkedRuntimePool(ctx, workspace); poolErr == nil {
			suspended := pool != nil && strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceSuspendAnnotation]) != ""
			if foreign || pool == nil || suspended || (pool != nil && pool.Spec.DesiredReplicas == 0) {
				logf.FromContext(ctx).Info("ACP workspace adapter serving a Ready workspace",
					"workspace", workspace.Name, "generation", workspace.Generation,
					"poolFound", pool != nil, "poolForeign", foreign,
					"poolSuspendIntent", suspended,
					"poolReplicas", func() int32 {
						if pool == nil {
							return -1
						}
						return pool.Spec.DesiredReplicas
					}())
			}
		}
		if requeue, err := r.driveLinkedRuntimePoolResume(ctx, workspace); err != nil || requeue {
			return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, err
		}
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			if workspace.Spec.Attachment != nil {
				status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
				status.AttachedEpoch = workspace.Spec.Attachment.Epoch
				workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
					Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue,
					Reason:             string(workspacev1alpha1.ReasonReady),
					Message:            "the attached Task holds the exclusive RuntimeSession writer",
					ObservedGeneration: workspace.Generation,
				})
			} else {
				status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
				status.AttachedEpoch = 0
				workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
					Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse,
					Reason:             string(workspacev1alpha1.ReasonAttachmentRevoked),
					Message:            "no Task attachment is active",
					ObservedGeneration: workspace.Generation,
				})
			}
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionTrue,
				Reason:             string(workspacev1alpha1.ReasonReady),
				Message:            "the class-backed RuntimeSession binding is provisioned",
				ObservedGeneration: workspace.Generation,
			})
		})
	case workspacev1alpha1.ExecutionWorkspaceDesiredSuspended:
		return r.reconcileSuspension(ctx, workspace)
	default:
		return ctrl.Result{}, nil
	}
}

// reconcileSuspension drives a requested data-only suspension through the
// linked RuntimePool and reports sanitized progress. The pool backend owns
// every provider call; this adapter only records the intent and observes the
// consensual checkpoint record.
func (r *ACPExecutionWorkspaceAdapterReconciler) reconcileSuspension(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (ctrl.Result, error) {
	pool, foreign, err := r.linkedRuntimePool(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if foreign || pool == nil || !runtimePoolWorkspaceSuspendCapable(pool) {
		// No suspend-capable physical runtime backs this workspace; nothing
		// durable exists to resume into, so the suspension fails closed.
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
			status.AttachedEpoch = 0
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse,
				Reason:             string(workspacev1alpha1.ReasonCleanupFailed),
				Message:            "no suspend-capable linked RuntimePool backs this workspace; the requested suspension cannot preserve data",
				ObservedGeneration: workspace.Generation,
			})
		})
	}
	changed, err := r.patchLinkedPoolSuspendIntent(ctx, pool, true)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, nil
	}
	settled := pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped &&
		pool.Status.ObservedGeneration == pool.Generation
	consent := runtimePoolWorkspaceSuspendConsentRecorded(pool)
	switch {
	case settled && consent:
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
			status.AttachedEpoch = 0
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionTrue,
				Reason:             string(workspacev1alpha1.ReasonReady),
				Message:            "the data-only workspace checkpoint is settled; cold resume restores the logical session with a fresh boot",
				ObservedGeneration: workspace.Generation,
			})
		})
	case settled:
		// The pool stopped without a recorded consensual checkpoint: the
		// actor was lost or recycled before suspension, so no durable data
		// exists and resume must fail closed instead of fabricating one.
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
			status.AttachedEpoch = 0
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse,
				Reason:             string(workspacev1alpha1.ReasonCleanupFailed),
				Message:            "the physical runtime settled without a data-only checkpoint; the requested suspension preserved no workspace data",
				ObservedGeneration: workspace.Generation,
			})
		})
	default:
		return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, r.patchWorkspaceStatus(ctx, workspace,
			func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
				status.ObservedGeneration = workspace.Generation
				status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspending
				status.AttachedEpoch = 0
			})
	}
}

// driveLinkedRuntimePoolResume lifts a prior suspension intent when the
// workspace returns to Ready so the pool backend cold-resumes the actor.
func (r *ACPExecutionWorkspaceAdapterReconciler) driveLinkedRuntimePoolResume(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	pool, foreign, err := r.linkedRuntimePool(ctx, workspace)
	if err != nil || foreign || pool == nil {
		return false, err
	}
	if strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceSuspendAnnotation]) == "" {
		return false, nil
	}
	logf.FromContext(ctx).Info("ACP workspace adapter lifting the pool suspension intent for resume",
		"workspace", workspace.Name, "pool", pool.Name)
	return r.patchLinkedPoolSuspendIntent(ctx, pool, false)
}

// linkedRuntimePool resolves the workspace's linked pool; foreign reports a
// same-name pool that is not linked to this workspace.
func (r *ACPExecutionWorkspaceAdapterReconciler) linkedRuntimePool(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (*corev1alpha1.RuntimePool, bool, error) {
	poolName := strings.TrimSpace(workspace.Annotations[acpExecutionWorkspacePoolAnnotation])
	if poolName == "" {
		return nil, false, nil
	}
	pool := &corev1alpha1.RuntimePool{}
	err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: poolName}, pool)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if pool.Labels[acpExecutionWorkspaceLinkLabel] != workspace.Name || pool.Spec.ExecutionWorkspace == nil {
		return nil, true, nil
	}
	return pool, false, nil
}

// patchLinkedPoolSuspendIntent records or lifts the suspension intent and the
// matching desired scale on the linked pool. It reports whether a write
// happened so callers requeue on the fresh object.
func (r *ACPExecutionWorkspaceAdapterReconciler) patchLinkedPoolSuspendIntent(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	suspend bool,
) (bool, error) {
	desiredReplicas := int32(1)
	if suspend {
		desiredReplicas = 0
	}
	intentSet := strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceSuspendAnnotation]) != ""
	if intentSet == suspend && pool.Spec.DesiredReplicas == desiredReplicas {
		return false, nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	if suspend {
		pool.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
	} else {
		delete(pool.Annotations, runtimePoolWorkspaceSuspendAnnotation)
	}
	pool.Spec.DesiredReplicas = desiredReplicas
	if err := r.Patch(ctx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return true, nil
		}
		return false, err
	}
	return true, nil
}

// workspaceOwnership reports whether this adapter serves the workspace, and
// whether the live provider binding is exact. Deletion may proceed on the
// controller-name label alone; normal lifecycle requires the exact provider.
func (r *ACPExecutionWorkspaceAdapterReconciler) workspaceOwnership(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, bool, error) {
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	err := r.Get(ctx, types.NamespacedName{Name: workspace.Spec.ProviderBinding.Name}, provider)
	if apierrors.IsNotFound(err) {
		labeled := workspace.Labels[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceControllerLabelValue
		return labeled, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if provider.Spec.ControllerName != acpWorkspaceProviderControllerName {
		return false, false, nil
	}
	exact := provider.UID == workspace.Spec.ProviderBinding.UID
	return true, exact, nil
}

// reconcileMaintenance handles Deleted and Quarantined intent plus object
// deletion: tear down the linked RuntimePool through its own authenticated
// drain and finalizer, then report the terminal state and disposition.
func (r *ACPExecutionWorkspaceAdapterReconciler) reconcileMaintenance(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (ctrl.Result, error) {
	poolGone, foreign, err := r.ensureLinkedRuntimePoolDeleted(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if foreign {
		// A same-name pool that is not linked to this workspace is never
		// deleted; report the blocker and hold the terminal disposition so
		// core finalization fails closed instead of releasing the finalizer.
		return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, r.patchWorkspaceStatus(ctx, workspace,
			func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
				status.ObservedGeneration = workspace.Generation
				workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
					Type: string(workspacev1alpha1.ConditionWorkspaceFinalized), Status: metav1.ConditionFalse,
					Reason:             string(workspacev1alpha1.ReasonCleanupFailed),
					Message:            "a RuntimePool with the linked name is not owned by this workspace; refusing to delete a foreign object",
					ObservedGeneration: workspace.Generation,
				})
			})
	}
	if !poolGone {
		return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, r.patchWorkspaceStatus(ctx, workspace,
			func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
				status.ObservedGeneration = workspace.Generation
				status.State = workspacev1alpha1.ExecutionWorkspaceStateDeleting
			})
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined && workspace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			status.State = workspacev1alpha1.ExecutionWorkspaceStateQuarantined
			status.AttachedEpoch = 0
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceQuarantined), Status: metav1.ConditionTrue,
				Reason:             string(workspacev1alpha1.ReasonQuarantined),
				Message:            "workspace is quarantined; its RuntimePool was destroyed and it will never be reused",
				ObservedGeneration: workspace.Generation,
			})
		})
	}
	return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
		status.ObservedGeneration = workspace.Generation
		status.State = workspacev1alpha1.ExecutionWorkspaceStateDeleted
		status.AttachedEpoch = 0
		status.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{
			Compute:           workspacev1alpha1.DispositionDeleted,
			AccessCredentials: workspacev1alpha1.DispositionRevoked,
			EphemeralSecrets:  workspacev1alpha1.DispositionDeleted,
			WorkspaceData:     workspacev1alpha1.DispositionDeleted,
			PersistentVolumes: workspacev1alpha1.DispositionNotApplicable,
			Checkpoints:       workspacev1alpha1.DispositionNotApplicable,
			ProviderResources: workspacev1alpha1.DispositionDeleted,
		}
		workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
			Type: string(workspacev1alpha1.ConditionWorkspaceFinalized), Status: metav1.ConditionTrue,
			Reason:             string(workspacev1alpha1.ReasonReady),
			Message:            "linked RuntimePool and its provider children are deleted",
			ObservedGeneration: workspace.Generation,
		})
	})
}

// ensureLinkedRuntimePoolDeleted deletes the RuntimePool linked to this
// workspace and reports whether it is gone. A pool that carries the linked
// name but not this workspace's link identity is foreign and never deleted.
func (r *ACPExecutionWorkspaceAdapterReconciler) ensureLinkedRuntimePoolDeleted(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, bool, error) {
	poolName := strings.TrimSpace(workspace.Annotations[acpExecutionWorkspacePoolAnnotation])
	if poolName == "" {
		return true, false, nil
	}
	pool := &corev1alpha1.RuntimePool{}
	err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: poolName}, pool)
	if apierrors.IsNotFound(err) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if pool.Labels[acpExecutionWorkspaceLinkLabel] != workspace.Name || pool.Spec.ExecutionWorkspace == nil {
		return false, true, nil
	}
	if pool.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, pool, client.Preconditions{UID: &pool.UID}); err != nil && !apierrors.IsNotFound(err) {
			return false, false, err
		}
	}
	return false, false, nil
}

func (r *ACPExecutionWorkspaceAdapterReconciler) patchWorkspaceStatus(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	mutate func(*workspacev1alpha1.ExecutionWorkspaceStatus),
) error {
	before := workspace.DeepCopy()
	mutate(&workspace.Status)
	if err := r.Status().Patch(ctx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch ACP workspace status: %w", err)
	}
	return nil
}

// SetupWithManager registers the adapter for ExecutionWorkspaces and requeues
// them as their linked RuntimePools progress through deletion.
func (r *ACPExecutionWorkspaceAdapterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		Watches(&corev1alpha1.RuntimePool{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, object client.Object) []ctrl.Request {
				name := strings.TrimSpace(object.GetLabels()[acpExecutionWorkspaceLinkLabel])
				if name == "" {
					return nil
				}
				return []ctrl.Request{{NamespacedName: types.NamespacedName{
					Namespace: object.GetNamespace(), Name: name,
				}}}
			},
		)).
		Named("acp-execution-workspace-adapter").
		Complete(r)
}
