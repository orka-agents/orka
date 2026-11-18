package service

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

// The gate hides only replacement dispatch candidates. Gets and writes remain
// untouched so an old driver can still expose a broken UID fence.
type recreatedRunDispatchGate struct {
	client.Reader
	mu  sync.Mutex
	old runIdentity
}

func (r *recreatedRunDispatchGate) blockReplacement(old runIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.old = old
}

func (r *recreatedRunDispatchGate) allowReplacement() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.old = runIdentity{}
}

func (r *recreatedRunDispatchGate) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, options...); err != nil {
		return err
	}
	items, ok := list.(*corev1.ConfigMapList)
	if !ok {
		return nil
	}
	r.mu.Lock()
	old := r.old
	r.mu.Unlock()
	if old.uid != "" {
		items.Items = slices.DeleteFunc(items.Items, func(item corev1.ConfigMap) bool {
			return item.Namespace == old.namespace && item.Name == stateName(old.id) && item.UID != old.uid
		})
	}
	return nil
}

func TestRecreatedRunDispatchGateFencesInFlightList(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	old, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	started, resume := make(chan struct{}), make(chan struct{})
	var first, release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	dispatch := &recreatedRunDispatchGate{Reader: interceptor.NewClient(fixture.client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			first.Do(func() {
				close(started)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			})
			return c.List(ctx, list, options...)
		},
	})}
	type listResult struct {
		items []corev1.ConfigMap
		err   error
	}
	listed := make(chan listResult, 1)
	go func() {
		items, err := listPendingStates(t.Context(), dispatch, "private")
		listed <- listResult{items: items, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pending-state list did not start")
	}
	dispatch.blockReplacement(runIdentity{namespace: "private", id: request.RequestID, uid: old.object.UID})
	require.NoError(t, fixture.client.Delete(t.Context(), old.object))
	_, err = fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	fresh, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.NotEqual(t, old.object.UID, fresh.object.UID)
	require.Equal(t, boolTrue, fresh.object.Labels[activeLabel])
	release.Do(func() { close(resume) })
	select {
	case result := <-listed:
		require.NoError(t, result.err)
		require.Empty(t, result.items, "a list started before the gate closed must not dispatch the replacement")
	case <-time.After(5 * time.Second):
		t.Fatal("pending-state list did not finish")
	}
	visible := new(corev1.ConfigMap)
	require.NoError(t, dispatch.Get(t.Context(), client.ObjectKeyFromObject(fresh.object), visible))
	require.Equal(t, fresh.object.UID, visible.UID, "the gate must not hide replacement state from old-driver reads")
	dispatch.allowReplacement()
	items, err := listPendingStates(t.Context(), dispatch, "private")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, fresh.object.UID, items[0].UID)
}

func TestServiceFixtureRejectsStaleRecreatedStateWrite(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	old, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.NoError(t, fixture.client.Delete(t.Context(), old.object))
	_, err = fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	fresh, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.NotEqual(t, old.object.UID, fresh.object.UID)
	// Per-object fake resource versions restart at creation and let this stale
	// update restore the deleted UID, which the API server would reject.
	ctx := bindRunIdentity(t.Context(), runIdentity{namespace: "private", id: request.RequestID, uid: old.object.UID})
	require.ErrorIs(t, fixture.service.save(ctx, old), contract.ErrConflict)
	after, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Equal(t, fresh.object, after.object)
}
