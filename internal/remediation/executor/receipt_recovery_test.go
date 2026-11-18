package executor

import (
	"context"
	"errors"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRecoverExactReceiptAfterAcceptanceFailure(t *testing.T) {
	f := newFixture(t, true)
	rejected := errors.New("coordinator receipt was not persisted")
	var accepted contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(receipt contract.ExecutionReceipt) error {
		accepted = receipt
		return rejected
	})
	require.ErrorIs(t, err, rejected)
	require.Empty(t, f.jobs)
	restarted := *f.backend
	recovered, err := restarted.Recover(t.Context(), f.request)
	require.NoError(t, err)
	require.Equal(t, &accepted, recovered.Receipt)
	require.False(t, recovered.Settled)
	require.NoError(t, restarted.Cancel(t.Context(), *recovered.Receipt))
	require.Empty(t, f.jobs, "recovery and cancellation cannot dispatch a preparation Job")
}

func TestRecoverDoesNotAdoptTaskAfterUnknownCreate(t *testing.T) {
	f := newFixture(t, true)
	f.backend.client = interceptor.NewClient(f.backend.client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := c.Create(ctx, object, options...); err != nil {
				return err
			}
			if _, task := object.(*corev1alpha1.Task); task {
				return errors.New("Task create response was lost")
			}
			return nil
		},
	})
	_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error {
		t.Fatal("an unacknowledged create cannot be accepted")
		return nil
	})
	require.Error(t, err)
	recovered, err := f.backend.Recover(t.Context(), f.request)
	require.ErrorIs(t, err, contract.ErrConflict)
	require.Nil(t, recovered.Receipt)
	require.False(t, recovered.Settled)
	require.Empty(t, f.jobs)
}

func TestRecoverRejectsChangedRequestAndReplacedTask(t *testing.T) {
	f := newFixture(t, true)
	_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error {
		return errors.New("receipt not saved")
	})
	require.Error(t, err)
	recovered, err := f.backend.Recover(t.Context(), f.request)
	require.NoError(t, err)
	changed := f.request
	changed.Problem += " changed"
	_, err = f.backend.Recover(t.Context(), changed)
	require.Error(t, err)

	task := new(corev1alpha1.Task)
	require.NoError(t, f.backend.reader.Get(t.Context(),
		client.ObjectKey{Namespace: recovered.Receipt.Namespace, Name: recovered.Receipt.TaskName}, task))
	task.UID = "replacement-uid"
	task.Finalizers = nil
	// A replacement returned by the reader cannot change the durable receipt.
	f.backend.reader = interceptor.NewClient(f.backend.reader.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if current, ok := object.(*corev1alpha1.Task); ok && key.Name == task.Name {
				*current = *task.DeepCopy()
				return nil
			}
			return c.Get(ctx, key, object, options...)
		},
	})
	again, err := f.backend.Recover(t.Context(), f.request)
	require.NoError(t, err)
	require.Equal(t, recovered.Receipt, again.Receipt)
	require.ErrorIs(t, f.backend.Cancel(t.Context(), *again.Receipt), contract.ErrConflict)
	require.Empty(t, f.jobs)

	marker := new(corev1.ConfigMap)
	require.NoError(t, f.backend.reader.Get(t.Context(),
		client.ObjectKey{Namespace: task.Namespace, Name: task.Name + "-intent"}, marker))
	require.Equal(t, recovered.Receipt.TaskUID, marker.Data["taskUID"])
}
