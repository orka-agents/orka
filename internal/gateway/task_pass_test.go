package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"context"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func delegatedEventBody(t *testing.T, id string) []byte {
	t.Helper()
	var event protocol.EventEnvelope
	require.NoError(t, json.Unmarshal(gatewayEventBody(t, id, "user-1"), &event))
	event.EventType = "delegatedText"
	event.Metadata = map[string]string{"gatewayGrantRef": "sender-generation-1"}
	body, err := json.Marshal(event)
	require.NoError(t, err)
	return body
}

func TestGatewayTaskGrantDurableOrigin(t *testing.T) {
	s, _, _ := newGatewayServiceFixture(t)
	class := &gatewayv1alpha1.GatewayClass{}
	require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Name: "generic-chat"}, class))
	class.Spec.AllowedMetadataKeys = append(class.Spec.AllowedMetadataKeys, "gatewayGrantRef")
	require.NoError(t, s.Client.Update(t.Context(), class))
	var task *corev1alpha1.Task
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		err := c.Create(ctx, obj, opts...)
		if candidate, ok := obj.(*corev1alpha1.Task); ok && err == nil {
			task = candidate.DeepCopy()
			grant, err := s.ResolveTaskGrant(ctx, task)
			require.Nil(t, grant)
			require.ErrorIs(t, err, store.ErrNotReady)
		}
		return err
	}})
	_, err := s.AdmitEvent(t.Context(), "default", "chat", "Bearer inbound-token", delegatedEventBody(t, "delegated"))
	require.NoError(t, err)
	binding := &gatewayv1alpha1.GatewayBinding{}
	require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "room"}, binding))
	binding.Spec.SenderPolicy.Mode = gatewayv1alpha1.GatewaySenderPolicyAll
	require.NoError(t, s.Client.Update(t.Context(), binding))
	secondBody := strings.ReplaceAll(string(delegatedEventBody(t, "sender-two")), "user-1", "user-2")
	secondBody = strings.ReplaceAll(secondBody, "sender-generation-1", "sender-generation-2")
	_, err = s.AdmitEvent(t.Context(), "default", "chat", "Bearer inbound-token", []byte(secondBody))
	require.NoError(t, err)
	require.NoError(t, s.DispatchOnce(t.Context()))
	require.NotNil(t, task)
	grant, err := s.ResolveTaskGrant(t.Context(), task)
	require.NoError(t, err)
	require.Equal(t, "sender-generation-1", grant.GrantRef)
	require.Equal(t, "gateway-uid", grant.GatewayUID)
	require.Equal(t, "namespace-uid", grant.NamespaceUID)
	child := task.DeepCopy()
	child.Labels["orka.ai/parent-task"] = task.Name
	grant, err = s.ResolveTaskGrant(t.Context(), child)
	require.NoError(t, err)
	require.Nil(t, grant)
	task.UID = "replacement"
	grant, err = s.ResolveTaskGrant(t.Context(), task)
	require.NoError(t, err)
	require.Nil(t, grant)
}

func TestTaskPassCredentialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"gateway-uid":{"endpoint":"https://gateway.example","token":"fixture-credential"}}`), 0600))
	credentials, err := LoadTaskPassCredentials(path)
	require.NoError(t, err)
	require.Equal(t, TaskPassCredential{Endpoint: "https://gateway.example", Token: "fixture-credential"}, credentials["gateway-uid"])
	for _, input := range []string{`{}`, `{"gateway-uid":""}`, `{"gateway-uid":{"endpoint":"http://gateway.example","token":"fixture"}}`, `{"gateway-uid":{"endpoint":"https://gateway.example/path","token":"fixture"}}`, `{"gateway-uid":{"endpoint":"https://gateway.example","token":"bad\nheader"}}`, `not-json`} {
		require.NoError(t, os.WriteFile(path, []byte(input), 0600))
		_, err := LoadTaskPassCredentials(path)
		require.Error(t, err)
		require.NotContains(t, err.Error(), input)
	}
}

func TestDelegatedTextRefusesNativeAdmission(t *testing.T) {
	s, _, _ := newGatewayServiceFixture(t)
	setGatewayAgentNativeAI(t, s, true)
	class := &gatewayv1alpha1.GatewayClass{}
	require.NoError(t, s.Client.Get(t.Context(), client.ObjectKey{Name: "generic-chat"}, class))
	class.Spec.AllowedMetadataKeys = append(class.Spec.AllowedMetadataKeys, "gatewayGrantRef")
	require.NoError(t, s.Client.Update(t.Context(), class))
	admitted, err := s.AdmitEvent(t.Context(), "default", "chat", "Bearer inbound-token", delegatedEventBody(t, "native"))
	require.NoError(t, err)
	require.Equal(t, "rejected", admitted.Status)
}
