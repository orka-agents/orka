/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestValidateToolHTTPURLPrivateConnectorEndpoints(t *testing.T) {
	r := &ToolReconciler{}
	if err := r.validateToolHTTPURL("https://10.96.0.10:8443/api/items", false); err == nil || !strings.Contains(err.Error(), "private/loopback") {
		t.Fatalf("without the allowance a private endpoint must be refused: %v", err)
	}
	if err := r.validateToolHTTPURL("https://10.96.0.10:8443/api/items", true); err != nil {
		t.Fatalf("with the allowance a private endpoint is accepted: %v", err)
	}
	if err := r.validateToolHTTPURL("https://127.0.0.1:8443/api/items", true); err != nil {
		t.Fatalf("loopback under the allowance: %v", err)
	}
	// The fixed metadata and API server hosts stay blocked under the allowance.
	for _, url := range []string{
		"https://169.254.169.254/latest", "https://kubernetes.default.svc/api", "https://metadata.google.internal/",
		"https://KUBERNETES.DEFAULT.SVC/api", "https://kubernetes.default.svc./api", "https://Metadata.Google.Internal./",
		"https://kubernetes.default.svc.cluster.local/api", "https://[fd00:ec2::254]/latest", "https://metadata/computeMetadata/v1/",
	} {
		if err := r.validateToolHTTPURL(url, true); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("%s under the allowance: %v", url, err)
		}
	}
	// The rest of the URL rules are untouched by the allowance.
	if err := r.validateToolHTTPURL("https://user:pw@10.96.0.10/api", true); err == nil || !strings.Contains(err.Error(), "embedded credentials") {
		t.Fatalf("embedded credentials under the allowance: %v", err)
	}
}

func TestValidateToolRequiresHTTPSUnderThePrivateAllowance(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "as-me", Namespace: "tenant", Generation: 1},
		Spec:       corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "fixture"}}},
		Status: corev1alpha1.OutboundAccessPolicyStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{
			{Type: corev1alpha1.OutboundAccessPolicyConditionAccepted, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: 1},
			{Type: corev1alpha1.OutboundAccessPolicyConditionResolvedRefs, Status: metav1.ConditionTrue, Reason: "Resolved", ObservedGeneration: 1},
		}},
	}
	r := &ToolReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build(), AllowPrivateConnectorEndpoints: true}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "items", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{Description: "items", HTTP: &corev1alpha1.HTTPExecution{
			URL: "http://10.96.0.10/api/items", OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "as-me"},
		}},
	}
	if err := r.validateTool(context.Background(), tool); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain http under the allowance: %v", err)
	}
	tool.Spec.HTTP.URL = "https://10.96.0.10/api/items"
	if err := r.validateTool(context.Background(), tool); err != nil {
		t.Fatalf("https private endpoint under the allowance: %v", err)
	}
}

func TestPrivateEndpointHealthClientRefusesInfrastructureAndRedirects(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	client := privateEndpointHealthClient(0)
	for _, target := range []string{"https://169.254.169.254/latest", "https://10.96.0.1/api", "https://kubernetes.default.svc/api"} {
		resp, err := client.Head(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("%s: the probe must not reach an infrastructure address", target)
		}
		if !strings.Contains(err.Error(), "infrastructure") {
			t.Fatalf("%s: err = %v", target, err)
		}
	}
	// A redirect is reported as the response it is, never followed.
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect = %v", err)
	}
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("the probe transport must not use a proxy")
	}
}
