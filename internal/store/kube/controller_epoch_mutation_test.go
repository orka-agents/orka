package kube

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestControllerEpochMutationSerializesTakeoverAndReleasesOnError(t *testing.T) {
	kubeStore, _, fence := newTestStoreWithEpoch(t)
	ctx := t.Context()
	sentinel := errors.New("mutation failed")
	change := store.ControllerEpochCAS{ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: "successor", RequestDigest: testDigest("successor"), UpdatedAt: time.Now().UTC()}
	err := kubeStore.WithControllerEpochMutation(ctx, fence, func(mutationCtx context.Context) error {
		deadline, bounded := mutationCtx.Deadline()
		if !bounded || time.Until(deadline) > 21*time.Second {
			t.Fatal("external mutation did not receive a bounded context")
		}
		// Takeover uses a distinct store instance so this checks the durable
		// Lease authority, not just a per-instance synchronization primitive.
		successor, err := New(kubeStore.client, testControlNamespace, WithAPIReader(kubeStore.readClient()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := successor.CompareAndSwapControllerEpoch(ctx, change); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("takeover passed an active mutation fence: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error was lost: %v", err)
	}
	if _, err := kubeStore.CompareAndSwapControllerEpoch(ctx, change); err != nil {
		t.Fatalf("mutation lock was not released on failure: %v", err)
	}
	called := false
	err = kubeStore.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, store.ErrConflict) || called {
		t.Fatalf("stale owner callback called=%t, err=%v", called, err)
	}
}

func TestControllerEpochMutationRejectsMissingClientAndCallback(t *testing.T) {
	var missing *Store
	if err := missing.WithControllerEpochMutation(t.Context(), store.ControllerEpochFence{}, func(context.Context) error { t.Fatal("callback ran"); return nil }); err == nil {
		t.Fatal("nil control store accepted a mutation")
	}
	s, _, fence := newTestStoreWithEpoch(t)
	if err := s.WithControllerEpochMutation(t.Context(), fence, nil); err == nil {
		t.Fatal("nil callback was accepted")
	}
}

func TestControllerEpochMutationHeldLockContentionAndRecovery(t *testing.T) {
	owner, rawClient, fence := newTestStoreWithEpoch(t)
	_, snapshot, err := owner.requireControllerEpoch(t.Context(), fence)
	require.NoError(t, err)
	release := sync.OnceFunc(func() { owner.releaseControllerEpochMutation(snapshot) })
	t.Cleanup(release)
	// A distinct Store bypasses the owner's local queue and must acquire the
	// same durable Lease before its callback can run.
	contender, err := New(rawClient, testControlNamespace, WithAPIReader(rawClient))
	require.NoError(t, err)
	called := 0
	mutate := func(context.Context) error { called++; return nil }
	err = contender.WithControllerEpochMutation(t.Context(), fence, mutate)
	require.ErrorIs(t, err, store.ErrControllerEpochMutationContention)
	require.ErrorIs(t, err, store.ErrConflict)
	require.Contains(t, err.Error(), "serializing another control-store mutation")
	require.Zero(t, called)

	release()
	require.NoError(t, contender.WithControllerEpochMutation(t.Context(), fence, mutate))
	require.Equal(t, 1, called, "cleared contention must permit normal acquisition")
}

func TestControllerEpochMutationCASContentionAndRecovery(t *testing.T) {
	_, rawClient, fence := newTestStoreWithEpoch(t)
	withWatch, ok := rawClient.(client.WithWatch)
	require.True(t, ok)
	conflicting, conflicts := true, 0
	writer := interceptor.NewClient(withWatch, interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if lease, ok := object.(*coordinationv1.Lease); ok && conflicting && lease.Annotations[annotationMutationToken] != "" {
				conflicts++
				return apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
					lease.Name, errors.New("simulated concurrent lock update"))
			}
			return delegate.Update(ctx, object, options...)
		},
	})
	contender, err := New(writer, testControlNamespace, WithAPIReader(rawClient))
	require.NoError(t, err)
	called := 0
	mutate := func(context.Context) error { called++; return nil }
	err = contender.WithControllerEpochMutation(t.Context(), fence, mutate)
	require.ErrorIs(t, err, store.ErrControllerEpochMutationContention)
	require.ErrorIs(t, err, store.ErrConflict)
	require.Contains(t, err.Error(), "acquire controller epoch mutation fence")
	require.Greater(t, conflicts, 1, "lock-update conflicts must exhaust the existing retry window")
	require.Zero(t, called)

	conflicting = false
	require.NoError(t, contender.WithControllerEpochMutation(t.Context(), fence, mutate))
	require.Equal(t, 1, called)
}

func TestControllerEpochMutationDefinitiveFailuresAreNotContention(t *testing.T) {
	for _, failure := range []struct {
		name string
		kind error
	}{
		{name: "missing lease", kind: store.ErrConflict},
		{name: "missing object", kind: store.ErrConflict},
		{name: "epoch drift", kind: store.ErrConflict},
		{name: "holder drift", kind: store.ErrConflict},
		{name: "lease UID drift", kind: store.ErrConflict},
		{name: "invalid fence", kind: store.ErrValidation},
		{name: "invalid Lease", kind: store.ErrValidation},
		{name: "malformed mutation lock"},
		{name: "read conflict", kind: store.ErrConflict},
		{name: "cancelled caller", kind: context.Canceled},
	} {
		t.Run(failure.name, func(t *testing.T) {
			kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
			lease := &coordinationv1.Lease{}
			leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(fence.Name)}
			require.NoError(t, rawClient.Get(t.Context(), leaseKey, lease))
			ctx := t.Context()
			switch failure.name {
			case "missing lease":
				require.NoError(t, rawClient.Delete(ctx, lease))
			case "missing object":
				object := &corev1alpha1.ControllerEpoch{}
				require.NoError(t, rawClient.Get(ctx, client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(fence.Name)}, object))
				require.NoError(t, rawClient.Delete(ctx, object))
			case "epoch drift":
				_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
					ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2, HolderID: "successor",
					RequestDigest: testDigest("successor"), UpdatedAt: time.Now().UTC(),
				})
				require.NoError(t, err)
			case "holder drift":
				holder := "different-holder"
				lease.Spec.HolderIdentity = &holder
				require.NoError(t, rawClient.Update(ctx, lease))
			case "lease UID drift":
				require.NoError(t, rawClient.Delete(ctx, lease))
				lease.UID, lease.ResourceVersion = "", ""
				require.NoError(t, rawClient.Create(ctx, lease))
			case "invalid fence":
				fence.Epoch = 0
			case "invalid Lease":
				lease.Annotations[annotationRequestDigest] = "invalid-digest"
				require.NoError(t, rawClient.Update(ctx, lease))
			case "malformed mutation lock":
				lease.Annotations[annotationMutationToken] = "held"
				lease.Annotations[annotationMutationExpiresAt] = "invalid-expiry"
				require.NoError(t, rawClient.Update(ctx, lease))
			case "read conflict":
				withWatch, ok := rawClient.(client.WithWatch)
				require.True(t, ok)
				kubeStore.reader = interceptor.NewClient(withWatch, interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return apierrors.NewConflict(schema.GroupResource{Resource: "leases"}, lease.Name, errors.New("read conflict"))
					},
				})
			case "cancelled caller":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			called := false
			err := kubeStore.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil })
			require.Error(t, err)
			if failure.kind != nil {
				require.ErrorIs(t, err, failure.kind)
			}
			require.NotErrorIs(t, err, store.ErrControllerEpochMutationContention)
			require.False(t, called)
		})
	}
}
