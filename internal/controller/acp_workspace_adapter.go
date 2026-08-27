/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
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
	// The class's frozen maximum lifetime is evaluated BEFORE the revocation
	// bypass: settlement's BeginRevocation clears spec.attachment, and taking
	// the bypass for an expired workspace would publish AttachedEpoch=0
	// while the linked RuntimePool still drains - exactly the premature
	// release reconcileExpiredACPWorkspace exists to prevent. Expired
	// materialized workspaces stay on the expiry path until pool absence is
	// proven.
	remainingLifetime, lifetimeBounded := acpWorkspaceMaxLifetimeRemaining(workspace, time.Now())
	expired := workspaceCarriesACPMaterializationMarkers(workspace) && lifetimeBounded && remainingLifetime <= 0
	if expired {
		// Expiry does NOT require the exact live provider binding: a deleted
		// or recreated provider withdraws admission but must not let the
		// linked RuntimePool run past the frozen hard deadline. The expiry
		// path itself acts only on the admission-protected markers and the
		// UID-pinned pool link, so an owned materialized workspace is safe to
		// enforce even while the provider object is unavailable.
		return r.reconcileExpiredACPWorkspace(ctx, workspace)
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
	// Normal lifecycle requires the exact provider binding, a current core
	// admission, and the controller's own materialization markers; anything
	// else stays unserved and fails closed. An independently created
	// workspace that merely binds this provider UID has no linked RuntimePool
	// and must never be advertised as a usable physical environment.
	if !exact || !workspaceCurrentlyAdmittedByCore(workspace) ||
		!workspaceCarriesACPMaterializationMarkers(workspace) {
		if workspaceCarriesACPMaterializationMarkers(workspace) && lifetimeBounded {
			// An unadmitted (or provider-unbound) bounded workspace still
			// schedules its own expiry wake-up: admission-denied reconciles
			// must not strand the enforcement deadline.
			return ctrl.Result{RequeueAfter: remainingLifetime}, nil
		}
		return ctrl.Result{}, nil
	}

	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
			// A failed cold resume is terminal while DesiredState stays
			// Ready: overwriting it with Ready would let a continuation
			// attach and recreate an empty pool despite the checkpoint being
			// declared unrecoverable.
			return ctrl.Result{}, nil
		}
		if pool, foreign, poolErr := r.linkedRuntimePool(ctx, workspace); poolErr != nil {
			return ctrl.Result{}, poolErr
		} else if pool != nil && !foreign &&
			strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceResumeLostAnnotation]) != "" {
			// The pool proved the durable data unrecoverable; the workspace
			// fails closed instead of resuming against a silently
			// re-materialized volume.
			return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
				status.ObservedGeneration = workspace.Generation
				status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
				status.AttachedEpoch = 0
				workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
					Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse,
					Reason:             string(workspacev1alpha1.ReasonCleanupFailed),
					Message:            "the suspended workspace's durable data is unrecoverable; cold resume fails closed",
					ObservedGeneration: workspace.Generation,
				})
			})
		}
		if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspending ||
			workspace.Annotations[acpWorkspaceResumedLineageAnnotation] == booleanTrueValue {
			if pool, foreign, poolErr := r.linkedRuntimePool(ctx, workspace); poolErr != nil {
				return ctrl.Result{}, poolErr
			} else if pool == nil || foreign {
				// The checkpoint lives in the linked pool's actor; a missing
				// or foreign pool — during the resume transition OR at any
				// later point of a resumed lineage — means the preserved data
				// is gone, and publishing Ready would let a fresh pool
				// silently re-materialize an empty baseline.
				return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
					status.ObservedGeneration = workspace.Generation
					status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
					status.AttachedEpoch = 0
					workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
						Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse,
						Reason:             string(workspacev1alpha1.ReasonCleanupFailed),
						Message:            "the suspended workspace's linked pool is gone; the data checkpoint is unrecoverable and cold resume fails closed",
						ObservedGeneration: workspace.Generation,
					})
				})
			}
		}
		if requeue, err := r.driveLinkedRuntimePoolResume(ctx, workspace); err != nil || requeue {
			return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, err
		}
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
	if foreign || pool == nil ||
		pool.Spec.ExecutionWorkspace == nil || pool.Spec.ExecutionWorkspace.Substrate == nil ||
		pool.Spec.ExecutionWorkspace.Substrate.SuspendMode == "" {
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
	consent := strings.TrimSpace(pool.Annotations[substrateActorSuspendedAnnotation]) != ""
	switch {
	case settled && consent:
		// The stopped pool produces no further status events, so the settled
		// suspension must self-schedule its own maxLifetime deadline: without
		// the requeue the retained checkpoint, actor, and pool could outlive
		// the frozen bound until some unrelated mutation wakes the adapter.
		settledResult := ctrl.Result{}
		if remaining, bounded := acpWorkspaceMaxLifetimeRemaining(workspace, time.Now()); bounded {
			settledResult = ctrl.Result{RequeueAfter: max(remaining, time.Second)}
		}
		return settledResult, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
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
	if strings.TrimSpace(pool.Annotations[substrateWorkspaceSuspendAnnotation]) == "" {
		return false, nil
	}
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
	if pool.Labels[acpExecutionWorkspaceLinkLabel] != workspace.Name || pool.Spec.ExecutionWorkspace == nil ||
		pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
		// The reusable name link is not ownership: only the controller-
		// stamped workspace-incarnation pin proves this pool serves exactly
		// this workspace, and suspension intent, replica counts, and resume
		// must never be driven onto a pool of a different incarnation.
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
	intentSet := strings.TrimSpace(pool.Annotations[substrateWorkspaceSuspendAnnotation]) != ""
	if intentSet == suspend && pool.Spec.DesiredReplicas == desiredReplicas {
		return false, nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	if suspend {
		pool.Annotations[substrateWorkspaceSuspendAnnotation] = booleanTrueValue
	} else {
		delete(pool.Annotations, substrateWorkspaceSuspendAnnotation)
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
	// Ownership rests on the admission-protected controller label alone: a
	// live same-name provider serving the ACP controller must never claim a
	// FOREIGN workspace (missing or different label, different provider
	// UID) - the maintenance path would report a terminal Deleted
	// disposition for resources this adapter never managed. The live
	// provider decides only whether the binding is exact.
	labeled := workspace.Labels[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceProviderControllerName
	if !labeled {
		return false, false, nil
	}
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	err := r.Get(ctx, types.NamespacedName{Name: workspace.Spec.ProviderBinding.Name}, provider)
	if apierrors.IsNotFound(err) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	exact := provider.Spec.ControllerName == acpWorkspaceProviderControllerName &&
		provider.UID == workspace.Spec.ProviderBinding.UID
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
	if !poolGone || foreign {
		// The enforced attachment epoch is released only AFTER the linked
		// pool is proven absent: the pool finalizer's authenticated drain is
		// still running (or a foreign same-name pool blocks deletion), and
		// clearing the epoch now would let FinalizeRevocation delete the
		// attachment authority before runtime quiescence is proven. The
		// workspace already reports Failed so no new work is admitted.
		return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, r.patchWorkspaceStatus(ctx, workspace,
			func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
				status.ObservedGeneration = workspace.Generation
				setACPWorkspaceProviderBindingStatus(status)
				status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
				workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
					Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionFalse,
					Reason:             string(workspacev1alpha1.ReasonLifetimeExceeded),
					Message:            "the class maximum workspace lifetime elapsed; the linked RuntimePool is being torn down",
					ObservedGeneration: workspace.Generation,
				})
			})
	}
	return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
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
			Message:            "the class maximum workspace lifetime elapsed; the linked RuntimePool is deleted",
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
	credentialsGone, err := r.ensureACPWorkspaceAttachmentCredentialsDeleted(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !credentialsGone {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined && workspace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
			status.ObservedGeneration = workspace.Generation
			status.State = workspacev1alpha1.ExecutionWorkspaceStateQuarantined
			status.AttachedEpoch = 0
			workspaceprovider.SetCondition(&status.Conditions, metav1.Condition{
				Type: string(workspacev1alpha1.ConditionWorkspaceQuarantined), Status: metav1.ConditionTrue,
				Reason:             string(workspacev1alpha1.ReasonQuarantined),
				Message:            "workspace is quarantined; its RuntimePool and attachment credentials were destroyed and it will never be reused",
				ObservedGeneration: workspace.Generation,
			})
		})
	}
	// A suspend-capable class can have produced a real provider data
	// checkpoint; once teardown succeeds the terminal audit record affirms
	// its deletion instead of reporting NotApplicable.
	checkpoints := workspacev1alpha1.DispositionNotApplicable
	if slices.Contains(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend) {
		checkpoints = workspacev1alpha1.DispositionDeleted
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
			Checkpoints:       checkpoints,
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

// ensureACPWorkspaceAttachmentCredentialsDeleted removes and then proves the
// absence of the attachment Secret and Lease before either terminal
// maintenance state revokes the enforced epoch. Quarantine preserves the
// workspace object, so owner-reference garbage collection is not a cleanup
// mechanism for these bearer credentials.
func (r *ACPExecutionWorkspaceAdapterReconciler) ensureACPWorkspaceAttachmentCredentialsDeleted(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	credentialReader := client.Reader(r.Client)
	if r.APIReader != nil {
		credentialReader = r.APIReader
	}
	attachmentSecrets := &corev1.SecretList{}
	if err := credentialReader.List(ctx, attachmentSecrets,
		client.InNamespace(workspace.Namespace),
		client.MatchingLabels{workspaceAttachmentLabel: string(workspace.UID)},
	); err != nil {
		return false, fmt.Errorf("list workspace attachment Secrets: %w", err)
	}
	for i := range attachmentSecrets.Items {
		secret := &attachmentSecrets.Items[i]
		owner := metav1.GetControllerOf(secret)
		if owner == nil || owner.UID != workspace.UID {
			continue
		}
		if err := r.Delete(ctx, secret, deleteCurrentObjectPreconditions(secret)...); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete workspace attachment Secret %s: %w", secret.Name, err)
		}
	}
	if epoch := workspace.Spec.AttachmentEpoch; epoch > 0 {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: attachmentSecretName(workspace.Name, epoch), Namespace: workspace.Namespace,
		}}
		if err := deleteWorkspaceOwnedAttachmentObject(ctx, credentialReader, r.Client, workspace, secret, "Secret"); err != nil {
			return false, err
		}
	}
	attachmentLease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace,
	}}
	if err := deleteWorkspaceOwnedAttachmentObject(ctx, credentialReader, r.Client, workspace, attachmentLease, "Lease"); err != nil {
		return false, err
	}
	attachmentSecrets = &corev1.SecretList{}
	if err := credentialReader.List(ctx, attachmentSecrets,
		client.InNamespace(workspace.Namespace),
		client.MatchingLabels{workspaceAttachmentLabel: string(workspace.UID)},
	); err != nil {
		return false, fmt.Errorf("prove workspace attachment Secret absence: %w", err)
	}
	for i := range attachmentSecrets.Items {
		owner := metav1.GetControllerOf(&attachmentSecrets.Items[i])
		if owner != nil && owner.UID == workspace.UID {
			return false, nil
		}
	}
	if epoch := workspace.Spec.AttachmentEpoch; epoch > 0 {
		err := credentialReader.Get(ctx, types.NamespacedName{
			Namespace: workspace.Namespace, Name: attachmentSecretName(workspace.Name, epoch),
		}, &corev1.Secret{})
		if err == nil {
			return false, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("prove attachment Secret absence: %w", err)
		}
	}
	if err := credentialReader.Get(ctx, types.NamespacedName{
		Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name),
	}, &coordinationv1.Lease{}); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("prove attachment Lease absence: %w", err)
	}
	return true, nil
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
	if reflect.DeepEqual(before.Status, workspace.Status) {
		return nil
	}
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
