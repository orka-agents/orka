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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestToolExecutorConnectionOutboundAccessInjectsCredential(t *testing.T) {
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		http.Error(w, "debug gho_person_token", http.StatusBadGateway)
	}))
	defer server.Close()

	resolver := &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter:          outboundaccess.AdapterConnection,
		CredentialHeader: "Authorization",
		CredentialValue:  "Bearer gho_person_token",
		SensitiveValues:  []string{"gho_person_token"},
		ConnectionUID:    "conn-uid",
	}}
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: resolver, skipDirectPublicValidation: true}
	executor.SetRequester(&corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"})
	executor.SetFrozenConnections(map[string]outboundaccess.FrozenConnection{"github-conn": {UID: "conn-uid", Generation: 3}})
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gh_search", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     server.URL,
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "github-conn"},
		}},
	}
	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"q":"orka"}`))
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	if strings.Contains(err.Error(), "gho_person_token") {
		t.Fatalf("Execute() leaked the person's token: %v", err)
	}
	if authorization != "Bearer gho_person_token" {
		t.Fatalf("Authorization = %q", authorization)
	}
	if resolver.request.Requester == nil || resolver.request.Requester.Subject != "alice" {
		t.Fatalf("resolver request requester = %+v", resolver.request.Requester)
	}
	if got := resolver.request.FrozenConnections["github-conn"]; got.UID != "conn-uid" || got.Generation != 3 {
		t.Fatalf("resolver request frozen = %+v", resolver.request.FrozenConnections)
	}
	if executor.Requester().Subject != "alice" || executor.FrozenConnections()["github-conn"].UID != "conn-uid" {
		t.Fatal("accessors must return the bound values")
	}
	if (&ToolExecutor{}).Requester() != nil || (&ToolExecutor{}).FrozenConnections() != nil {
		t.Fatal("unbound executor must report nothing")
	}
}

func TestToolExecutorConnectionOutboundAccessRequiresHTTPS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	resolver := &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter: outboundaccess.AdapterConnection, CredentialHeader: "Authorization", CredentialValue: "Bearer x",
	}}
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: resolver}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     server.URL,
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "github-conn"},
		}},
	}
	if _, err := executor.Execute(context.Background(), tool, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("Execute() error = %v, want HTTPS rejection", err)
	}
}

func TestToolExecutorCredentialRequestRefusesCrossOriginRedirect(t *testing.T) {
	leaked := false
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("X-Github-Token") != "" || r.Header.Get("Authorization") != ""
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/collect", http.StatusFound)
	}))
	defer provider.Close()
	resolver := &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter: outboundaccess.AdapterConnection, CredentialHeader: "X-Github-Token", CredentialValue: "token gho_person",
	}}
	executor := &ToolExecutor{client: provider.Client(), namespace: "tenant", outboundResolver: resolver, skipDirectPublicValidation: true}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gh_search", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL: provider.URL, Method: "GET", OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "github-conn"},
		}},
	}
	// The shared redirect policy stops at the origin boundary and hands the
	// 302 back as the tool result instead of following it with the header.
	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("Execute() error = %v, want the unfollowed 302", err)
	}
	if leaked {
		t.Fatal("the credential header reached the redirect target")
	}
}

func TestToolExecutorConnectionOutboundAccessValidatesArgumentsBeforeInjecting(t *testing.T) {
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	resolver := &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter: outboundaccess.AdapterConnection, CredentialHeader: "Authorization", CredentialValue: "Bearer gho_person_token",
		SensitiveValues: []string{"gho_person_token"}, ConnectionUID: "conn-uid",
		Parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"},"limit":{"type":"integer","maximum":10}},"additionalProperties":false}`)},
	}}
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: resolver, skipDirectPublicValidation: true}
	executor.SetRequester(&corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"})
	executor.SetFrozenConnections(map[string]outboundaccess.FrozenConnection{"github-conn": {UID: "conn-uid", Generation: 3}})
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gh_search", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL: server.URL, OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "github-conn"},
		}},
	}
	// Arguments outside the provider's curated schema never leave with the
	// person's credential, whatever the caller supplied.
	for _, args := range []string{`{"limit":3}`, `{"q":"x","limit":11}`, `{"q":"x","extra":true}`} {
		if _, err := executor.Execute(context.Background(), tool, json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), "arguments rejected") || ToolRequestWasAttempted(err) {
			t.Fatalf("%s: err = %v, want refusal before any request", args, err)
		}
	}
	if requests != 0 {
		t.Fatalf("provider received %d requests for rejected arguments", requests)
	}
	if _, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"q":"x","limit":3}`)); err != nil {
		t.Fatalf("admitted arguments: %v", err)
	}
	if requests != 1 {
		t.Fatalf("provider received %d requests, want one", requests)
	}
}

func TestToolExecutorPrivateConnectionEndpointsKeepTransportHardening(t *testing.T) {
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	resolver := &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter:          outboundaccess.AdapterConnection,
		CredentialHeader: "Authorization",
		CredentialValue:  "Bearer gho_person_token",
		SensitiveValues:  []string{"gho_person_token"},
		ConnectionUID:    "conn-uid",
	}}
	// A Pod-wide proxy setting must not carry a linked-account bearer
	// anywhere: the hardened transport ignores it even under the allowance.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("https_proxy", "http://127.0.0.1:9")
	base := server.Client()
	transport := base.Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	client := &http.Client{Transport: transport}

	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "items", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     server.URL,
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "as-me"},
		}},
	}
	newExecutor := func() *ToolExecutor {
		executor := &ToolExecutor{client: client, namespace: "tenant", outboundResolver: resolver}
		executor.SetRequester(&corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"})
		executor.SetFrozenConnections(map[string]outboundaccess.FrozenConnection{"as-me": {UID: "conn-uid", Generation: 3}})
		return executor
	}

	// Without the allowance the loopback server is refused outright.
	SetAllowPrivateConnectionEndpoints(false)
	if _, err := newExecutor().Execute(context.Background(), tool, json.RawMessage(`{}`)); err == nil {
		t.Fatal("a private endpoint must be refused without the allowance")
	}
	if authorization != "" {
		t.Fatalf("the bearer reached the endpoint without the allowance: %q", authorization)
	}

	SetAllowPrivateConnectionEndpoints(true)
	t.Cleanup(func() { SetAllowPrivateConnectionEndpoints(false) })
	if _, err := newExecutor().Execute(context.Background(), tool, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("under the allowance the private endpoint is dialed directly, never through the proxy: %v", err)
	}
	if authorization != "Bearer gho_person_token" {
		t.Fatalf("Authorization = %q", authorization)
	}
	if transport.Proxy == nil {
		t.Fatal("the executor must clone the transport rather than strip the caller's proxy")
	}
}
