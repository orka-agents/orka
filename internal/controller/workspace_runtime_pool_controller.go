// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// WorkspaceRuntimePoolReconciler owns the core-side link and demand. Provider
// status and attachment acknowledgement remain exclusively provider-written.
type WorkspaceRuntimePoolReconciler struct {
	client.Client
	APIReader client.Reader
}

func (r *WorkspaceRuntimePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	w := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, w); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	name := w.Annotations[acpExecutionWorkspacePoolAnnotation]
	if name == "" {
		return ctrl.Result{}, nil
	}
	pool := &corev1alpha1.RuntimePool{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: name}, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !runtimePoolHasExternalWorkspace(pool) {
		return ctrl.Result{}, nil
	}
	ref := pool.Spec.ExecutionWorkspace.WorkspaceRef
	if ref.Name != w.Name || ref.UID != w.UID || pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(w.UID) {
		return ctrl.Result{}, fmt.Errorf("workspace RuntimePool link identifies another incarnation")
	}
	deleting := !w.DeletionTimestamp.IsZero() || w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	// The retention controller owns lifetime expiry and requests API deletion
	// with the exact workspace UID. Setting desiredState here first would stop
	// retention before it establishes the metadata deletion/finalizer path.
	if deleting {
		if pool.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pool, deleteCurrentObjectPreconditions(pool)...); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if w.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || w.Spec.Attachment == nil {
		if pool.Spec.DesiredReplicas != 0 {
			before := pool.DeepCopy()
			pool.Spec.DesiredReplicas = 0
			if err := r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{}, nil
}

func (r *WorkspaceRuntimePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&workspacev1alpha1.ExecutionWorkspace{}).
		Watches(&corev1alpha1.RuntimePool{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
			name := obj.GetLabels()[acpExecutionWorkspaceLinkLabel]
			if name == "" {
				return nil
			}
			return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
		})).Named("workspace-runtime-pool").Complete(r)
}

// ValidateExternalWorkspaceUpgrade runs before dispatch controllers start.
// Pool schema pruning is never authority to recreate a legacy allocation.
func ValidateExternalWorkspaceUpgrade(ctx context.Context, reader client.Reader) error {
	var blockers []string
	pools := &corev1alpha1.RuntimePoolList{}
	if err := reader.List(ctx, pools); err != nil && !apimeta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
		return fmt.Errorf("workspace upgrade preflight: list RuntimePools: %w", err)
	}
	for _, pool := range pools.Items {
		if pool.Spec.ExecutionWorkspace != nil && !runtimePoolHasExternalWorkspace(&pool) {
			blockers = append(blockers, "RuntimePool "+pool.Namespace+"/"+pool.Name)
		}
	}
	workspaces := &workspacev1alpha1.ExecutionWorkspaceList{}
	if err := reader.List(ctx, workspaces); err != nil && !apimeta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
		return fmt.Errorf("workspace upgrade preflight: list ExecutionWorkspaces: %w", err)
	}
	for _, w := range workspaces.Items {
		if w.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceControllerLabelValue {
			continue
		}
		blockers = append(blockers, "legacy retained ExecutionWorkspace "+w.Namespace+"/"+w.Name)
	}
	if len(blockers) == 0 {
		return nil
	}
	sort.Strings(blockers)
	return fmt.Errorf("external workspace dispatch requires legacy resources to retire under their original provider before upgrade: %s", strings.Join(blockers, ", "))
}
