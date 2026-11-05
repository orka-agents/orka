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
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

func forwardingTestTool(url string) *corev1alpha1.Tool {
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-arm", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Azure Resource Manager",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP: &corev1alpha1.HTTPExecution{
				URL:                     url,
				Method:                  http.MethodPost,
				OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "azure"},
				Forward:                 &corev1alpha1.HTTPForward{Protocol: corev1alpha1.HTTPForwardProtocolAzureResourceManager},
			},
		},
	}
}

func agentIdentityResolver() *fakeOutboundAccessResolver {
	return &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter: outboundaccess.AdapterAgentIdentity, CredentialHeader: "Authorization",
		CredentialValue: "Bearer arm-token-secret", SensitiveValues: []string{"arm-token-secret"},
	}}
}

func TestForwardingToolSendsTheReadWithTheCredentialAndReturnsAnyStatus(t *testing.T) {
	var got *http.Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=leak")
		w.Header().Set("X-Ms-Request-Id", "req-1")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"ResourceGroupNotFound","message":"echo arm-token-secret"}}`))
	}))
	defer server.Close()
	resolver := agentIdentityResolver()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: resolver, skipDirectPublicValidation: true}
	executor.AllowForwarding()

	result, err := executor.Execute(context.Background(), forwardingTestTool(server.URL),
		json.RawMessage(`{"method":"GET","path":"/subscriptions/s1/resourcegroups/rg1","query":"api-version=2024-11-01"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got == nil || got.Method != http.MethodGet || got.URL.Path != "/subscriptions/s1/resourcegroups/rg1" ||
		got.URL.RawQuery != "api-version=2024-11-01" || got.Header.Get("Authorization") != "Bearer arm-token-secret" {
		t.Fatalf("upstream request = %#v", got)
	}
	if resolver.request.Tool.Forward != corev1alpha1.HTTPForwardProtocolAzureResourceManager || resolver.request.Tool.Method != http.MethodGet {
		t.Fatalf("resolver tool binding = %#v", resolver.request.Tool)
	}
	response, err := harnessv2.DecodeMCPForwardResponse(json.RawMessage(result))
	if err != nil {
		t.Fatalf("result %q: %v", result, err)
	}
	if response.Status != http.StatusNotFound || response.Headers["X-Ms-Request-Id"] != "req-1" || response.Headers["Set-Cookie"] != "" {
		t.Fatalf("response = %#v", response)
	}
	if strings.Contains(response.Body, "arm-token-secret") || !strings.Contains(response.Body, "ResourceGroupNotFound") {
		t.Fatalf("body = %q", response.Body)
	}
}

func TestForwardingToolDoesNotFollowRedirects(t *testing.T) {
	leaked := false
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
	}))
	defer sink.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/collect", http.StatusFound)
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: agentIdentityResolver(), skipDirectPublicValidation: true}
	executor.AllowForwarding()
	result, err := executor.Execute(context.Background(), forwardingTestTool(server.URL), json.RawMessage(`{"method":"GET","path":"/subscriptions"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	response, err := harnessv2.DecodeMCPForwardResponse(json.RawMessage(result))
	if err != nil || response.Status != http.StatusFound || leaked {
		t.Fatalf("response = %#v, err = %v, leaked = %v", response, err, leaked)
	}
}

func TestForwardingToolRefusals(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	for _, tc := range []struct {
		name      string
		forward   bool
		resolver  *fakeOutboundAccessResolver
		arguments string
		want      string
	}{
		{"outside the ACP broker", false, agentIdentityResolver(), `{"method":"GET","path":"/subscriptions"}`, "runs only through an ACP runtime"},
		{"write", true, agentIdentityResolver(), `{"method":"PUT","path":"/subscriptions/s1/resourcegroups/rg"}`, "only GET and HEAD"},
		{"dot segment", true, agentIdentityResolver(), `{"method":"GET","path":"/subscriptions/../tenants"}`, "dot segments"},
		{"another origin", true, agentIdentityResolver(), `{"method":"GET","path":"//evil.example/x"}`, "absolute"},
		{"extra argument", true, agentIdentityResolver(), `{"method":"GET","path":"/subscriptions","body":"x"}`, "forwarding arguments"},
		{"gateway policy", true, &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{Adapter: outboundaccess.AdapterGateway}},
			`{"method":"GET","path":"/subscriptions"}`, "not a gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: tc.resolver, skipDirectPublicValidation: true}
			if tc.forward {
				executor.AllowForwarding()
			}
			_, err := executor.Execute(context.Background(), forwardingTestTool(server.URL), json.RawMessage(tc.arguments))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute() error = %v, want %q", err, tc.want)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("refused calls sent %d requests", requests)
	}
}
