package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

func TestExecutionReceiptRecoveryAfterCoordinatorRestart(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume retained result", true: "cancel real backend"}[cancelRun], func(t *testing.T) {
			f := newServiceFixture(t)
			f.service.config.BoundedExecution = true
			counted := &pauseAfterReceiptBackend{
				ExecutionBackend: f.backend, admitted: make(chan pausedAdmission, 1),
			}
			if cancelRun {
				counted.ExecutionBackend = newRealDeletionBackend(t, f.client)
			}
			f.service.config.Backend = counted
			var blocked atomic.Bool
			var failures atomic.Int32
			blocked.Store(true)
			wrapped := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
					if cm, ok := object.(*corev1.ConfigMap); ok && cm.Name == stateName("request-one") && blocked.Load() {
						var proposed state
						if json.Unmarshal([]byte(cm.Data["state"]), &proposed) == nil {
							if op := proposed.Executions["original"]; op != nil && op.Receipt != nil {
								failures.Add(1)
								return apierrors.NewServiceUnavailable("synthetic receipt persistence outage")
							}
						}
					}
					return c.Update(ctx, object, options...)
				},
			})
			f.service.config.Client, f.service.config.Reader = wrapped, wrapped
			stop := f.start()
			request := f.request(contract.Validate)
			_, err := f.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				if failures.Load() > 0 {
					return
				}
				current, err := f.service.load(t.Context(), request.RequestID)
				require.NoError(c, err)
				var roles []string
				for role, op := range current.state.Agents {
					roles = append(roles, fmt.Sprintf("%s(uid=%t,job=%t,claim=%t,output=%t)",
						role, op.UID != "", op.Job != nil, op.ModelCall != nil, !op.Output.empty()))
				}
				require.Positive(c, failures.Load(), "phase=%s reason=%s step=%s starts=%d roles=%s",
					current.state.Status.Phase, current.state.Status.Reason, current.state.Step,
					counted.starts.Load(), strings.Join(roles, ","))
			}, 5*time.Second, time.Millisecond)
			t.Log("receipt persistence fault exercised")
			stop()
			before, err := f.service.load(t.Context(), request.RequestID)
			require.NoError(t, err)
			require.True(t, before.state.Executions["original"].Attempted)
			require.Nil(t, before.state.Executions["original"].Receipt)
			require.False(t, before.state.Settled)
			require.EqualValues(t, 1, counted.starts.Load())

			blocked.Store(false)
			if cancelRun {
				counted.ExecutionBackend = newRealDeletionBackend(t, f.client)
			}
			f.service, err = New(f.service.config)
			require.NoError(t, err)
			if cancelRun {
				_, err = f.service.Cancel(t.Context(), "private", request.RequestID)
				require.NoError(t, err)
			}
			stop = f.start()
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
			}, 10*time.Second, time.Millisecond)
			stop()

			after, err := f.service.load(t.Context(), request.RequestID)
			require.NoError(t, err)
			require.True(t, after.state.Settled)
			require.NotNil(t, after.state.Executions["original"].Receipt)
			require.Equal(t, before.state.Deadline, after.state.Deadline)
			require.Equal(t, before.state.Parent.UID, after.state.Parent.UID)
			require.Equal(t, before.state.Executions["original"].AdmissionAttempt, after.state.Executions["original"].AdmissionAttempt)
			for role, operation := range before.state.Agents {
				require.Equal(t, operation.ModelCall, after.state.Agents[role].ModelCall, "recovery cannot replenish model permits")
			}
			require.EqualValues(t, 1, counted.starts.Load(), "receipt recovery must not call Start again")
			if cancelRun {
				require.Equal(t, contract.OutcomeCancelled, after.state.Status.Outcome)
			} else {
				require.Equal(t, contract.OutcomeReproduced, after.state.Status.Outcome, "status=%+v", after.state.Status)
			}
			require.True(t, apierrors.IsNotFound(f.client.Get(t.Context(),
				client.ObjectKey{Namespace: "private", Name: runSlotName}, &corev1.ConfigMap{})))
			next := f.request(contract.Validate)
			next.RequestID = "request-after-recovery"
			_, err = f.service.Submit(t.Context(), "private", "subject", next)
			require.NoError(t, err)
			nextRun, err := f.service.load(t.Context(), next.RequestID)
			require.NoError(t, err)
			require.NoError(t, f.service.acquireRunSlot(t.Context(), nextRun))
		})
	}
}
