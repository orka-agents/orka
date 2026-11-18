package applicationexec

import (
	"context"
	"encoding/json"

	"github.com/orka-agents/orka/internal/remediation/contract"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) verifyOwnedTarget(ctx context.Context, r *run) error {
	if claim := r.effect("target-claim"); claim != nil && claim.UID != "" && !claim.Deleted {
		object, err := b.boundObject(ctx, r, "target-claim")
		if err != nil {
			return err
		}
		lease := object.(*coordinationv1.Lease)
		holder, _ := json.Marshal([]string{r.state.ControlUID, string(r.object.UID), string(r.state.Task.UID)})
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(holder) || lease.Spec.LeaseDurationSeconds != nil {
			return contract.ErrConflict
		}
	}
	for _, entry := range r.state.Effects {
		if entry.Kind != namespaceKind || entry.Rejected || entry.Deleted {
			continue
		}
		object, err := b.boundObject(ctx, r, entry.Key)
		if err != nil {
			return err
		}
		if object.GetLabels()["pod-security.kubernetes.io/enforce"] != podSecurityRestricted {
			return ErrIsolation
		}
	}
	namespaces := &corev1.NamespaceList{}
	if err := b.targetReader.List(ctx, namespaces, client.Limit(64)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if namespaces.Continue != "" {
		return ErrIsolation
	}
	for _, namespace := range namespaces.Items {
		switch namespace.Name {
		case "default", "kube-public", "kube-node-lease":
			pods := &corev1.PodList{}
			if err := b.targetReader.List(ctx, pods, client.InNamespace(namespace.Name), client.Limit(1)); err != nil {
				return safeError(contract.ErrUnavailable, err)
			}
			if len(pods.Items) != 0 {
				return ErrIsolation
			}
		case metav1.NamespaceSystem, "local-path-storage":
			// The dedicated kind control plane, CNI and dynamic provisioner
			// are operator-owned prerequisites, not application fixtures.
		default:
			if namespace.Labels[runLabel] != string(r.object.UID) {
				return ErrIsolation
			}
			known := false
			for _, entry := range r.state.Effects {
				known = known || entry.Kind == namespaceKind && entry.Name == namespace.Name &&
					entry.UID == namespace.UID && entry.UID != "" && !entry.Deleted
			}
			if !known {
				return contract.ErrConflict
			}
		}
	}
	return b.auditAuthorization(ctx, r)
}
