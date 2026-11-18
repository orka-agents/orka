package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

type noEffectsStateOutage struct {
	operation string
	remaining atomic.Int32
	blocked   atomic.Bool
	hits      atomic.Int32
}

func (o *noEffectsStateOutage) fail() bool {
	if o.blocked.Load() {
		o.hits.Add(1)
		return true
	}
	for {
		remaining := o.remaining.Load()
		if remaining <= 0 {
			return false
		}
		if o.remaining.CompareAndSwap(remaining, remaining-1) {
			o.hits.Add(1)
			return true
		}
	}
}

func installNoEffectsStateOutage(f *serviceFixture, outage *noEffectsStateOutage) {
	f.t.Helper()
	wrapped := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			deadline, bounded := ctx.Deadline()
			_, configMap := object.(*corev1.ConfigMap)
			if outage.operation == "get" && configMap && key.Name == stateName("request-one") &&
				bounded && time.Until(deadline) <= noEffectsSettlementTimeout && outage.fail() {
				return fmt.Errorf("synthetic API read: %w", apierrors.NewServiceUnavailable("unavailable"))
			}
			return c.Get(ctx, key, object, options...)
		},
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if configMap, ok := object.(*corev1.ConfigMap); ok && outage.operation == "update" &&
				configMap.Name == stateName("request-one") {
				var next state
				if json.Unmarshal([]byte(configMap.Data["state"]), &next) == nil {
					if op := next.Executions["original"]; op != nil && !op.Attempted && !op.Rejected && outage.fail() {
						return fmt.Errorf("synthetic API write: %w", apierrors.NewTimeoutError("unavailable", 0))
					}
				}
			}
			return c.Update(ctx, object, options...)
		},
	})
	f.service.config.Client, f.service.config.Reader = wrapped, wrapped
}

func TestNoEffectsProofSurvivesTransientStateFailures(t *testing.T) {
	for _, operation := range []string{"get", "update"} {
		for _, failures := range []int32{1, 3} {
			t.Run(fmt.Sprintf("%s-%d", operation, failures), func(t *testing.T) {
				f := newServiceFixture(t)
				outage := &noEffectsStateOutage{operation: operation}
				installNoEffectsStateOutage(f, outage)
				f.backend.beforeStart = func(context.Context, contract.ExecutionRequest) error {
					if f.backend.startCalls.Load() == 1 {
						outage.remaining.Store(failures)
						return beforeEffectFailure{cause: contract.ErrUnavailable, retryable: true}
					}
					return nil
				}
				f.start()
				request := f.request(contract.Validate)
				_, err := f.service.Submit(t.Context(), "private", "subject", request)
				require.NoError(t, err)
				status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
				require.Equal(t, contract.PhaseSucceeded, status.Phase)
				require.Equal(t, failures, outage.hits.Load())
				require.EqualValues(t, 2, f.backend.startCalls.Load())
				f.backend.mu.Lock()
				require.Len(t, f.backend.requests, 1, "transient bookkeeping errors created replacement work")
				f.backend.mu.Unlock()
				require.EventuallyWithT(t, func(c *assert.CollectT) {
					require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
				}, 5*time.Second, time.Millisecond)
			})
		}
	}
}

func TestNoEffectsProofRetainedBeyondOnePersistenceWindow(t *testing.T) {
	f := newServiceFixture(t)
	outage := &noEffectsStateOutage{operation: "update"}
	installNoEffectsStateOutage(f, outage)
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
		status, err := f.service.Get(t.Context(), "private", request.RequestID)
		return err == nil && contract.TerminalPhase(status.Phase)
	}, noEffectsSettlementTimeout+100*time.Millisecond, 10*time.Millisecond)
	require.EqualValues(t, 1, f.backend.startCalls.Load())
	outage.blocked.Store(false)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseSucceeded, status.Phase)
	require.EqualValues(t, 2, f.backend.startCalls.Load())
	f.backend.mu.Lock()
	require.Len(t, f.backend.requests, 1)
	f.backend.mu.Unlock()
}

func TestNoEffectsStateOutageDuringCancellationOrShutdown(t *testing.T) {
	for _, operation := range []string{"get", "update"} {
		for _, shutdown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-shutdown-%t", operation, shutdown), func(t *testing.T) {
				f := newServiceFixture(t)
				outage := &noEffectsStateOutage{operation: operation}
				installNoEffectsStateOutage(f, outage)
				entered := make(chan struct{})
				f.backend.beforeStart = func(ctx context.Context, _ contract.ExecutionRequest) error {
					if f.backend.startCalls.Load() == 1 {
						close(entered)
						<-ctx.Done()
						outage.remaining.Store(2)
						return beforeEffectFailure{cause: ctx.Err()}
					}
					return nil
				}
				stop := f.start()
				request := f.request(contract.Validate)
				_, err := f.service.Submit(t.Context(), "private", "subject", request)
				require.NoError(t, err)
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("pre-effect admission not reached")
				}
				if shutdown {
					stop()
					saved, err := f.service.load(t.Context(), request.RequestID)
					require.NoError(t, err)
					require.False(t, saved.state.Executions["original"].Attempted)
					f.service, err = New(f.service.config)
					require.NoError(t, err)
					f.start()
				} else {
					_, err := f.service.Cancel(t.Context(), "private", request.RequestID)
					require.NoError(t, err)
				}
				require.EventuallyWithT(t, func(c *assert.CollectT) {
					status, err := f.service.Get(t.Context(), "private", request.RequestID)
					require.NoError(c, err)
					want := contract.PhaseCancelled
					if shutdown {
						want = contract.PhaseSucceeded
					}
					require.Equal(c, want, status.Phase)
					require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
				}, 5*time.Second, 5*time.Millisecond)
				require.NoError(t, f.service.CheckReady(t.Context()))
				require.EqualValues(t, 2, outage.hits.Load())
				f.backend.mu.Lock()
				if shutdown {
					require.Len(t, f.backend.requests, 1)
				} else {
					require.Empty(t, f.backend.requests)
				}
				f.backend.mu.Unlock()
			})
		}
	}
}

func TestDelayedNoEffectsProofCannotResetLaterAdmission(t *testing.T) {
	f := newServiceFixture(t)
	var advanced atomic.Bool
	var oldAttempt atomic.Uint64
	wrapped := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			configMap, ok := object.(*corev1.ConfigMap)
			if !ok || configMap.Name != stateName("request-one") || advanced.Load() {
				return c.Update(ctx, object, options...)
			}
			var next state
			if json.Unmarshal([]byte(configMap.Data["state"]), &next) != nil {
				return c.Update(ctx, object, options...)
			}
			op := next.Executions["original"]
			if op == nil || op.AdmissionAttempt == 0 || op.Attempted || op.Rejected || !advanced.CompareAndSwap(false, true) {
				return c.Update(ctx, object, options...)
			}
			oldAttempt.Store(op.AdmissionAttempt)
			if err := c.Update(ctx, object, options...); err != nil {
				return err
			}
			// The reset committed, but its acknowledgement is lost. Simulate a
			// new leader beginning the next admission before the old retry.
			current := new(corev1.ConfigMap)
			if err := c.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
				return err
			}
			var saved state
			if err := json.Unmarshal([]byte(current.Data["state"]), &saved); err != nil {
				return err
			}
			saved.Executions["original"].AdmissionAttempt++
			saved.Executions["original"].Attempted = true
			data, err := json.Marshal(saved)
			if err != nil {
				return err
			}
			current.Data["state"] = string(data)
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			return apierrors.NewServiceUnavailable("lost acknowledgement")
		},
	})
	f.service.config.Client, f.service.config.Reader = wrapped, wrapped
	f.backend.beforeStart = func(context.Context, contract.ExecutionRequest) error {
		return beforeEffectFailure{cause: contract.ErrUnavailable, retryable: true}
	}
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseNeedsInput, status.Phase)
	require.True(t, advanced.Load())
	saved, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	op := saved.state.Executions["original"]
	require.Equal(t, oldAttempt.Load()+1, op.AdmissionAttempt)
	require.True(t, op.Attempted, "old proof reset a different in-flight admission")
	require.False(t, op.Rejected)
	require.Nil(t, op.Receipt)
	require.EqualValues(t, 1, f.backend.startCalls.Load())
	require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), f.client, "private"), contract.ErrConflict)
}

func TestUnpersistedNoEffectsDoesNotAuthorizeReplayAfterProcessLoss(t *testing.T) {
	f := newServiceFixture(t)
	outage := &noEffectsStateOutage{operation: "update"}
	installNoEffectsStateOutage(f, outage)
	entered := make(chan struct{})
	f.backend.beforeStart = func(ctx context.Context, _ contract.ExecutionRequest) error {
		if f.backend.startCalls.Load() == 1 {
			close(entered)
			<-ctx.Done()
			outage.blocked.Store(true)
			return beforeEffectFailure{cause: ctx.Err()}
		}
		return nil
	}
	stop := f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pre-effect admission not reached")
	}
	stop()
	outage.blocked.Store(false)
	saved, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.True(t, saved.state.Executions["original"].Attempted)
	// A fresh process has no trustworthy in-memory proof. Durable uncertainty
	// remains non-replayable even though the previous process knew more.
	f.service, err = New(f.service.config)
	require.NoError(t, err)
	f.start()
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseNeedsInput, status.Phase)
	require.EqualValues(t, 1, f.backend.startCalls.Load())
	require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), f.client, "private"), contract.ErrConflict)
}
