package usage

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NamespaceUID(ctx context.Context, reader client.Reader, namespace string) (string, error) {
	if reader == nil {
		return "", fmt.Errorf("namespace identity reader is unavailable")
	}
	var current corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, &current); err != nil {
		return "", fmt.Errorf("read usage namespace identity: %w", err)
	}
	if current.UID == "" {
		return "", fmt.Errorf("usage namespace has no UID")
	}
	return string(current.UID), nil
}

// Read the namespace first, then fence the source object's UID. A stale cached
// Task or monitor cannot be relabeled with a recreated namespace's identity.
func NamespaceUIDForObject(ctx context.Context, reader client.Reader, object client.Object) (string, error) {
	uid, err := NamespaceUID(ctx, reader, object.GetNamespace())
	if err != nil {
		return "", err
	}
	current := object.DeepCopyObject().(client.Object)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
		return "", fmt.Errorf("verify usage source identity: %w", err)
	}
	if object.GetUID() == "" || current.GetUID() != object.GetUID() {
		return "", fmt.Errorf("usage source identity changed")
	}
	return uid, nil
}
