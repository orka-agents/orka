package kube

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	controlstore "github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// releasableStorageHold records hold-1 over an acknowledgement of dataset-a
// and acknowledges it, as an operator does after the recovery procedure.
func releasableStorageHold(t *testing.T) (*Store, client.Client, controlstore.ControllerEpochFence) {
	t.Helper()
	ctx := t.Context()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	require.NoError(t, kubeStore.RecordStorageAcknowledgement(ctx, fence, storageAck("state.v1", "dataset-a", 1, 8)))
	_, err := kubeStore.HoldStorage(ctx, fence, testStorageHold("hold-1"))
	require.NoError(t, err)
	object := controllerEpochObject(t, kubeClient)
	object.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] = "hold-1"
	require.NoError(t, kubeClient.Update(ctx, object))
	return kubeStore, kubeClient, fence
}

// A release whose status update committed but whose response was lost is
// reconciled when it is retried: Kubernetes already shows this release, so
// the retry succeeds without writing again.
func TestStorageHoldReleaseWithALostResponseSucceedsOnRetry(t *testing.T) {
	ctx := t.Context()
	kubeStore, kubeClient, fence := releasableStorageHold(t)
	withWatch, ok := kubeClient.(client.WithWatch)
	require.True(t, ok, "fake client does not implement client.WithWatch")
	var updates atomic.Int32
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if err := delegate.SubResource(subresource).Update(ctx, object, options...); err != nil {
				return err
			}
			if _, ok := object.(*corev1alpha1.ControllerEpoch); ok && updates.Add(1) == 1 {
				return errors.New("connection reset by peer before the response")
			}
			return nil
		},
	})
	acks := []controlstore.StorageAcknowledgement{storageAck("state.v1", "dataset-a", 1, 2), storageAck("artifacts.v1", "dataset-b", 1, 0)}
	require.Error(t, kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", acks), "the lost response")
	require.NoError(t, kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", acks), "the retry of the committed release")
	require.EqualValues(t, 1, updates.Load(), "the retry writes nothing again")
	record := requireStorageRecord(t, kubeStore)
	require.Nil(t, record.Hold)
	state, _ := record.Acknowledgement("state.v1")
	artifacts, _ := record.Acknowledgement("artifacts.v1")
	require.EqualValues(t, 2, state.Watermark)
	require.Equal(t, "dataset-b", artifacts.DatasetID)
}

// A retried release that Kubernetes does not show as this exact release
// stays a conflict: another release of the hold recorded other
// acknowledgements, the record changed since the release, or the retry names
// a hold that was never released.
func TestStorageHoldReleaseRetryThatDoesNotMatchConflicts(t *testing.T) {
	released := []controlstore.StorageAcknowledgement{storageAck("state.v1", "dataset-a", 1, 2)}
	for name, test := range map[string]struct {
		after func(t *testing.T, kubeStore *Store, fence controlstore.ControllerEpochFence)
		id    string
		acks  []controlstore.StorageAcknowledgement
	}{
		"other acknowledgements": {
			id: "hold-1", acks: []controlstore.StorageAcknowledgement{storageAck("state.v1", "dataset-a", 1, 3)},
		},
		"fewer acknowledgements": {id: "hold-1", acks: nil},
		"record changed since": {
			after: func(t *testing.T, kubeStore *Store, fence controlstore.ControllerEpochFence) {
				require.NoError(t, kubeStore.RecordStorageWatermark(t.Context(), fence, "dataset-a", 5, testNow))
			},
			id: "hold-1", acks: released,
		},
		"another hold": {id: "hold-2", acks: released},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			kubeStore, _, fence := releasableStorageHold(t)
			require.NoError(t, kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", released))
			if test.after != nil {
				test.after(t, kubeStore, fence)
			}
			err := kubeStore.ReleaseStorageHold(ctx, fence, test.id, test.acks)
			require.ErrorIs(t, err, controlstore.ErrConflict)
		})
	}
}

// A release by another controller epoch is never taken for this one: the
// retry under a replaced fence is fenced.
func TestStorageHoldReleaseRetryUnderAReplacedFenceIsFenced(t *testing.T) {
	ctx := t.Context()
	kubeStore, _, fence := releasableStorageHold(t)
	acks := []controlstore.StorageAcknowledgement{storageAck("state.v1", "dataset-a", 1, 2)}
	require.NoError(t, kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", acks))
	_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2, HolderID: "controller-b",
		RequestDigest: testDigest("epoch-2"), UpdatedAt: testNow,
	})
	require.NoError(t, err)
	require.ErrorIs(t, kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", acks), controlstore.ErrFenced)
}
