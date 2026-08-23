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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
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
	// RuntimeNamespace is the controller-configured ACP runtime namespace
	// where workspace-backed pools place their provider children (the
	// SandboxClaim and its realized durable PVC); empty means the workspace
	// namespace.
	RuntimeNamespace string
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
		// Only an actual resume admission is polled: the workspace was
		// admitted before (its core-admission evidence exists for an older
		// generation) and a resume is outstanding (the demand annotation or
		// the suspended/suspending transition). A brand-new allocation
		// waiting on initial admission — class readiness, pool capacity —
		// relies on the core controller's own retry instead of a sustained
		// two-second adapter poll and log stream.
		resumeOutstanding := strings.TrimSpace(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]) != "" ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspending
		if exact && workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
			resumeOutstanding && workspaceHasCoreAdmissionEvidence(workspace) {
			logf.FromContext(ctx).Info("ACP workspace adapter holding a resume request",
				"workspace", workspace.Name, "generation", workspace.Generation,
				"coreAdmitted", workspaceCurrentlyAdmittedByCore(workspace))
			// Core admission is normally observed through a workspace event,
			// but a resume must never strand on a missed one.
			return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, nil
		}
		// A non-exact provider binding is permanent: spec.providerBinding is
		// immutable and provider UIDs are never reused, so no amount of
		// polling repairs it. The workspace stays dormant - no requeue, no
		// per-pass log - until deletion or another real watch event.
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
			// The pool proved the durable data unrecoverable during cold
			// resume; the workspace fails closed instead of resuming against
			// a silently re-materialized volume.
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
			} else {
				// Transitional observability for the conformance lanes: a
				// Ready workspace still bound to a suspended or scaled-down
				// pool is about to drive resume below.
				suspended := strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceSuspendAnnotation]) != ""
				if suspended || pool.Spec.DesiredReplicas == 0 {
					logf.FromContext(ctx).Info("ACP workspace adapter serving a Ready workspace",
						"workspace", workspace.Name, "generation", workspace.Generation,
						"poolSuspendIntent", suspended, "poolReplicas", pool.Spec.DesiredReplicas)
				}
			}
		}
		if requeue, err := r.driveLinkedRuntimePoolResume(ctx, workspace); err != nil || requeue {
			return ctrl.Result{RequeueAfter: acpWorkspaceAdapterRequeue}, err
		}
		return ctrl.Result{}, r.patchWorkspaceStatus(ctx, workspace, func(status *workspacev1alpha1.ExecutionWorkspaceStatus) {
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
	if !runtimePoolWorkspaceSuspendIntentSet(pool) {
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
	intentSet := runtimePoolWorkspaceSuspendIntentSet(pool)
	legacySet := strings.TrimSpace(pool.Annotations[runtimePoolLegacySubstrateSuspendAnnotation]) != ""
	if intentSet == suspend && !legacySet && pool.Spec.DesiredReplicas == desiredReplicas {
		return false, nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	// Every intent write migrates the legacy substrate spelling to the shared
	// key so recognition of the old key can eventually retire.
	delete(pool.Annotations, runtimePoolLegacySubstrateSuspendAnnotation)
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
	if gone, pvcErr := r.durableWorkspacePVCGone(ctx, workspace); pvcErr != nil {
		return ctrl.Result{}, pvcErr
	} else if !gone {
		// The pool is gone but background garbage collection has not removed
		// the realized durable PVC yet (a protection finalizer or CSI delay
		// can hold it). The deletion guarantee must not be published while
		// repository data can still exist; hold the finalizer instead.
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
	// A suspend-capable class produced real durable artifacts; once teardown
	// succeeds the terminal audit record affirms their deletion instead of
	// reporting NotApplicable. Substrate preserves data in a provider
	// checkpoint; agent-sandbox preserves it in a durable workspace PVC.
	checkpoints := workspacev1alpha1.DispositionNotApplicable
	persistentVolumes := workspacev1alpha1.DispositionNotApplicable
	// The frozen durable-capability record, not the allowed detach actions,
	// decides the disposition: a profile can provision the durable PVC while
	// the class allows only Delete, and allowing Suspend without a suspension
	// profile provisions nothing.
	if workspace.Annotations[acpWorkspaceDurableAnnotation] == booleanTrueValue {
		switch workspace.Annotations[acpWorkspaceBackendAnnotation] {
		case string(acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox):
			persistentVolumes = workspacev1alpha1.DispositionDeleted
		default:
			checkpoints = workspacev1alpha1.DispositionDeleted
		}
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
			PersistentVolumes: persistentVolumes,
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

// durableWorkspacePVCGone proves through the uncached reader that the
// realized durable workspace PVC of a durable agent-sandbox workspace no
// longer exists. Non-durable and substrate workspaces have no realized PVC
// and report gone immediately.
func (r *ACPExecutionWorkspaceAdapterReconciler) durableWorkspacePVCGone(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	if workspace.Annotations[acpWorkspaceDurableAnnotation] != booleanTrueValue ||
		workspace.Annotations[acpWorkspaceBackendAnnotation] != string(acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox) {
		return true, nil
	}
	poolName := strings.TrimSpace(workspace.Annotations[acpExecutionWorkspacePoolAnnotation])
	if poolName == "" {
		return true, nil
	}
	// The namespace frozen at creation wins: the controller's current
	// --acp-runtime-namespace may have changed since, and probing the wrong
	// namespace would prove a false NotFound while the original PVC lives on.
	pvcNamespace := strings.TrimSpace(workspace.Annotations[acpWorkspaceRuntimeNamespaceAnnotation])
	if pvcNamespace == "" {
		pvcNamespace = strings.TrimSpace(r.RuntimeNamespace)
	}
	if pvcNamespace == "" {
		pvcNamespace = workspace.Namespace
	}
	claimName := runtimePoolSandboxClaimName(runtimePoolResourceName(workspace.Namespace, poolName))
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: pvcNamespace, Name: durableWorkspacePVCName(claimName)}, pvc)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("prove durable workspace PVC deletion: %w", err)
	}
	return false, nil
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
