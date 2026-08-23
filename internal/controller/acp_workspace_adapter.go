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
	// The class's frozen maximum lifetime is a hard bound on the physical
	// workspace, enforced BEFORE the admission gate: a workspace that lost
	// current core admission (for example a Disabled provider) must not keep
	// its linked RuntimePool executing past the frozen deadline just because
	// the normal lifecycle is unserved.
	remainingLifetime, lifetimeBounded := acpWorkspaceMaxLifetimeRemaining(workspace, time.Now())
	if exact && workspaceCarriesACPMaterializationMarkers(workspace) &&
		lifetimeBounded && remainingLifetime <= 0 {
		return r.reconcileExpiredACPWorkspace(ctx, workspace)
	}
	// Normal lifecycle requires the exact provider binding, a current core
	// admission, and the controller's own materialization markers; anything
	// else stays unserved and fails closed. An independently created
	// workspace that merely binds this provider UID has no linked RuntimePool
	// and must never be advertised as a usable physical environment.
	if !exact || !workspaceCurrentlyAdmittedByCore(workspace) ||
		!workspaceCarriesACPMaterializationMarkers(workspace) {
		if exact && workspaceCarriesACPMaterializationMarkers(workspace) && lifetimeBounded {
			// An unadmitted bounded workspace still schedules its own expiry
			// wake-up: admission-denied reconciles must not strand the
			// enforcement deadline.
			return ctrl.Result{RequeueAfter: remainingLifetime}, nil
		}
		return ctrl.Result{}, nil
	}

	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		result := ctrl.Result{}
		if lifetimeBounded {
			// Enforcement must fire even without another triggering event.
			result.RequeueAfter = remainingLifetime
		}
		return result, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			setACPWorkspaceProviderBindingStatus(status)
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
		// Data-only suspension is not executable yet: report the transition
		// without destroying anything so the request stays visibly pending.
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspending
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse,
				Reason:             string(workspacev1alpha1.ReasonProgressing),
				Message:            "suspension is not yet executable for ACP runtime workspaces",
				ObservedGeneration: workspace.Generation,
			})
		})
	default:
		return ctrl.Result{}, nil
	}
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
		labeled := workspace.Labels[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceProviderControllerName
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

// workspaceCarriesACPMaterializationMarkers reports whether the workspace was
// materialized by this controller: it carries the controller label and the
// RuntimePool link annotation stamped at creation. A foreign workspace bound
// to the ACP provider UID without them has no physical backing here.
func workspaceCarriesACPMaterializationMarkers(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspace.Labels[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceProviderControllerName &&
		strings.TrimSpace(workspace.Annotations[acpExecutionWorkspacePoolAnnotation]) != ""
}

// acpWorkspaceMaxLifetimeRemaining returns the time left before the class's
// frozen MaxLifetime bound expires, and whether such a bound exists.
func acpWorkspaceMaxLifetimeRemaining(
	workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time,
) (time.Duration, bool) {
	maxLifetime := workspace.Spec.Lifecycle.MaxLifetime
	if maxLifetime == nil || maxLifetime.Duration <= 0 {
		return 0, false
	}
	return workspace.CreationTimestamp.Add(maxLifetime.Duration).Sub(now), true
}

// reconcileExpiredACPWorkspace enforces the frozen maximum workspace lifetime:
// the linked RuntimePool is deleted so no RuntimeSession can keep executing,
// and the workspace reports a terminal Failed state with the enforced epoch
// revoked. Deletion-policy settlement still runs through the maintenance path
// when the owning Task detaches.
func (r *ACPExecutionWorkspaceAdapterReconciler) reconcileExpiredACPWorkspace(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (ctrl.Result, error) {
	poolGone, foreign, err := r.ensureLinkedRuntimePoolDeleted(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	result := ctrl.Result{}
	if !poolGone && !foreign {
		result.RequeueAfter = acpWorkspaceAdapterRequeue
	}
	return result, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
		status.ObservedGeneration = workspace.Generation
		setACPWorkspaceProviderBindingStatus(status)
		status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
		status.AttachedEpoch = 0
		workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
			Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse,
			Reason:             string(workspacev1alpha1.ReasonLifetimeExceeded),
			Message:            "the class maximum workspace lifetime elapsed; the enforced epoch is revoked",
			ObservedGeneration: workspace.Generation,
		})
		workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
			Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse,
			Reason:             string(workspacev1alpha1.ReasonLifetimeExceeded),
			Message:            "the class maximum workspace lifetime elapsed; the linked RuntimePool is being torn down",
			ObservedGeneration: workspace.Generation,
		})
	})
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
	// Cleanup is proven by absence of reverse-linked pools, never by the
	// mutable annotation alone: a lost or stale annotation value must not
	// let core finalize the workspace while provider resources remain live.
	gone := true
	foreign := false
	// Absence is proven through the uncached reader: a pool created moments
	// before workspace deletion can be invisible to the informer cache, and a
	// cached miss must never let core finalize the workspace while the pool
	// and its physical workspace remain live.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if poolName := strings.TrimSpace(workspace.Annotations[acpExecutionWorkspacePoolAnnotation]); poolName != "" {
		pool := &corev1alpha1.RuntimePool{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: poolName}, pool)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return false, false, err
		case pool.Labels[acpExecutionWorkspaceLinkLabel] != workspace.Name || pool.Spec.ExecutionWorkspace == nil ||
			pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID):
			// The mutable name link is not ownership: only the controller-
			// stamped workspace-incarnation pin proves this pool serves
			// exactly this workspace. A same-name pool without it is foreign
			// and never deleted.
			foreign = true
		default:
			gone = false
			if pool.DeletionTimestamp.IsZero() {
				// UID+resourceVersion preconditions: a concurrent pool update
				// after this read turns the delete into a retried conflict
				// instead of removing a pool whose linkage just changed.
				if err := r.Delete(ctx, pool, deleteCurrentObjectPreconditions(pool)...); err != nil &&
					!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
					return false, false, err
				}
			}
		}
	}
	pools := &corev1alpha1.RuntimePoolList{}
	if err := reader.List(ctx, pools, client.InNamespace(workspace.Namespace),
		client.MatchingLabels{acpExecutionWorkspaceLinkLabel: workspace.Name}); err != nil {
		return false, false, err
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		if pool.Spec.ExecutionWorkspace == nil {
			continue
		}
		if pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
			// Reverse-linked but not pinned to this incarnation: refuse to
			// delete it and hold the finalizer fail-closed.
			foreign = true
			continue
		}
		gone = false
		if pool.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pool, deleteCurrentObjectPreconditions(pool)...); err != nil &&
				!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return false, false, err
			}
		}
	}
	return gone, foreign, nil
}

// setACPWorkspaceProviderBindingStatus publishes the stable adapter/contract
// identity the generic provider-binding conformance requires once a workspace
// reaches Ready: the selected contract, the adapter version, and the resolved
// physical backend recorded at workspace creation.
func setACPWorkspaceProviderBindingStatus(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
	if status.ProviderBinding != nil {
		return
	}
	// BackendAPIVersion stays empty: the provider advertisement publishes no
	// backend API versions, the backend NAME is not an API version, and the
	// shared provider-binding conformance rejects any unadvertised value.
	status.ProviderBinding = &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
		ContractVersion: workspacev1alpha1.ContractVersionV1,
		AdapterVersion:  acpWorkspaceAdapterVersion,
	}
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
