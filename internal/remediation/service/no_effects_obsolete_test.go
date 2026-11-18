package service

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

func TestObsoleteNoEffectsProofReleasesDriveWithoutReplay(t *testing.T) {
	for _, mutation := range []string{"deleted", "replaced", "malformed", "operation-removed", "operation-mismatch", "forbidden", "invalid"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			f := newServiceFixture(t)
			outage := &noEffectsStateOutage{operation: "update"}
			installNoEffectsStateOutage(f, outage)
			var permanent atomic.Bool
			wrapped := interceptor.NewClient(f.service.config.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if permanent.Load() && mutation == "forbidden" && key.Name == stateName("request-one") {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
					}
					return c.Get(ctx, key, object, options...)
				},
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
					if permanent.Load() && mutation == "invalid" && object.GetName() == stateName("request-one") {
						return apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, object.GetName(),
							field.ErrorList{field.Invalid(field.NewPath("data"), "", "rejected")})
					}
					return c.Update(ctx, object, options...)
				},
			})
			f.service.config.Client, f.service.config.Reader = wrapped, wrapped
			f.backend.beforeStart = func(context.Context, contract.ExecutionRequest) error {
				if f.backend.startCalls.Load() == 1 {
					outage.blocked.Store(true)
					return beforeEffectFailure{cause: contract.ErrUnavailable, retryable: true}
				}
				return nil
			}
			f.start()
			request := f.request(contract.Validate)
			_, err := f.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			require.Eventually(t, func() bool { return outage.hits.Load() > 0 }, 5*time.Second, time.Millisecond)
			require.Never(t, func() bool {
				f.service.mu.Lock()
				defer f.service.mu.Unlock()
				_, active := f.service.active[request.RequestID]
				_, proof := f.service.noEffects[noEffectsKey{run: request.RequestID, operation: "original"}]
				return !active || !proof
			}, noEffectsSettlementTimeout+100*time.Millisecond, 10*time.Millisecond)

			key := client.ObjectKey{Namespace: "private", Name: stateName(request.RequestID)}
			object := new(corev1.ConfigMap)
			require.NoError(t, f.client.Get(t.Context(), key, object))
			var before state
			require.NoError(t, json.Unmarshal([]byte(object.Data["state"]), &before))
			require.True(t, before.Executions["original"].Attempted)
			require.Nil(t, before.Executions["original"].Receipt)

			// Keep replacement/corrupt records out of the queue so this test
			// observes release of the existing drive, not a newly queued one.
			object.Labels[activeLabel] = boolFalse
			switch mutation {
			case "deleted":
				require.NoError(t, f.client.Delete(t.Context(), object))
			case "replaced":
				require.NoError(t, f.client.Delete(t.Context(), object))
				object.UID, object.ResourceVersion = "", ""
				require.NoError(t, f.client.Create(t.Context(), object))
			case "malformed":
				object.Data["state"] = "{invalid state"
				require.NoError(t, f.client.Update(t.Context(), object))
			case "forbidden", "invalid":
				require.NoError(t, f.client.Update(t.Context(), object))
				permanent.Store(true)
			default:
				if mutation == "operation-removed" {
					delete(before.Executions, "original")
				} else {
					before.Executions["original"].InputDigest = pv.Digest([]byte("different operation"))
				}
				data, err := json.Marshal(before)
				require.NoError(t, err)
				object.Data["state"] = string(data)
				require.NoError(t, f.client.Update(t.Context(), object))
			}
			expectedState := object.Data["state"]
			outage.blocked.Store(false)
			require.Eventually(t, func() bool {
				f.service.mu.Lock()
				defer f.service.mu.Unlock()
				_, active := f.service.active[request.RequestID]
				_, proof := f.service.noEffects[noEffectsKey{run: request.RequestID, operation: "original"}]
				return !active && !proof
			}, 2*time.Second, 5*time.Millisecond, "obsolete proof retained a run slot or retry goroutine")
			require.EqualValues(t, 1, f.backend.startCalls.Load())
			f.backend.mu.Lock()
			require.Empty(t, f.backend.requests)
			f.backend.mu.Unlock()
			current := new(corev1.ConfigMap)
			err = f.client.Get(t.Context(), key, current)
			if mutation == "deleted" {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				require.Equal(t, expectedState, current.Data["state"], "discarding proof rewrote durable uncertainty")
			}
		})
	}
}

func TestDiscardObsoleteProofKeepsNewerProofAtSameKey(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	key := noEffectsKey{run: request.RequestID, operation: "original"}
	old := noEffectsProof{stateUID: types.UID("old-state-uid"), inputDigest: pv.Digest([]byte("old")), attempt: 1, retry: true}
	newer := noEffectsProof{stateUID: r.object.UID, inputDigest: pv.Digest([]byte("new")), attempt: 2, retry: true}
	f.service.mu.Lock()
	f.service.noEffects[key] = newer
	f.service.mu.Unlock()
	applied, err := f.service.persistNoEffects(t.Context(), key, old)
	require.ErrorIs(t, err, errNoEffectsObsolete)
	require.False(t, applied)
	f.service.mu.Lock()
	require.Equal(t, newer, f.service.noEffects[key])
	f.service.mu.Unlock()
	current, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Empty(t, current.state.Executions)
}

func TestObsoleteProofDoesNotBlockFreshDriveForRecreatedRun(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	old, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	key := noEffectsKey{run: request.RequestID, operation: "original"}
	f.service.mu.Lock()
	f.service.noEffects[key] = noEffectsProof{
		stateUID: old.object.UID, inputDigest: pv.Digest([]byte("old admission")), attempt: 1, retry: true,
	}
	f.service.mu.Unlock()
	require.NoError(t, f.client.Delete(t.Context(), old.object))
	_, err = f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	fresh, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.NotEqual(t, old.object.UID, fresh.object.UID)
	f.start()
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseSucceeded, status.Phase)
	require.Equal(t, contract.OutcomeReproduced, status.Outcome)
	require.EqualValues(t, 1, f.backend.startCalls.Load())
	f.service.mu.Lock()
	require.Empty(t, f.service.noEffects)
	f.service.mu.Unlock()
	fresh, err = f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.EqualValues(t, 1, fresh.state.Executions["original"].AdmissionAttempt)
	require.NotNil(t, fresh.state.Executions["original"].Receipt)
}
