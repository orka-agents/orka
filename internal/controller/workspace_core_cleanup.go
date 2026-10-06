// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type workspaceCoreCleanup struct {
	client.Client
	APIReader client.Reader
}

func workspaceCarriesACPMaterializationMarkers(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != "" &&
		strings.TrimSpace(workspace.Annotations[acpExecutionWorkspacePoolAnnotation]) != ""
}

func workspaceHasACPControllerOwnership(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspace.Labels[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceControllerLabelValue || workspaceCarriesACPMaterializationMarkers(workspace)
}

func acpWorkspaceMaxLifetimeRemaining(
	workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time,
) (time.Duration, bool) {
	maxLifetime := workspace.Spec.Lifecycle.MaxLifetime
	if maxLifetime == nil || maxLifetime.Duration <= 0 {
		return 0, false
	}
	return workspace.CreationTimestamp.Add(maxLifetime.Duration).Sub(now), true
}

func (r *workspaceCoreCleanup) ensureACPWorkspaceAttachmentCredentialsDeleted(
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

func (r *workspaceCoreCleanup) ensureLinkedRuntimePoolDeleted(
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
	reader := uncachedReader(r.APIReader, r.Client)
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

func uncachedReader(api client.Reader, c client.Client) client.Reader {
	if api != nil {
		return api
	}
	return c
}

const (
	runtimePoolBootstrapNonceEnv = "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE"
	objectLabelsField            = "labels"
)
