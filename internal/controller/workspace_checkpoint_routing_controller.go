// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"fmt"
	"slices"

	workspace "github.com/orka-agents/orka-workspace/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const workspaceCheckpointProviderNameLabel = "workspace.orka.ai/provider-name"
const workspaceCheckpointRoutingFailureAnnotation = "workspace.orka.ai/checkpoint-routing-failure"

// Core establishes checkpoint routing from the exact source workspace. The
// selected provider owns artifact export, observed status and reference cleanup.
type WorkspaceCheckpointRoutingReconciler struct {
	client.Client
	APIReader client.Reader
}

func (r *WorkspaceCheckpointRoutingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	checkpoint := &workspace.ExecutionWorkspaceCheckpoint{}
	if err := r.Get(ctx, req.NamespacedName, checkpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !checkpoint.DeletionTimestamp.IsZero() || checkpoint.Labels[workspaceCheckpointProviderNameLabel] != "" {
		return ctrl.Result{}, nil
	}
	// Published exports and in-progress legacy references keep their prior
	// owner even when the source is gone and no new routing is established.
	if checkpoint.Status.Digest != "" || slices.Contains(checkpoint.Finalizers, "orka.ai/substrate-checkpoint-reference") {
		return ctrl.Result{}, nil
	}
	if checkpoint.Annotations[workspaceCheckpointRoutingFailureAnnotation] != "" {
		return ctrl.Result{}, nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	source := &workspace.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: checkpoint.Namespace, Name: checkpoint.Spec.WorkspaceRef.Name}, source); err != nil {
		if r.APIReader != nil && apierrors.IsNotFound(err) {
			return r.failRouting(ctx, checkpoint, "SourceNotFound")
		}
		return ctrl.Result{}, err
	}
	if source.UID != checkpoint.Spec.WorkspaceRef.UID {
		if r.APIReader != nil {
			return r.failRouting(ctx, checkpoint, "SourceUIDMismatch")
		}
		return ctrl.Result{}, fmt.Errorf("checkpoint source workspace identity is unavailable from the authoritative API")
	}
	if !source.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, fmt.Errorf("checkpoint source workspace has changed or is deleting")
	}
	if source.Labels[workspace.ProviderControllerLabel] == acpWorkspaceControllerLabelValue {
		return ctrl.Result{}, nil // Retained legacy checkpoints keep their original owner.
	}
	provider := &workspace.ExecutionWorkspaceProvider{}
	if err := reader.Get(ctx, types.NamespacedName{Name: source.Spec.ProviderBinding.Name}, provider); err != nil {
		return ctrl.Result{}, err
	}
	if provider.UID != source.Spec.ProviderBinding.UID || provider.Spec.ControllerName == "" || provider.Spec.ControllerName != source.Labels[workspace.ProviderControllerLabel] {
		return ctrl.Result{}, fmt.Errorf("checkpoint provider identity or Data export capability is unavailable")
	}
	if !workspaceProviderNameSupportsRouting(provider.Name) {
		if r.APIReader == nil {
			return ctrl.Result{}, fmt.Errorf("checkpoint provider registration name requires authoritative validation")
		}
		return r.failRouting(ctx, checkpoint, reasonProviderNameUnsupported)
	}
	if provider.Spec.LifecycleState == workspace.ExecutionWorkspaceProviderDisabled || !slices.Contains(provider.Status.SupportedFeatures, workspace.WorkspaceFeatureCheckpoint) {
		return ctrl.Result{}, fmt.Errorf("checkpoint provider identity or Data export capability is unavailable")
	}
	before := checkpoint.DeepCopy()
	if checkpoint.Labels == nil {
		checkpoint.Labels = map[string]string{}
	}
	checkpoint.Labels[workspace.ProviderControllerLabel] = provider.Spec.ControllerName
	checkpoint.Labels[workspaceCheckpointProviderNameLabel] = provider.Name
	if err := r.Patch(ctx, checkpoint, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// A routing failure is Core-owned metadata, not a provider export or cleanup
// result. The immutable source UID cannot become available again after loss.
func (r *WorkspaceCheckpointRoutingReconciler) failRouting(ctx context.Context, checkpoint *workspace.ExecutionWorkspaceCheckpoint, reason string) (ctrl.Result, error) {
	before := checkpoint.DeepCopy()
	if checkpoint.Annotations == nil {
		checkpoint.Annotations = map[string]string{}
	}
	checkpoint.Annotations[workspaceCheckpointRoutingFailureAnnotation] = reason
	return ctrl.Result{}, r.Patch(ctx, checkpoint, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *WorkspaceCheckpointRoutingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&workspace.ExecutionWorkspaceCheckpoint{}).
		Watches(&workspace.ExecutionWorkspace{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			checkpoints := &workspace.ExecutionWorkspaceCheckpointList{}
			if err := r.List(ctx, checkpoints, client.InNamespace(obj.GetNamespace())); err != nil {
				return nil
			}
			var requests []ctrl.Request
			for _, checkpoint := range checkpoints.Items {
				if checkpoint.Spec.WorkspaceRef.Name == obj.GetName() && checkpoint.Spec.WorkspaceRef.UID == obj.GetUID() {
					requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&checkpoint)})
				}
			}
			return requests
		})).Named("workspace-checkpoint-routing").Complete(r)
}
