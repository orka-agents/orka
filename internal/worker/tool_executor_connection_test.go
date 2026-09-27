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
