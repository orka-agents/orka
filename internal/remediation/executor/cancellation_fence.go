package executor

import (
	"context"
	"reflect"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) rejectCancellationFence(ctx context.Context, namespace, name string) error {
	fence := &corev1.ConfigMap{}
	err := b.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pv.PreparationCancellationFenceName(name)}, fence)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	return contract.ErrConflict
}

// The cancellation fence deliberately outlives both Tasks. Parent garbage
// collection must not authorize another preparation after receipt settlement.
func (b *Backend) ensureCancellationFence(ctx context.Context, receipt contract.ExecutionReceipt) error {
	expected := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: pv.PreparationCancellationFenceName(receipt.TaskName), Namespace: receipt.Namespace},
		Immutable: new(true), Data: map[string]string{"taskUID": receipt.TaskUID, "inputDigest": receipt.InputDigest}}
	current := &corev1.ConfigMap{}
	err := b.reader.Get(ctx, client.ObjectKeyFromObject(expected), current)
	if apierrors.IsNotFound(err) {
		if err := b.client.Create(ctx, expected); err != nil && !apierrors.IsAlreadyExists(err) {
			return safeError(contract.ErrUnavailable, err)
		}
		err = b.reader.Get(ctx, client.ObjectKeyFromObject(expected), current)
	}
	if err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if current.UID == "" || !current.DeletionTimestamp.IsZero() || !ptr.Deref(current.Immutable, false) ||
		!reflect.DeepEqual(current.Data, expected.Data) || len(current.BinaryData) != 0 || len(current.OwnerReferences) != 0 {
		return contract.ErrConflict
	}
	return nil
}
