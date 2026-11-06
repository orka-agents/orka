package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

func TestSubstrateMCPReadinessTreatsActorAuthenticationAsReachable(t *testing.T) {
	for status, reachable := range map[int]bool{
		http.StatusOK:                  true,
		http.StatusMethodNotAllowed:    true,
		http.StatusUnauthorized:        true, // Actor protected by spec.http.authSecretRef
		http.StatusForbidden:           true,
		http.StatusNotFound:            false,
		http.StatusBadGateway:          false,
		http.StatusInternalServerError: false,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var target string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target = r.Header.Get("ate-target-actor")
				w.WriteHeader(status)
			}))
			defer server.Close()
			router, err := workerexecutor.NewSubstrateMCPRouter(server.URL, server.Client())
			require.NoError(t, err)
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "lookup", Namespace: "tools"},
				Spec:       corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{}}},
				Status: corev1alpha1.ToolStatus{Actor: &corev1alpha1.ToolActorStatus{
					Provider: corev1alpha1.WorkspaceProviderSubstrate, ActorID: "actor-1.tools",
				}},
			}
			err = (&ToolReconciler{}).healthCheckMCPActorEndpoint(t.Context(), router, tool)
			require.Equal(t, reachable, err == nil, "status %d: %v", status, err)
			require.Equal(t, "tools/actor-1", target, "the probe is routed by Actor target")
		})
	}
}
