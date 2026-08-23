/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
)

const (
	// acpWorkspaceLastDetachedAnnotation records when the last Task attachment
	// was revoked. The class idleTimeout counts from this instant (or from
	// creation for a workspace that never attached).
	acpWorkspaceLastDetachedAnnotation = "acp.workspace.orka.ai/last-detached-at"
	// acpWorkspaceMaxSuspendedAnnotation freezes the class retention cap on
	// the materialized workspace so settlement enforces it without reloading
	// the execution snapshot.
	acpWorkspaceMaxSuspendedAnnotation = "acp.workspace.orka.ai/max-suspended"
	// acpWorkspaceRetentionRequeue bounds how often retention re-evaluates a
	// workspace with no imminent deadline.
	acpWorkspaceRetentionRequeue = 5 * time.Minute
)

// ACPWorkspaceRetentionReconciler enforces the frozen class lifetime policy on
// class-backed ACP execution workspaces: idleTimeout bounds how long an
// unattached workspace may stay Ready or Suspended, and maxLifetime is the
// hard upper bound that forces terminal cleanup regardless of state. It acts
// in Orka core's role (it writes desired state and object deletion only);
// the workspace adapter keeps executing the transitions.
type ACPWorkspaceRetentionReconciler struct {
	client.Client
	// APIReader bypasses the informer cache for suspended-capacity counts so
	// a quota claim never trusts a stale list. Falls back to Client when nil.
	APIReader client.Reader
	Recorder  events.EventRecorder
	Now       func() time.Time
}

func (r *ACPWorkspaceRetentionReconciler) quotaReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

//nolint:gocyclo // The retention decision table stays auditable in one place.
func (r *ACPWorkspaceRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceControllerLabelValue ||
		!workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(workspace, executionWorkspaceFinalizer) {
		// The cleanup finalizer is the guarantee that an expiry delete runs
		// the linked RuntimePool teardown and records the terminal
		// disposition; deleting before the core controller installs it would
		// remove the object immediately and orphan the pool. Wait for it.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}

	// The lifetime deadline never bypasses idle evaluation: a class whose
	// maxLifetime is nearer than the poll interval must still apply an
	// already-elapsed idleTimeout now, so the earliest applicable deadline
	// only clamps the requeue below.
	lifetimeRequeue := acpWorkspaceRetentionRequeue
	if lifetime := workspace.Spec.Lifecycle.MaxLifetime; lifetime != nil && lifetime.Duration > 0 {
		deadline := workspace.CreationTimestamp.Add(lifetime.Duration)
		if !now.Before(deadline) {
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "MaxLifetimeExpired",
				"class maxLifetime elapsed; the workspace is forced into terminal cleanup", false)
		}
		if requeue := deadline.Sub(now) + time.Second; requeue < lifetimeRequeue {
			lifetimeRequeue = requeue
		}
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		// Quarantined workspaces are never reused and skip ordinary idle
		// handling, but the frozen maxLifetime above remains the hard upper
		// bound so terminal records cannot leak forever.
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}

	if workspace.Spec.Attachment != nil || workspace.Status.AttachedEpoch > 0 {
		// A live attachment, or a revoked one whose epoch the adapter still
		// enforces, defers idle evaluation; the detach instant stamped at
		// revocation start opens a fresh idle window. spec.attachmentEpoch is
		// deliberately NOT consulted: it is the monotonic high-water mark and
		// stays positive forever after the first attachment.
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
	demandOutstanding, err := r.pendingWorkspaceDemandOutstanding(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		(workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspending ||
			demandOutstanding) {
		// A continuation requested cold resume and has not attached yet; the
		// workspace is actively demanded, not idle, even after the boot
		// completes (a cold boot can outlast idleTimeout). The demand record
		// is cleared by the continuation's attachment, and the frozen
		// maxLifetime above remains the hard bound if the requester dies
		// before attaching.
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
	idle := workspace.Spec.Lifecycle.IdleTimeout
	if idle == nil || idle.Duration <= 0 {
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
	idleStart := workspace.CreationTimestamp.Time
	if raw := strings.TrimSpace(workspace.Annotations[acpWorkspaceLastDetachedAnnotation]); raw != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			idleStart = parsed
		}
	}
	deadline := idleStart.Add(idle.Duration)
	if now.Before(deadline) {
		requeue := min(deadline.Sub(now)+time.Second, lifetimeRequeue)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredSuspended:
		// A suspended workspace past its idle timeout has exhausted its
		// retention: only terminal deletion is admitted until richer retention
		// dispositions exist.
		return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "IdleRetentionExpired",
			"class idleTimeout elapsed for the suspended workspace; retention is exhausted", true)
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		// The frozen effective action stamped at materialization or attach
		// wins over the class default: a Task that explicitly selected
		// Suspend under a Delete-default class must idle into suspension,
		// not deletion, even if it never attached.
		idleAction := workspace.Annotations[acpWorkspaceDetachActionAnnotation]
		if idleAction == "" {
			idleAction = string(workspace.Spec.Lifecycle.DefaultOnDetach)
		}
		if idleAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			runtimePoolWorkspaceSuspendableAnnotationPresent(workspace) {
			err := suspendACPWorkspaceWithinQuota(ctx, r.Client, r.quotaReader(), workspace, now, "")
			switch {
			case errors.Is(err, errACPSuspendQuotaExhausted):
				// The freed slot was consumed while this workspace idled; the
				// only admitted fallback disposition is Delete.
				return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "SuspendQuotaExhausted",
					"class idleTimeout elapsed and the retention cap is exhausted; applying the Delete disposition", true)
			case apierrors.IsConflict(err) || apierrors.IsNotFound(err):
				return ctrl.Result{RequeueAfter: time.Second}, nil
			case err != nil:
				return ctrl.Result{}, err
			}
			r.recordRetention(workspace, "IdleSuspended", "class idleTimeout elapsed; applying the class default Suspend action")
			metrics.RecordACPWorkspaceRetentionAction("suspend", "idle_timeout")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "IdleExpired",
			"class idleTimeout elapsed for the unattached workspace; applying the class Delete disposition", true)
	default:
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
}

// runtimePoolWorkspaceSuspendableAnnotationPresent reports whether the
// materialized workspace was admitted under a suspension-capable class (the
// frozen detach action recorded at attach time, or a recorded retention cap,
// both imply the class profile permitted DataOnly suspension).
func runtimePoolWorkspaceSuspendableAnnotationPresent(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend)
}

func (r *ACPWorkspaceRetentionReconciler) expireWorkspace(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	reason, message string,
	fenced bool,
) error {
	// Idle-triggered deletions are fenced with UID+resourceVersion so a
	// concurrent attachment or resume settles as a retried conflict instead
	// of destroying a workspace that became actively demanded; the
	// maxLifetime hard bound stays intentionally unconditional.
	preconditions := []client.DeleteOption{client.Preconditions{UID: &workspace.UID}}
	if fenced {
		preconditions = deleteCurrentObjectPreconditions(workspace)
	}
	if err := r.Delete(ctx, workspace, preconditions...); err != nil && !apierrors.IsNotFound(err) {
		if fenced && apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	r.recordRetention(workspace, reason, message)
	metrics.RecordACPWorkspaceRetentionAction("delete", strings.ToLower(reason))
	return nil
}

func (r *ACPWorkspaceRetentionReconciler) recordRetention(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	reason, message string,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(workspace, nil, corev1.EventTypeNormal, reason, "Retention", "%s", message)
}

// acpSuspendQuotaLocks serializes suspended-capacity claims per class: two
// concurrent reconciles listing the same free slot and patching different
// workspaces to Suspended would both succeed under per-object optimistic
// locks. The controller is the single leader-elected writer of DesiredState,
// so an in-process mutex around an uncached count-and-patch closes that race.
var acpSuspendQuotaLocks sync.Map

// errACPSuspendQuotaExhausted reports a claim rejected by the frozen class
// retention cap; the caller applies its fallback disposition.
var errACPSuspendQuotaExhausted = errors.New("class suspended-workspace retention cap is exhausted")

func lockACPSuspendQuota(namespace string, classUID types.UID) func() {
	value, _ := acpSuspendQuotaLocks.LoadOrStore(namespace+"/"+string(classUID), &sync.Mutex{})
	mutex, _ := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// suspendACPWorkspaceWithinQuota atomically claims one suspension slot under
// the workspace's frozen retention cap and patches it to Suspended, stamping
// the detach instant so the suspended state earns a full retention interval.
// It returns errACPSuspendQuotaExhausted when the cap is already consumed and
// passes patch conflicts through for the caller's requeue policy.
func suspendACPWorkspaceWithinQuota(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
	settledTaskUID string,
) error {
	unlock := lockACPSuspendQuota(workspace.Namespace, workspace.Spec.ClassBinding.UID)
	defer unlock()
	if limit := acpWorkspaceSuspendedCapFromAnnotation(workspace); limit != nil {
		suspended, err := countSuspendedClassWorkspaces(
			ctx, reader, workspace.Namespace, workspace.Spec.ClassBinding.UID,
			func(candidate *workspacev1alpha1.ExecutionWorkspace) bool {
				return candidate.Name == workspace.Name
			},
		)
		if err != nil {
			return err
		}
		if suspended >= int(*limit) {
			return errACPSuspendQuotaExhausted
		}
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now.UTC().Format(time.RFC3339Nano)
	delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
	if settledTaskUID != "" {
		// The settlement receipt lands in the SAME patch as the suspension:
		// if the controller dies before the separate Task-side marker patch,
		// a restarted reconcile of that Task finds its receipt here and
		// completes the marker instead of re-applying Suspend to newer
		// session state.
		workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation] = settledTaskUID
	}
	// Suspension settles any pending provisioning or resume demand; a later
	// continuation stamps fresh demand when it flips the workspace back.
	delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	return writer.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// countSuspendedClassWorkspaces counts suspended workspaces bound to the exact
// class UID in one namespace, skipping candidates the exclude predicate
// accepts.
func countSuspendedClassWorkspaces(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	classUID types.UID,
	exclude func(*workspacev1alpha1.ExecutionWorkspace) bool,
) (int, error) {
	list := &workspacev1alpha1.ExecutionWorkspaceList{}
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("list execution workspaces for retention accounting: %w", err)
	}
	count := 0
	for i := range list.Items {
		workspace := &list.Items[i]
		if workspace.Spec.ClassBinding.UID != classUID || (exclude != nil && exclude(workspace)) {
			continue
		}
		if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
			// A terminally failed suspension preserved no resumable data and
			// must not consume a capacity slot forever.
			continue
		}
		suspendedCharge := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		// Deletion flips DesiredState to Deleted before the finalizers drain
		// the pool and retained PVC/checkpoint, so a deleting durable
		// workspace stays charged until its terminal disposition proves the
		// retained artifacts are gone; freeing the slot early would let slow
		// or stuck teardown accumulate an unbounded backlog past
		// maxSuspendedWorkspaces.
		deletingCharge := !workspace.DeletionTimestamp.IsZero() &&
			workspace.Annotations[acpWorkspaceDurableAnnotation] == booleanTrueValue &&
			workspace.Status.Disposition == nil
		if suspendedCharge || deletingCharge {
			count++
		}
	}
	return count, nil
}

// pendingWorkspaceDemandOutstanding reports whether the demand record on the
// workspace still has a live requester. The record binds the requesting
// Task's name; when that Task is gone or terminal, no attachment can ever
// fulfil the demand and ordinary idle handling resumes, so a create/link
// crash window cannot leak the workspace forever.
func (r *ACPWorkspaceRetentionReconciler) pendingWorkspaceDemandOutstanding(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	raw := strings.TrimSpace(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation])
	if raw == "" {
		return false, nil
	}
	fields := strings.Fields(raw)
	if len(fields) < 2 {
		// A legacy stamp without requester identity is honored; maxLifetime
		// remains its hard bound.
		return true, nil
	}
	task := &corev1alpha1.Task{}
	err := r.quotaReader().Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: fields[1]}, task)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if len(fields) >= 3 && string(task.UID) != fields[2] {
		// A replacement Task under the recycled namespace/name is not the
		// requester; its unrelated lifetime must not keep stale demand alive.
		return false, nil
	}
	if !task.DeletionTimestamp.IsZero() ||
		task.Status.Phase == corev1alpha1.TaskPhaseSucceeded ||
		task.Status.Phase == corev1alpha1.TaskPhaseFailed ||
		task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		// The requester can never attach; its settlement (or deletion
		// settlement) owns any remaining cleanup.
		return false, nil
	}
	return true, nil
}

// acpWorkspaceSuspendedCapFromAnnotation parses the frozen retention cap
// recorded on the materialized workspace, or nil when unbounded.
func acpWorkspaceSuspendedCapFromAnnotation(workspace *workspacev1alpha1.ExecutionWorkspace) *int32 {
	raw := strings.TrimSpace(workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation])
	if raw == "" {
		// Absent means the class froze no cap: retention is unbounded by
		// design.
		return nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < 0 {
		// A present-but-invalid frozen value fails closed as an exhausted cap
		// (zero) instead of silently disabling the class's hard quota.
		zero := int32(0)
		return &zero
	}
	limit := int32(parsed)
	return &limit
}

// SetupWithManager registers retention enforcement for ACP class workspaces.
func (r *ACPWorkspaceRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ours := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceControllerLabelValue
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		WithEventFilter(ours).
		Named("acp-workspace-retention").
		Complete(r)
}
