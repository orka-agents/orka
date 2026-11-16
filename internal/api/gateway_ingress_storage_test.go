package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
)

type uncertainGatewayAdmissionStore struct {
	store.GatewayEventStore
	commit    bool
	uncertain bool
}

func (s *uncertainGatewayAdmissionStore) AdmitGatewayEvent(ctx context.Context, admission store.GatewayEventAdmission) (*store.GatewayEvent, bool, error) {
	if !s.uncertain {
		return s.GatewayEventStore.AdmitGatewayEvent(ctx, admission)
	}
	s.uncertain = false
	if s.commit {
		if _, _, err := s.GatewayEventStore.AdmitGatewayEvent(ctx, admission); err != nil {
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("admission response lost: %w", store.ErrOutcomeUnknown)
}

func TestGatewayIngressRetriesUnknownAdmissionWithoutDuplicatingEvents(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			backing := newGatewayHandlerStore(t)
			admissions := &uncertainGatewayAdmissionStore{GatewayEventStore: backing, commit: committed, uncertain: true}
			service := newGatewayIngressStorageFixture(t, admissions, backing)
			h := NewHandlers(HandlersConfig{GatewayService: service})
			app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
			app.Post("/api/v1/gateways/:namespace/:name/events", h.HandleGatewayEvent)
			body, err := json.Marshal(protocol.EventEnvelope{
				ProtocolVersion: protocol.Version, ExternalEventID: "uncertain-event", EventType: protocol.EventTypeText,
				AccountID: "acct", ContextID: "room", Sender: protocol.Sender{ID: "user-1"}, Text: "hello", ReplyTarget: "room",
			})
			require.NoError(t, err)
			request := func() *http.Response {
				t.Helper()
				req := httptest.NewRequest(http.MethodPost, "/api/v1/gateways/default/chat/events", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer fixture-bearer")
				response, err := app.Test(req)
				require.NoError(t, err)
				return response
			}
			first := request()
			require.NoError(t, first.Body.Close())
			require.Equal(t, http.StatusServiceUnavailable, first.StatusCode)
			require.Equal(t, serviceUnavailableRetryAfterSeconds, first.Header.Get(fiber.HeaderRetryAfter))
			retry := request()
			defer retry.Body.Close() //nolint:errcheck
			require.Equal(t, http.StatusAccepted, retry.StatusCode)
			var acknowledged protocol.IngressResponse
			require.NoError(t, json.NewDecoder(retry.Body).Decode(&acknowledged))
			wantStatus := "accepted"
			if committed {
				wantStatus = "duplicate"
			}
			require.Equal(t, wantStatus, acknowledged.Status)
			records, err := backing.ListGatewayEvents(t.Context(), store.GatewayEventFilter{Namespace: "default"})
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, records[0].ID, acknowledged.EventID)
		})
	}
}

func newGatewayIngressStorageFixture(t *testing.T, events store.GatewayEventStore, backing interface {
	store.GatewayDeliveryStore
	store.ResultStore
}) *gatewayruntime.Service {
	t.Helper()
	kube := newGatewayIdentityClient(t, "namespace-uid", "gateway-uid")
	require.NoError(t, corev1alpha1.AddToScheme(kube.Scheme()))
	object := &gatewayv1alpha1.Gateway{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "chat"}, object))
	object.Generation = 1
	object.Spec = gatewayv1alpha1.GatewaySpec{
		GatewayClassName: "chat-class", Adapter: gatewayv1alpha1.GatewayAdapterLocation{Endpoint: "http://127.0.0.1:8081"},
		InboundAuthRef: gatewayv1alpha1.GatewayBearerAuthReference{Name: "inbound", Key: "token"},
	}
	object.Status = gatewayv1alpha1.GatewayStatus{Ready: true, ObservedGeneration: 1}
	require.NoError(t, kube.Update(t.Context(), object))
	for _, resource := range []client.Object{
		&gatewayv1alpha1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "chat-class", Generation: 1},
			Status:     gatewayv1alpha1.GatewayClassStatus{Accepted: true, ObservedGeneration: 1},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "inbound", Namespace: "default",
				Labels:      map[string]string{gatewayruntime.GatewayInboundAuthLabel: gatewayruntime.GatewayAuthEnabledValue},
				Annotations: map[string]string{gatewayruntime.GatewayAuthNameAnnotation: "chat"},
			},
			Data: map[string][]byte{"token": []byte("fixture-bearer")},
		},
		&gatewayv1alpha1.GatewayBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "room", Namespace: "default", UID: "binding-uid", Generation: 1},
			Spec: gatewayv1alpha1.GatewayBindingSpec{
				GatewayRef: gatewayv1alpha1.GatewayBindingReference{Name: "chat"},
				AgentRef:   gatewayv1alpha1.GatewayBindingReference{Name: "assistant"},
				Match:      gatewayv1alpha1.GatewayBindingMatch{AccountID: "acct", ContextID: "room"},
				SenderPolicy: gatewayv1alpha1.GatewaySenderPolicy{
					Mode: gatewayv1alpha1.GatewaySenderPolicyAllowlist, AllowedSenderIDs: []string{"user-1"},
				},
				Session: gatewayv1alpha1.GatewaySessionSpec{Mode: gatewayv1alpha1.GatewaySessionContext},
			},
			Status: gatewayv1alpha1.GatewayBindingStatus{Ready: true, ObservedGeneration: 1},
		},
		&corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "default", UID: "agent-uid"}},
	} {
		require.NoError(t, kube.Create(t.Context(), resource))
	}
	config := gatewayruntime.DefaultConfig()
	config.AllowInsecureLoopback = true
	return gatewayruntime.NewService(kube, events, backing, backing, config)
}
