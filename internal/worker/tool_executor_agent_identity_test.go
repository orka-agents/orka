/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

// An Agent identity credential is bound to the Tool's declared destination,
// so even a same-origin redirect is returned unfollowed, as for a person's
// linked-account credential.
func TestToolExecutorAgentIdentityRequestNeverFollowsRedirects(t *testing.T) {
	followed := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	resolver := &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter: outboundaccess.AdapterAgentIdentity, CredentialHeader: "X-Agent-Token", CredentialValue: "agent-token",
		SensitiveValues: []string{"agent-token"},
	}}
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: resolver, skipDirectPublicValidation: true}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "graph_me", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL: server.URL + "/me", Method: "GET", OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "agent"},
		}},
	}
	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("Execute() error = %v, want the unfollowed 307", err)
	}
	if followed {
		t.Fatal("the Agent identity credential followed a redirect")
	}
}
