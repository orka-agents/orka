package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HasJobPods uses owner UIDs rather than mutable labels, and bounds each
// authoritative API response even in a namespace containing unrelated work.
func HasJobPods(ctx context.Context, reader client.Reader, namespace string, jobUIDs map[string]bool, jobNames ...string) (bool, error) {
	return HasOwnedPods(ctx, reader, namespace, jobUIDs, jobNames...)
}

// HasOwnedPods treats any exact owner UID as outstanding cleanup, including a
// malformed owner kind. Presence checks never authorize deleting that Pod.
func HasOwnedPods(ctx context.Context, reader client.Reader, namespace string, ownerUIDs map[string]bool, ownerNames ...string) (bool, error) {
	if len(ownerUIDs) == 0 && len(ownerNames) == 0 {
		return false, nil
	}
	names := make(map[string]bool, len(ownerNames))
	for _, name := range ownerNames {
		names[name] = true
	}
	token := ""
	for {
		pods := &corev1.PodList{}
		if err := reader.List(ctx, pods, client.InNamespace(namespace), client.Limit(64), client.Continue(token)); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			for _, owner := range pod.OwnerReferences {
				if ownerUIDs[string(owner.UID)] || names[owner.Name] {
					return true, nil
				}
			}
		}
		if pods.Continue == "" {
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		token = pods.Continue
	}
}
