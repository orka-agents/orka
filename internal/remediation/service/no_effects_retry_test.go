package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

type beforeEffectFailure struct {
	cause     error
	retryable bool
}

func (e beforeEffectFailure) Error() string   { return "backend failed before effects" }
func (e beforeEffectFailure) Unwrap() error   { return e.cause }
func (beforeEffectFailure) NoEffects() bool   { return true }
func (e beforeEffectFailure) Retryable() bool { return e.retryable }

func TestRetryableNoEffectsRetriesSameIdentityAndOneWorkload(t *testing.T) {
	f := newServiceFixture(t)
	var mu sync.Mutex
	var requests []contract.ExecutionRequest
	f.backend.beforeStart = func(_ context.Context, request contract.ExecutionRequest) error {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, request)
		if len(requests) == 1 {
			return fmt.Errorf("safe wrapper: %w", beforeEffectFailure{cause: contract.ErrUnavailable, retryable: true})
		}
		return nil
	}
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseSucceeded, status.Phase)
	require.Equal(t, contract.OutcomeReproduced, status.Outcome)
	mu.Lock()
	require.Len(t, requests, 2)
	require.Equal(t, requests[0], requests[1])
	mu.Unlock()
	f.backend.mu.Lock()
	require.Len(t, f.backend.requests, 1, "a retry of pre-effect admission created a second workload")
	f.backend.mu.Unlock()
	require.EqualValues(t, 2, f.backend.startCalls.Load())
}

func TestCancellationDuringNoEffectsIsDurablySettled(t *testing.T) {
	for _, retryable := range []bool{false, true} {
		t.Run(fmt.Sprintf("retryable-%t", retryable), func(t *testing.T) {
			f := newServiceFixture(t)
			var accountingWrites atomic.Int32
			var invalidAccountingContext atomic.Bool
			f.service.config.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
					if next, ok := object.(*corev1.ConfigMap); ok && next.Labels[stateLabel] == "v1" {
						var desired state
						if json.Unmarshal([]byte(next.Data["state"]), &desired) == nil {
							if op := desired.Executions["original"]; op != nil && !op.Attempted {
								prior := new(corev1.ConfigMap)
								var previous state
								if c.Get(ctx, client.ObjectKeyFromObject(next), prior) == nil &&
									json.Unmarshal([]byte(prior.Data["state"]), &previous) == nil {
									if before := previous.Executions["original"]; before != nil && before.Attempted {
										accountingWrites.Add(1)
										deadline, bounded := ctx.Deadline()
										if ctx.Err() != nil || !bounded || time.Until(deadline) <= 0 ||
											time.Until(deadline) > noEffectsSettlementTimeout {
											invalidAccountingContext.Store(true)
										}
									}
								}
							}
						}
					}
					return c.Update(ctx, object, options...)
				},
			})
			entered := make(chan struct{})
			f.backend.beforeStart = func(ctx context.Context, _ contract.ExecutionRequest) error {
				close(entered)
				<-ctx.Done()
				return beforeEffectFailure{cause: ctx.Err(), retryable: retryable}
			}
			f.start()
			request := f.request(contract.Generate)
			_, err := f.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("backend admission was not reached")
			}
			_, err = f.service.Cancel(t.Context(), "private", request.RequestID)
			require.NoError(t, err)
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				status, err := f.service.Get(t.Context(), "private", request.RequestID)
				require.NoError(c, err)
				require.Equal(c, contract.PhaseCancelled, status.Phase)
				require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
			}, 5*time.Second, 5*time.Millisecond)
			require.NoError(t, f.service.CheckReady(t.Context()))
			saved, err := f.service.load(t.Context(), request.RequestID)
			require.NoError(t, err)
			op := saved.state.Executions["original"]
			require.NotNil(t, op)
			require.False(t, op.Attempted)
			require.Nil(t, op.Receipt)
			require.Positive(t, accountingWrites.Load())
			require.False(t, invalidAccountingContext.Load())
			f.backend.mu.Lock()
			require.Empty(t, f.backend.requests)
			f.backend.mu.Unlock()
			require.EqualValues(t, 1, f.backend.startCalls.Load())
		})
	}
}

func TestInvalidNoEffectsCannotRetryEvenWithRetryableMarker(t *testing.T) {
	f := newServiceFixture(t)
	f.backend.beforeStart = func(context.Context, contract.ExecutionRequest) error {
		return beforeEffectFailure{cause: contract.ErrInvalid, retryable: true}
	}
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseFailed, status.Phase)
	saved, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.True(t, saved.state.Executions["original"].Attempted)
	require.True(t, saved.state.Executions["original"].Rejected)
	require.Nil(t, saved.state.Executions["original"].Receipt)
	require.EqualValues(t, 1, f.backend.startCalls.Load())
}

func TestRetryableNoEffectsAfterReceiptOnlyResumes(t *testing.T) {
	f := newServiceFixture(t)
	f.backend.afterReceiptError = beforeEffectFailure{cause: contract.ErrUnavailable, retryable: true}
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseSucceeded, status.Phase)
	saved, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.True(t, saved.state.Executions["original"].Attempted)
	require.False(t, saved.state.Executions["original"].Rejected)
	require.NotNil(t, saved.state.Executions["original"].Receipt)
	require.EqualValues(t, 1, f.backend.startCalls.Load())
	f.backend.mu.Lock()
	require.Equal(t, 1, f.backend.resumes)
	require.Len(t, f.backend.requests, 1)
	f.backend.mu.Unlock()
}

func TestShutdownDuringNoEffectsResumesPreEffectAdmission(t *testing.T) {
	f := newServiceFixture(t)
	entered := make(chan struct{})
	var attempts atomic.Int32
	f.backend.beforeStart = func(ctx context.Context, _ contract.ExecutionRequest) error {
		if attempts.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
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
		t.Fatal("backend admission was not reached")
	}
	stop()
	saved, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	op := saved.state.Executions["original"]
	require.False(t, op.Attempted, "shutdown discarded definitive proof that Start performed no effects")
	require.False(t, op.Rejected)
	digest, requestBlob := op.InputDigest, op.Request
	f.service, err = New(f.service.config)
	require.NoError(t, err)
	require.NoError(t, f.service.CheckReady(t.Context()))
	f.start()
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseSucceeded, status.Phase)
	saved, err = f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Equal(t, digest, saved.state.Executions["original"].InputDigest)
	require.Equal(t, requestBlob, saved.state.Executions["original"].Request)
	f.backend.mu.Lock()
	require.Len(t, f.backend.requests, 1)
	f.backend.mu.Unlock()
}
