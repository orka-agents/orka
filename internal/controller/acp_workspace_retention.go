/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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
	Recorder events.EventRecorder
	Now      func() time.Time
}

//nolint:gocyclo // The retention decision table stays auditable in one place.
func (r *ACPWorkspaceRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceProviderControllerName ||
		!workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		return ctrl.Result{}, nil
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}

	if lifetime := workspace.Spec.Lifecycle.MaxLifetime; lifetime != nil && lifetime.Duration > 0 {
		deadline := workspace.CreationTimestamp.Add(lifetime.Duration)
		if !now.Before(deadline) {
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "MaxLifetimeExpired",
				"class maxLifetime elapsed; the workspace is forced into terminal cleanup")
		}
		if requeue := deadline.Sub(now); requeue < acpWorkspaceRetentionRequeue {
			return ctrl.Result{RequeueAfter: requeue + time.Second}, nil
		}
	}

	if workspace.Spec.Attachment != nil {
		return ctrl.Result{RequeueAfter: acpWorkspaceRetentionRequeue}, nil
	}
	idle := workspace.Spec.Lifecycle.IdleTimeout
	if idle == nil || idle.Duration <= 0 {
		return ctrl.Result{RequeueAfter: acpWorkspaceRetentionRequeue}, nil
	}
	idleStart := workspace.CreationTimestamp.Time
	if raw := strings.TrimSpace(workspace.Annotations[acpWorkspaceLastDetachedAnnotation]); raw != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			idleStart = parsed
		}
	}
	deadline := idleStart.Add(idle.Duration)
	if now.Before(deadline) {
		requeue := min(deadline.Sub(now)+time.Second, acpWorkspaceRetentionRequeue)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredSuspended:
		// A suspended workspace past its idle timeout has exhausted its
		// retention: only terminal deletion is admitted until richer retention
		// dispositions exist.
		return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "IdleRetentionExpired",
			"class idleTimeout elapsed for the suspended workspace; retention is exhausted")
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		if workspace.Spec.Lifecycle.DefaultOnDetach == workspacev1alpha1.WorkspaceOnDetachSuspend &&
			runtimePoolWorkspaceSuspendableAnnotationPresent(workspace) {
			base := workspace.DeepCopy()
			workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
			if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return ctrl.Result{}, err
			}
			r.recordRetention(workspace, "IdleSuspended", "class idleTimeout elapsed; applying the class default Suspend action")
			metrics.RecordACPWorkspaceRetentionAction("suspend", "idle_timeout")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "IdleExpired",
			"class idleTimeout elapsed for the unattached workspace; applying the class Delete disposition")
	default:
		return ctrl.Result{RequeueAfter: acpWorkspaceRetentionRequeue}, nil
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
) error {
	if err := r.Delete(ctx, workspace, client.Preconditions{UID: &workspace.UID}); err != nil && !apierrors.IsNotFound(err) {
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

// countSuspendedClassWorkspaces counts suspended workspaces bound to the exact
// class UID in one namespace, excluding the named workspace.
func countSuspendedClassWorkspaces(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	classUID types.UID,
	exclude string,
) (int, error) {
	list := &workspacev1alpha1.ExecutionWorkspaceList{}
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("list execution workspaces for retention accounting: %w", err)
	}
	count := 0
	for i := range list.Items {
		workspace := &list.Items[i]
		if workspace.Name == exclude || workspace.Spec.ClassBinding.UID != classUID {
			continue
		}
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended &&
			workspace.DeletionTimestamp.IsZero() {
			count++
		}
	}
	return count, nil
}

// acpWorkspaceSuspendedCapFromAnnotation parses the frozen retention cap
// recorded on the materialized workspace, or nil when unbounded.
func acpWorkspaceSuspendedCapFromAnnotation(workspace *workspacev1alpha1.ExecutionWorkspace) *int32 {
	raw := strings.TrimSpace(workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation])
	if raw == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < 0 {
		return nil
	}
	limit := int32(parsed)
	return &limit
}

// SetupWithManager registers retention enforcement for ACP class workspaces.
func (r *ACPWorkspaceRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ours := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceProviderControllerName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		WithEventFilter(ours).
		Named("acp-workspace-retention").
		Complete(r)
}
