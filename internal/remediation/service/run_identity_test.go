package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

func TestDriverUpdateRetryRechecksPinnedStateUID(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	old, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	expectedState := old.object.Data["state"]
	var replaced atomic.Bool
	f.service.config.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if object.GetName() == old.object.Name && replaced.CompareAndSwap(false, true) {
				replacement := old.object.DeepCopy()
				if err := c.Delete(ctx, replacement); err != nil {
					return err
				}
				replacement.UID, replacement.ResourceVersion = "", ""
				if err := c.Create(ctx, replacement); err != nil {
					return err
				}
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, object.GetName(), errors.New("replaced"))
			}
			return c.Update(ctx, object, options...)
		},
	})
	ctx := bindRunIdentity(t.Context(), runIdentity{namespace: "private", id: request.RequestID, uid: old.object.UID})
	var callbacks int
	_, err = f.service.update(ctx, request.RequestID, func(r *run) error {
		callbacks++
		r.state.CancelRequested = true
		r.state.StopReason = reasonCancelled
		return nil
	})
	require.ErrorIs(t, err, errRunIdentityChanged)
	require.Equal(t, 1, callbacks, "retry invoked the old mutation against replacement state")
	current := new(corev1.ConfigMap)
	require.NoError(t, f.client.Get(t.Context(), client.ObjectKeyFromObject(old.object), current))
	require.NotEqual(t, old.object.UID, current.UID)
	require.Equal(t, expectedState, current.Data["state"])
}

func TestOldDriverCannotConsumeNewRunNoEffectsProof(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	fresh, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	op := &executionOperation{
		Request: blob{Digest: pv.Digest([]byte("new request"))}, InputDigest: pv.Digest([]byte("new input")),
		Attempted: true, AdmissionAttempt: 1,
	}
	fresh.state.Executions["original"] = op
	require.NoError(t, f.service.save(t.Context(), fresh))
	key := noEffectsKey{run: request.RequestID, operation: "original"}
	proof := noEffectsProof{
		stateUID: fresh.object.UID, requestDigest: op.Request.Digest, inputDigest: op.InputDigest, attempt: 1, retry: true,
	}
	f.service.mu.Lock()
	f.service.noEffects[key] = proof
	f.service.mu.Unlock()
	stale := bindRunIdentity(t.Context(), runIdentity{namespace: "private", id: request.RequestID, uid: types.UID("old-state-uid")})
	require.ErrorIs(t, f.service.flushNoEffects(stale, request.RequestID), errRunIdentityChanged)
	f.service.mu.Lock()
	require.Equal(t, proof, f.service.noEffects[key])
	f.service.mu.Unlock()
	current, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.True(t, current.state.Executions["original"].Attempted)
	live := bindRunIdentity(t.Context(), runIdentity{namespace: "private", id: request.RequestID, uid: fresh.object.UID})
	require.NoError(t, f.service.flushNoEffects(live, request.RequestID))
	current, err = f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.False(t, current.state.Executions["original"].Attempted)
}

func TestQueuedDriverCannotAdoptReplacementOnInitialLoad(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	old, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	queued := runIdentity{namespace: "private", id: request.RequestID, uid: old.object.UID}
	require.NoError(t, f.client.Delete(t.Context(), old.object))
	_, err = f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	fresh, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	f.service.drive(ctx, ctx, queued)
	after, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Equal(t, fresh.object, after.object)
	require.Empty(t, after.state.Parent.Name)
	require.Empty(t, after.state.Executions)
	require.Zero(t, f.backend.startCalls.Load())
}
