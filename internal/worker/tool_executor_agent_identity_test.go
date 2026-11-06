/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"context"
	"encoding/json"
	"errors"
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

// The authenticated Task reaches resolution on its own, so identity use
// records name it even for a Task with no identity binding.
func TestToolExecutorPassesTaskUIDToResolution(t *testing.T) {
	resolver := &fakeOutboundAccessResolver{err: errors.New("refused")}
	executor := &ToolExecutor{namespace: "tenant", outboundResolver: resolver, skipDirectPublicValidation: true}
	executor.SetTaskUID("task-uid")
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "graph_me", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL: "https://graph.example.test/me", Method: "GET", OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "agent"},
		}},
	}
	if _, err := executor.Execute(context.Background(), tool, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute() = nil error, want the refusal")
	}
	if resolver.request.TaskUID != "task-uid" || resolver.request.AgentIdentity != nil {
		t.Fatalf("resolve request TaskUID = %q AgentIdentity = %+v", resolver.request.TaskUID, resolver.request.AgentIdentity)
	}
}
