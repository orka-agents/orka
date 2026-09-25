package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/gateway/referenceadapter"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func newGatewayMessageFixture(t *testing.T) (*Service, *sqlite.Store, *referenceadapter.Server, *store.GatewayEvent, *corev1alpha1.Task) {
	t.Helper()
	s, db, adapter := newGatewayServiceFixture(t)
	setGatewayAgentNativeAI(t, s, true)
	updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
		g.Status.ObservedCapabilities = &gatewayv1alpha1.GatewayObservedCapabilities{ContractVersion: protocol.Version, Capabilities: gatewayv1alpha1.GatewayCapabilities{InterimDelivery: true}}
	})
	accepted, err := s.AdmitEvent(t.Context(), "default", "chat", "Bearer inbound-token", gatewayEventBody(t, "interim", "user-1"))
	require.NoError(t, err)
	require.NoError(t, s.DispatchOnce(t.Context()))
	event, err := db.GetGatewayEvent(t.Context(), "default", accepted.EventID)
	require.NoError(t, err)
	task := &corev1alpha1.Task{}
	require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: event.TaskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	require.NoError(t, s.Client.Status().Update(t.Context(), task))
	return s, db, adapter, event, task
}

func updateMessageGateway(t *testing.T, s *Service, update func(*gatewayv1alpha1.Gateway)) {
	t.Helper()
	g := &gatewayv1alpha1.Gateway{}
	require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "chat"}, g))
	update(g)
	status := g.Status
	require.NoError(t, s.Client.Update(t.Context(), g))
	g.Status = status
	require.NoError(t, s.Client.Status().Update(t.Context(), g))
}

func TestGatewayTaskMessageReceiptDoesNotProjectTerminal(t *testing.T) {
	for _, phase := range []corev1alpha1.TaskPhase{corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			s, db, adapter, event, task := newGatewayMessageFixture(t)
			original := task.DeepCopy()
			sessionBefore, err := db.GetSession(t.Context(), "default", event.SessionName)
			require.NoError(t, err)
			receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "progress-1", "first update")
			require.NoError(t, err)
			require.True(t, receipt.Created)
			require.Equal(t, store.GatewayDeliveryPending, receipt.Status)
			duplicate, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "progress-1", "first update")
			require.NoError(t, err)
			require.False(t, duplicate.Created)
			require.Equal(t, receipt.DeliveryID, duplicate.DeliveryID)
			require.NoError(t, s.DeliverOnce(t.Context()))
			duplicate, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "progress-1", "first update")
			require.NoError(t, err)
			require.Equal(t, store.GatewayDeliveryDelivered, duplicate.Status)
			require.Equal(t, receipt.DeliveryID, duplicate.DeliveryID)
			require.NoError(t, s.Client.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Equal(t, original, task, "interim must never patch Task correlation or content")
			currentEvent, err := db.GetGatewayEvent(t.Context(), event.Namespace, event.ID)
			require.NoError(t, err)
			require.Equal(t, event, currentEvent)
			sessionAfter, err := db.GetSession(t.Context(), "default", event.SessionName)
			require.NoError(t, err)
			require.Equal(t, sessionBefore, sessionAfter, "interim must preserve transcript and lock")

			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "progress-2", "second update")
			require.NoError(t, err)
			task.Status.Phase = phase
			require.NoError(t, db.SaveResult(t.Context(), task.Namespace, task.Name, []byte("final response")))
			require.NoError(t, s.Client.Status().Update(t.Context(), task))
			require.NoError(t, s.ProjectTerminals(t.Context()))
			require.NoError(t, s.DeliverOnce(t.Context()), "accepted messages drain even after terminal projection")
			require.NoError(t, s.DeliverOnce(t.Context()))
			sends := adapter.Deliveries()
			require.Len(t, sends, 3)
			require.Equal(t, "message", sends[0].Kind)
			require.Equal(t, "first update", sends[0].Text)
			require.Equal(t, "message", sends[1].Kind)
			wantKind := "final"
			if phase == corev1alpha1.TaskPhaseFailed {
				wantKind = "error"
			}
			require.Equal(t, wantKind, sends[2].Kind)
			sessionAfter, err = db.GetSession(t.Context(), "default", event.SessionName)
			require.NoError(t, err)
			require.Len(t, sessionAfter.Messages, 2)
			require.NotEmpty(t, sessionBefore.ActiveTaskUID)
			require.Empty(t, sessionAfter.ActiveTaskUID)
			require.NoError(t, s.Client.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Equal(t, sends[2].DeliveryID, task.Annotations[TaskGatewayDelivery])
			require.Empty(t, task.Spec.Prompt)
		})
	}
}

func TestGatewayTaskMessageRejectsIneligibleLiveIdentity(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *Service, *corev1alpha1.Task)
		code   int
	}{
		{"pending", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhasePending
			require.NoError(t, s.Client.Status().Update(t.Context(), task))
		}, 409},
		{"finalizing", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhaseFinalizing
			require.NoError(t, s.Client.Status().Update(t.Context(), task))
		}, 409},
		{"terminal", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			require.NoError(t, s.Client.Status().Update(t.Context(), task))
		}, 409},
		{"outcome recorded", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
			require.NoError(t, s.Client.Status().Update(t.Context(), task))
		}, 409},
		{"deleting", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Finalizers = []string{"test.orka.ai/hold"}
			require.NoError(t, s.Client.Update(t.Context(), task))
			require.NoError(t, s.Client.Delete(t.Context(), task))
		}, 409},
		{"forged provenance", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Spec.RequestedBy.Issuer = "forged"
			require.NoError(t, s.Client.Update(t.Context(), task))
		}, 403},
		{"non gateway", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Name = "ordinary-task"
			task.ResourceVersion = ""
			task.UID = "ordinary-uid"
			require.NoError(t, s.Client.Create(t.Context(), task))
		}, 403},
		{"wrong task UID", func(t *testing.T, s *Service, task *corev1alpha1.Task) { task.UID = "forged-uid" }, 403},
		{"namespace replacement", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			ns := &corev1.Namespace{}
			require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Name: "default"}, ns))
			ns.UID = "replacement"
			require.NoError(t, s.Client.Update(t.Context(), ns))
		}, 409},
		{"gateway replacement", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.UID = "replacement" })
		}, 409},
		{"gateway generation", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Generation++ })
		}, 409},
		{"unready", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.Ready = false })
		}, 503},
		{"stale capabilities", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedGeneration = 0 })
		}, 503},
		{"no capabilities", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities = nil })
		}, 409},
		{"capability false", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false })
		}, 409},
		{"wrong contract", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities.ContractVersion = "other" })
		}, 409},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, db, _, _, task := newGatewayMessageFixture(t)
			tt.change(t, s, task)
			_, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "private content")
			var httpErr *HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, tt.code, httpErr.Code)
			require.NotContains(t, err.Error(), "private content")
			rows, err := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestGatewayTaskMessageReceiptRejectsChangedLiveIdentity(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*testing.T, *Service, *corev1alpha1.Task)
		code   int
	}{
		{"Task replaced", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.UID = "replacement"
			require.NoError(t, s.Client.Update(t.Context(), task))
		}, 403},
		{"namespace replaced", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			ns := &corev1.Namespace{}
			require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Name: "default"}, ns))
			ns.UID = "replacement"
			require.NoError(t, s.Client.Update(t.Context(), ns))
		}, 409},
		{"namespace deleting", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			ns := &corev1.Namespace{}
			require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Name: "default"}, ns))
			ns.Finalizers = []string{"test.orka.ai/hold"}
			require.NoError(t, s.Client.Update(t.Context(), ns))
			require.NoError(t, s.Client.Delete(t.Context(), ns))
		}, 409},
		{"Gateway replaced", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.UID = "replacement" })
		}, 409},
		{"Gateway generation changed", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Generation++ })
		}, 409},
		{"Gateway deleting", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Finalizers = []string{"test.orka.ai/hold"} })
			g := &gatewayv1alpha1.Gateway{}
			require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "chat"}, g))
			require.NoError(t, s.Client.Delete(t.Context(), g))
		}, 409},
		{"execution outcome", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
			require.NoError(t, s.Client.Status().Update(t.Context(), task))
		}, 409},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, db, adapter, _, task := newGatewayMessageFixture(t)
			taskUID := string(task.UID)
			receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, taskUID, "request", "update")
			require.NoError(t, err)
			before, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
			require.NoError(t, err)
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.Ready = false })
			tt.change(t, s, task)
			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, taskUID, "request", "update")
			var httpErr *HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, tt.code, httpErr.Code)
			rows, err := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: task.Namespace})
			require.NoError(t, err)
			require.Equal(t, []store.GatewayDelivery{*before}, rows)
			require.Empty(t, adapter.Deliveries())
		})
	}
}

func TestGatewayTaskMessageReceiptRejectsIdentityReadFailure(t *testing.T) {
	for _, kind := range []string{"Task", "Namespace", "Gateway"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, _, task := newGatewayMessageFixture(t)
			_, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update")
			require.NoError(t, err)
			s.APIReader = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if (kind == "Task" && key.Name == task.Name) || (kind == "Namespace" && key.Name == "default") || (kind == "Gateway" && key.Name == "chat") {
					return fmt.Errorf("read unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update")
			var httpErr *HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, http.StatusServiceUnavailable, httpErr.Code)
			require.Equal(t, "gateway message identity is unavailable", httpErr.Message)
		})
	}
}

func TestGatewayTaskMessageContentAndControllerLimit(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		valid         bool
	}{
		{"bounded", strings.Repeat("é", 8192), true},
		{"oversize", strings.Repeat("x", 16385), false},
		{"oversize before trim", strings.Repeat(" ", 16384) + "x", false},
		{"invalid UTF8", string([]byte{'x', 0xff}), false},
		{"empty", " \t\x00\x01 ", false},
		{"sanitize", "  update\x00\nAuthorization: Bearer example-secret  ", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, db, _, _, task := newGatewayMessageFixture(t)
			receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", tt.content)
			if !tt.valid {
				require.Error(t, err)
				rows, listErr := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
				require.NoError(t, listErr)
				require.Empty(t, rows)
				return
			}
			require.NoError(t, err)
			row, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
			require.NoError(t, err)
			require.NotContains(t, row.Text, "example-secret")
			require.NotContains(t, row.Text, "\x00")
		})
	}
	for _, limit := range []int{0, 3} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			s, _, _, _, task := newGatewayMessageFixture(t)
			s.Config = normalizeConfig(Config{Enabled: true, InterimMessagesPerTask: limit})
			want := limit
			if want == 0 {
				want = 10
			}
			for i := range want {
				_, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), fmt.Sprint(i), "update")
				require.NoError(t, err)
			}
			replay, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "0", "update")
			require.NoError(t, err)
			require.False(t, replay.Created)
			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "extra", "update")
			var httpErr *HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, http.StatusTooManyRequests, httpErr.Code)
			_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "0", "changed")
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, http.StatusConflict, httpErr.Code)
		})
	}
}

func TestGatewayTaskMessageIdempotencyUsesSanitizedContent(t *testing.T) {
	s, db, adapter, _, task := newGatewayMessageFixture(t)
	s.Config.InterimMessagesPerTask = 2
	receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "  update\x00  ")
	require.NoError(t, err)
	require.True(t, receipt.Created)
	replay, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update\x01")
	require.NoError(t, err)
	require.False(t, replay.Created)
	require.Equal(t, receipt.DeliveryID, replay.DeliveryID)
	require.Equal(t, receipt.Status, replay.Status)

	_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "changed update")
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusConflict, httpErr.Code)
	row, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
	require.NoError(t, err)
	require.Equal(t, "update", row.Text, "only sanitized content is retained")

	second, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "second", "second update")
	require.NoError(t, err, "normalization-equivalent replay must leave the second quota slot available")
	require.True(t, second.Created)
	_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "third", "third update")
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusTooManyRequests, httpErr.Code)
	rows, err := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	require.NoError(t, s.DeliverOnce(t.Context()))
	replay, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update\x01")
	require.NoError(t, err, "normalization-equivalent replay must still succeed at quota")
	require.False(t, replay.Created)
	require.Equal(t, receipt.DeliveryID, replay.DeliveryID)
	require.Equal(t, store.GatewayDeliveryDelivered, replay.Status)
	sends := adapter.Deliveries()
	require.Len(t, sends, 1)
	require.Equal(t, receipt.DeliveryID, sends[0].DeliveryID)
	require.Equal(t, "update", sends[0].Text)
}

func gatewayMessageCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, counter.Write(&metric))
	return metric.GetCounter().GetValue()
}

func TestGatewayTaskMessagePermanentAbandonmentImmediatelyBeforePOST(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*testing.T, *Service, *corev1alpha1.Task)
	}{
		{"capability withdrawn", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false })
		}},
		{"Task deleting", func(t *testing.T, s *Service, task *corev1alpha1.Task) {
			task.Finalizers = []string{"test.orka.ai/hold"}
			require.NoError(t, s.Client.Update(t.Context(), task))
			require.NoError(t, s.Client.Delete(t.Context(), task))
		}},
		{"Gateway identity changed", func(t *testing.T, s *Service, _ *corev1alpha1.Task) {
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.UID = "replacement" })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, db, adapter, _, task := newGatewayMessageFixture(t)
			receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update")
			require.NoError(t, err)
			// Change after the initial Gateway read, while the outbound Secret is resolved.
			s.APIReader = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if _, ok := obj.(*corev1.Secret); ok && key.Name == "outbound" {
					tt.change(t, s, task)
				}
				return err
			}})
			outcomes := gatewayMessageCounterValue(t, gatewayDeliveryTotal.WithLabelValues("non_retryable_error"))
			deadLetters := gatewayMessageCounterValue(t, gatewayDeadLettersTotal.WithLabelValues("delivery"))
			require.NoError(t, s.DeliverOnce(t.Context()))
			require.Empty(t, adapter.Deliveries())
			row, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
			require.NoError(t, err)
			require.Equal(t, store.GatewayDeliveryDeadLettered, row.State)
			require.Equal(t, outcomes+1, gatewayMessageCounterValue(t, gatewayDeliveryTotal.WithLabelValues("non_retryable_error")))
			require.Equal(t, deadLetters+1, gatewayMessageCounterValue(t, gatewayDeadLettersTotal.WithLabelValues("delivery")))
		})
	}
}

func TestGatewayTaskMessageTimingAfterRevalidation(t *testing.T) {
	const requestTimeout = 250 * time.Millisecond
	for _, tt := range []struct {
		name      string
		expiry    time.Duration
		wantState store.GatewayDeliveryState
		wantPOSTs int
	}{
		{"adapter timeout", time.Hour, store.GatewayDeliveryDelivered, 1},
		{"delivery expiry", requestTimeout, store.GatewayDeliveryExpired, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, db, adapter, _, task := newGatewayMessageFixture(t)
			s.Config.DeliveryTimeout = requestTimeout
			s.Config.DeliveryMaxAttempts = 1
			s.Config.EventExpiry = tt.expiry
			receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "progress", "working")
			require.NoError(t, err)
			gatewayReads := 0
			s.APIReader = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*gatewayv1alpha1.Gateway); ok {
					gatewayReads++
					if gatewayReads == 2 {
						// The successful final eligibility read outlasts the adapter timeout.
						time.Sleep(2 * requestTimeout)
					}
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			require.NoError(t, s.DeliverOnce(t.Context()))
			require.Equal(t, 2, gatewayReads)
			require.Equal(t, tt.wantPOSTs, adapter.Attempts(receipt.DeliveryID))
			row, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
			require.NoError(t, err)
			require.Equal(t, tt.wantState, row.State)
			require.Equal(t, 1, row.AttemptCount)
		})
	}
}

func TestGatewayTaskMessageRetriesReadinessLossImmediatelyBeforePOST(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*gatewayv1alpha1.Gateway)
	}{
		{"unready", func(g *gatewayv1alpha1.Gateway) { g.Status.Ready = false }},
		{"stale observation", func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedGeneration = 0 }},
	} {
		for _, phase := range []corev1alpha1.TaskPhase{corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed} {
			t.Run(tt.name+"/"+string(phase), func(t *testing.T) {
				s, db, adapter, event, task := newGatewayMessageFixture(t)
				receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update")
				require.NoError(t, err)
				// The first Gateway read is ready; only the last pre-POST check
				// sees readiness lost while resolving the outbound Secret.
				s.APIReader = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := c.Get(ctx, key, obj, opts...)
					if _, ok := obj.(*corev1.Secret); ok && key.Name == "outbound" {
						updateMessageGateway(t, s, tt.change)
					}
					return err
				}})
				outcomes := gatewayMessageCounterValue(t, gatewayDeliveryTotal.WithLabelValues("non_retryable_error"))
				deadLetters := gatewayMessageCounterValue(t, gatewayDeadLettersTotal.WithLabelValues("delivery"))
				require.NoError(t, s.DeliverOnce(t.Context()))
				require.Equal(t, outcomes, gatewayMessageCounterValue(t, gatewayDeliveryTotal.WithLabelValues("non_retryable_error")))
				require.Equal(t, deadLetters, gatewayMessageCounterValue(t, gatewayDeadLettersTotal.WithLabelValues("delivery")))
				require.Empty(t, adapter.Deliveries())
				row, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
				require.NoError(t, err)
				require.Equal(t, store.GatewayDeliveryRetryScheduled, row.State)
				require.Equal(t, receipt.DeliveryID, row.IdempotencyID)
				require.Equal(t, 1, row.AttemptCount)

				s.APIReader = s.Client
				updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
					g.Status.Ready = true
					g.Status.ObservedGeneration = g.Generation
				})
				task.Status.Phase = phase
				require.NoError(t, db.SaveResult(t.Context(), task.Namespace, task.Name, []byte("final response")))
				require.NoError(t, s.Client.Status().Update(t.Context(), task))
				require.NoError(t, s.ProjectTerminals(t.Context()))
				projected, err := db.GetGatewayEvent(t.Context(), event.Namespace, event.ID)
				require.NoError(t, err)
				terminal, err := db.GetGatewayDelivery(t.Context(), event.Namespace, projected.DeliveryID)
				require.NoError(t, err)
				require.Equal(t, store.GatewayDeliveryPending, terminal.State)
				_, err = db.ClaimNextGatewayDelivery(t.Context(), task.Namespace, s.Owner, row.NextAttemptAt.Add(-time.Nanosecond), s.Config.ClaimLease)
				require.ErrorIs(t, err, store.ErrNotFound, "terminal must not overtake the retrying message")

				require.EventuallyWithT(t, func(collect *assert.CollectT) {
					err := s.DeliverOnce(t.Context())
					if err != nil {
						require.ErrorIs(collect, err, store.ErrNotFound)
					}
					current, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
					require.NoError(collect, err)
					require.Equal(collect, store.GatewayDeliveryDelivered, current.State)
				}, 5*time.Second, 10*time.Millisecond)
				sends := adapter.Deliveries()
				require.Len(t, sends, 1)
				require.Equal(t, receipt.DeliveryID, sends[0].DeliveryID)
				require.Equal(t, protocol.DeliveryKindMessage, sends[0].Kind)
				require.NoError(t, s.DeliverOnce(t.Context()))
				sends = adapter.Deliveries()
				require.Len(t, sends, 2)
				wantKind := protocol.DeliveryKindFinal
				if phase == corev1alpha1.TaskPhaseFailed {
					wantKind = protocol.DeliveryKindError
				}
				require.Equal(t, wantKind, sends[1].Kind)
				require.Equal(t, terminal.ID, sends[1].DeliveryID)
			})
		}
	}
}

func TestGatewayTaskMessageReceiptSurvivesAdmissionGate(t *testing.T) {
	for _, tt := range []struct {
		name        string
		change      func(*gatewayv1alpha1.Gateway)
		code        int
		message     string
		unsupported bool
	}{
		{"unready", func(g *gatewayv1alpha1.Gateway) { g.Status.Ready = false }, 503, "gateway is not ready for interim delivery", false},
		{"stale observation", func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedGeneration = 0 }, 503, "gateway is not ready for interim delivery", false},
		{"no capabilities", func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities = nil }, 409, ErrInterimDeliveryUnsupported.Message, true},
		{"capability withdrawn", func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false }, 409, ErrInterimDeliveryUnsupported.Message, true},
		{"wrong contract", func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities.ContractVersion = "other" }, 409, "gateway does not currently support interim delivery", false},
	} {
		for _, state := range []store.GatewayDeliveryState{store.GatewayDeliveryPending, store.GatewayDeliveryDelivered, store.GatewayDeliveryDeadLettered} {
			t.Run(tt.name+"/"+string(state), func(t *testing.T) {
				s, db, adapter, _, task := newGatewayMessageFixture(t)
				s.Config.InterimMessagesPerTask = 1
				receipt, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "  update\x00  ")
				require.NoError(t, err)
				if state != store.GatewayDeliveryPending {
					_, err := db.ClaimNextGatewayDelivery(t.Context(), task.Namespace, s.Owner, time.Now().UTC(), time.Minute)
					require.NoError(t, err)
					if state == store.GatewayDeliveryDelivered {
						require.NoError(t, db.MarkGatewayDeliveryDelivered(t.Context(), task.Namespace, receipt.DeliveryID, s.Owner, "provider-receipt", time.Now().UTC()))
					} else {
						require.NoError(t, db.MarkGatewayDeliveryTerminal(t.Context(), task.Namespace, receipt.DeliveryID, s.Owner, state, "abandoned", time.Now().UTC()))
					}
				}
				before, err := db.GetGatewayDelivery(t.Context(), task.Namespace, receipt.DeliveryID)
				require.NoError(t, err)
				updateMessageGateway(t, s, tt.change)
				replay, err := s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update\x01")
				require.NoError(t, err)
				require.Equal(t, &TaskMessageReceipt{DeliveryID: receipt.DeliveryID, Status: state, Created: false}, replay)

				_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "changed update")
				var httpErr *HTTPError
				require.ErrorAs(t, err, &httpErr)
				require.Equal(t, http.StatusConflict, httpErr.Code)
				require.Equal(t, "requestID was already used for different content", httpErr.Message)
				require.NotErrorIs(t, err, ErrInterimDeliveryUnsupported)

				_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "new", "update")
				require.ErrorAs(t, err, &httpErr)
				require.Equal(t, tt.code, httpErr.Code)
				require.Equal(t, tt.message, httpErr.Message)
				if tt.unsupported {
					require.ErrorIs(t, err, ErrInterimDeliveryUnsupported)
				}
				rows, err := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: task.Namespace})
				require.NoError(t, err)
				require.Equal(t, []store.GatewayDelivery{*before}, rows, "receipt lookup must not mutate or insert any row")
				require.Empty(t, adapter.Deliveries())
			})
		}
	}
}

func TestGatewayTaskMessagePreparedAdmissionSerializesTerminalCutoff(t *testing.T) {
	s, db, _, event, task := newGatewayMessageFixture(t)
	prepared, err := s.PrepareTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "request", "update")
	require.NoError(t, err)
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	require.NoError(t, s.Client.Status().Update(t.Context(), task))
	require.NoError(t, s.ProjectTerminals(t.Context()))
	err = db.WithTaskDataTransaction(t.Context(), func(ctx context.Context) error {
		_, err := s.EnqueuePreparedTaskMessage(ctx, prepared)
		return err
	})
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, 409, httpErr.Code)
	rows, err := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: event.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "error", rows[0].Kind)
}
