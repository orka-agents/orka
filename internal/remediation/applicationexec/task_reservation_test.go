package applicationexec

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func reservedRun(t *testing.T, f *fixture) *run {
	t.Helper()
	f.backend.qualified.Store(true)
	request, digest, err := f.backend.requestInput(f.request)
	require.NoError(t, err)
	r, err := f.backend.createRun(t.Context(), request, digest, false)
	require.NoError(t, err)
	require.Empty(t, r.state.Task.UID)
	require.False(t, r.state.Task.Attempted)
	return r
}

func TestNeverDispatchedReservationSettlesThroughCleanupSweep(t *testing.T) {
	for _, reason := range []string{"cancelled", "expired", "parent-deleted"} {
		t.Run(reason, func(t *testing.T) {
			f := newFixture(t)
			r := reservedRun(t, f)
			stale, err := f.backend.load(t.Context(), r.object.Name)
			require.NoError(t, err)
			switch reason {
			case "cancelled":
				r.state.Cancelled = true
			case "expired":
				r.state.Deadline = r.state.CreatedAt.Add(time.Nanosecond)
			case "parent-deleted":
				parent := &corev1alpha1.Task{}
				require.NoError(t, f.control.Get(t.Context(), client.ObjectKey{Namespace: "control", Name: "parent"}, parent))
				uid := parent.UID
				require.NoError(t, f.control.Delete(t.Context(), parent, client.Preconditions{UID: &uid}))
			}
			require.NoError(t, f.backend.save(t.Context(), r))
			require.NoError(t, f.backend.ReconcileCleanup(t.Context()))
			current, err := f.backend.load(t.Context(), r.object.Name)
			require.NoError(t, err)
			require.True(t, current.state.Settled)
			require.True(t, current.state.Cancelled)
			require.False(t, current.state.Task.Attempted)
			require.False(t, current.state.Task.Rejected, "never dispatched is not an API rejection")
			require.Empty(t, current.state.Task.UID)
			require.Empty(t, current.state.Effects)
			require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
			require.ErrorIs(t, f.backend.ensureTask(t.Context(), stale, ""), contract.ErrConflict)
			require.True(t, apierrors.IsNotFound(f.control.Get(t.Context(),
				client.ObjectKey{Namespace: "control", Name: r.state.Task.Name}, &corev1alpha1.Task{})))
		})
	}
}

func TestReservationSettlementWinsAgainstLateDispatchIntent(t *testing.T) {
	f := newFixture(t)
	r := reservedRun(t, f)
	recovery, err := New(f.control, f.control, f.target, f.target, f.backend.targetKube, f.store, f.scheme, f.config)
	require.NoError(t, err)
	injected, taskCreates := false, 0
	f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if cm, ok := object.(*corev1.ConfigMap); ok && !injected {
				var proposed state
				require.NoError(t, json.Unmarshal([]byte(cm.Data[stateKey]), &proposed))
				if proposed.Task.Attempted {
					injected = true
					require.NoError(t, recovery.recoverRetained(ctx))
				}
			}
			return c.Patch(ctx, object, patch, options...)
		},
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, ok := object.(*corev1alpha1.Task); ok {
				taskCreates++
			}
			return c.Create(ctx, object, options...)
		},
	})
	require.Error(t, f.backend.ensureTask(t.Context(), r, ""))
	require.True(t, injected)
	require.Zero(t, taskCreates)
	current, err := recovery.load(t.Context(), r.object.Name)
	require.NoError(t, err)
	require.True(t, current.state.Settled)
	require.False(t, current.state.Task.Attempted)
}

func TestDispatchIntentWinsAndRemainsUncertainWithoutCreateResponse(t *testing.T) {
	f := newFixture(t)
	r := reservedRun(t, f)
	recovery, err := New(f.control, f.control, f.target, f.target, f.backend.targetKube, f.store, f.scheme, f.config)
	require.NoError(t, err)
	injected := false
	f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if err := c.Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			if cm, ok := object.(*corev1.ConfigMap); ok && !injected {
				var proposed state
				require.NoError(t, json.Unmarshal([]byte(cm.Data[stateKey]), &proposed))
				if proposed.Task.Attempted {
					injected = true
					require.ErrorIs(t, recovery.ReconcileCleanup(ctx), ErrUncertain)
				}
			}
			return nil
		},
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, ok := object.(*corev1alpha1.Task); ok {
				return apierrors.NewTimeoutError("fixture response unavailable", 0)
			}
			return c.Create(ctx, object, options...)
		},
	})
	require.ErrorIs(t, f.backend.ensureTask(t.Context(), r, ""), ErrUncertain)
	require.True(t, injected)
	current, err := recovery.load(t.Context(), r.object.Name)
	require.NoError(t, err)
	require.True(t, current.state.Task.Attempted)
	require.False(t, current.state.Settled)
	require.Empty(t, current.state.Task.UID)
	require.ErrorIs(t, recovery.recoverRetained(t.Context()), ErrUncertain)
}
