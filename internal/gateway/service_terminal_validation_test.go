package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
)

func TestSucceededGatewayTaskInvalidUTF8ResultSettlesBeforeDeliveryValidation(t *testing.T) {
	for _, afterDeadline := range []bool{false, true} {
		name := "before event deadline"
		if afterDeadline {
			name = "after event deadline"
		}
		t.Run(name, func(t *testing.T) {
			service, sqliteStore, adapter := newGatewayServiceFixture(t)
			ctx := context.Background()
			accepted, err := service.AdmitEvent(ctx, "default", "chat", "Bearer inbound-token", gatewayEventBody(t, "invalid-result", "user-1"))
			require.NoError(t, err)
			require.NoError(t, service.DispatchOnce(ctx))
			event, err := sqliteStore.GetGatewayEvent(ctx, "default", accepted.EventID)
			require.NoError(t, err)
			task := &corev1alpha1.Task{}
			require.NoError(t, service.Client.Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.TaskName}, task))
			session, err := sqliteStore.GetSession(ctx, event.Namespace, event.SessionName)
			require.NoError(t, err)
			require.Equal(t, task.Name, session.ActiveTask)
			require.Equal(t, string(task.UID), session.ActiveTaskUID)

			result := []byte{'o', 'k', 0xff}
			require.NoError(t, sqliteStore.SaveResult(ctx, task.Namespace, task.Name, result))
			task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
			require.NoError(t, service.Client.Status().Update(ctx, task))
			projectionTime := time.Now().UTC()
			if afterDeadline {
				projectionTime = event.ExpiresAt.Add(time.Second)
			}
			require.NoError(t, service.projectTerminalEvents(ctx, []store.GatewayEvent{*event}, projectionTime))

			completed, err := sqliteStore.GetGatewayEvent(ctx, event.Namespace, event.ID)
			require.NoError(t, err)
			require.Equal(t, store.GatewayEventCompleted, completed.State)
			require.NotNil(t, completed.CompletedAt)
			require.NotEmpty(t, completed.DeliveryID)
			session, err = sqliteStore.GetSession(ctx, event.Namespace, event.SessionName)
			require.NoError(t, err)
			require.Empty(t, session.ActiveTask)
			require.Empty(t, session.ActiveTaskUID)
			require.Len(t, session.Messages, 2)
			require.Equal(t, string(result), session.Messages[1].Content)
			delivery, err := sqliteStore.GetGatewayDelivery(ctx, event.Namespace, completed.DeliveryID)
			require.NoError(t, err)
			require.Equal(t, protocol.DeliveryKindFinal, delivery.Kind)
			require.Equal(t, store.GatewayDeliveryPending, delivery.State)
			require.Equal(t, string(result), delivery.Text)

			requests := 0
			service.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, errors.New("unexpected adapter request")
			})}
			require.NoError(t, service.DeliverOnce(ctx))
			delivery, err = sqliteStore.GetGatewayDelivery(ctx, event.Namespace, completed.DeliveryID)
			require.NoError(t, err)
			require.Equal(t, store.GatewayDeliveryDeadLettered, delivery.State)
			require.Equal(t, "delivery validation failed", delivery.LastError)
			require.Zero(t, requests, "invalid terminal text must never reach the adapter")
			require.Empty(t, adapter.Deliveries())
		})
	}
}
