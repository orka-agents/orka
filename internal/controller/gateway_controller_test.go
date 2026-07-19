package controller

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/referenceadapter"
)

func TestGatewayReconcilerProbesReferenceAdapter(t *testing.T) {
	adapter := referenceadapter.New("outbound-token")
	server := httptest.NewServer(adapter.Handler())
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gatewayv1alpha1.AddToScheme(scheme)
	class := &gatewayv1alpha1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "generic-chat", Generation: 1},
		Spec: gatewayv1alpha1.GatewayClassSpec{
			ContractVersion: gatewayv1alpha1.ContractVersionV1,
			Category:        gatewayv1alpha1.GatewayCategoryChat,
			Capabilities: gatewayv1alpha1.GatewayCapabilities{
				InboundText: true, OutboundText: true, IdempotentDelivery: true,
			},
		},
		Status: gatewayv1alpha1.GatewayClassStatus{Accepted: true, ObservedGeneration: 1},
	}
	object := &gatewayv1alpha1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", Generation: 1},
		Spec: gatewayv1alpha1.GatewaySpec{
			GatewayClassName: "generic-chat",
			Adapter:          gatewayv1alpha1.GatewayAdapterLocation{Endpoint: server.URL},
			InboundAuthRef:   gatewayv1alpha1.GatewayBearerAuthReference{Name: "inbound", Key: "token"},
			OutboundAuthRef:  gatewayv1alpha1.GatewayBearerAuthReference{Name: "outbound", Key: "token"},
		},
	}
	inbound := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "inbound", Namespace: "default", Labels: map[string]string{
			gatewayruntime.GatewayInboundAuthLabel: gatewayruntime.GatewayAuthEnabledValue,
			gatewayruntime.GatewayAuthNameLabel:    "chat",
		}, Annotations: map[string]string{gatewayruntime.GatewayAuthNameAnnotation: "chat"}},
		Data: map[string][]byte{"token": []byte("inbound-token")},
	}
	outbound := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "outbound", Namespace: "default", Labels: map[string]string{
			gatewayruntime.GatewayOutboundAuthLabel: gatewayruntime.GatewayAuthEnabledValue,
			gatewayruntime.GatewayAuthNameLabel:     "chat",
		}, Annotations: map[string]string{
			gatewayruntime.GatewayAuthNameAnnotation:     "chat",
			gatewayruntime.GatewayAuthEndpointAnnotation: server.URL,
		}},
		Data: map[string][]byte{"token": []byte("outbound-token")},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&gatewayv1alpha1.Gateway{}).
		WithObjects(class, object, inbound, outbound).Build()
	reconciler := &GatewayReconciler{Client: client, Scheme: scheme, HTTPClient: server.Client(), AllowInsecureLoopback: true}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "chat"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	updated := &gatewayv1alpha1.Gateway{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "chat"}, updated); err != nil {
		t.Fatal(err)
	}
	if !updated.Status.Ready || !updated.Status.Connected || updated.Status.ObservedCapabilities == nil {
		t.Fatalf("Gateway status = %+v", updated.Status)
	}
	if updated.Status.ResolvedEndpoint != server.URL {
		t.Fatalf("resolved endpoint = %q, want %q", updated.Status.ResolvedEndpoint, server.URL)
	}
}

func TestValidateGatewaySpecRejectsNonCanonicalAuthSecretNames(t *testing.T) {
	tests := []struct {
		name         string
		inboundName  string
		outboundName string
		wantError    string
	}{
		{
			name:         "trim-equivalent names",
			inboundName:  "shared",
			outboundName: " shared ",
			wantError:    "must use separate Secrets",
		},
		{
			name:         "non-canonical inbound name",
			inboundName:  " inbound ",
			outboundName: "outbound",
			wantError:    "inboundAuthRef name must not contain surrounding whitespace",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := &gatewayv1alpha1.Gateway{
				Spec: gatewayv1alpha1.GatewaySpec{
					GatewayClassName: "generic-chat",
					InboundAuthRef: gatewayv1alpha1.GatewayBearerAuthReference{
						Name: test.inboundName, Key: "token",
					},
					OutboundAuthRef: gatewayv1alpha1.GatewayBearerAuthReference{
						Name: test.outboundName, Key: "token",
					},
				},
			}
			if err := validateGatewaySpec(object); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validateGatewaySpec() = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestValidateGatewayBindingFailsClosed(t *testing.T) {
	binding := &gatewayv1alpha1.GatewayBinding{
		Spec: gatewayv1alpha1.GatewayBindingSpec{
			GatewayRef: gatewayv1alpha1.GatewayBindingReference{Name: "chat"},
			AgentRef:   gatewayv1alpha1.GatewayBindingReference{Name: "agent"},
			Match:      gatewayv1alpha1.GatewayBindingMatch{AccountID: "acct", ContextID: "room"},
		},
	}
	if err := validateGatewayBindingSpec(binding); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("validateGatewayBindingSpec() = %v, want default-deny allowlist error", err)
	}
	binding.Spec.Match.SenderID = "sender"
	binding.Spec.Session.Mode = gatewayv1alpha1.GatewaySessionThread
	if err := validateGatewayBindingSpec(binding); err != nil {
		t.Fatalf("validateGatewayBindingSpec() error = %v", err)
	}
	if err := validateBindingCapabilities(binding, gatewayv1alpha1.GatewayCapabilities{InboundText: true, OutboundText: true, IdempotentDelivery: true}); err == nil {
		t.Fatal("validateBindingCapabilities() accepted thread mode without threads capability")
	}
	binding.Spec.Session.Mode = gatewayv1alpha1.GatewaySessionContext
	if err := validateBindingCapabilities(binding, gatewayv1alpha1.GatewayCapabilities{InboundText: true, OutboundText: true, IdempotentDelivery: true}); err == nil || !strings.Contains(err.Error(), "senderIdentity") {
		t.Fatalf("validateBindingCapabilities() = %v, want senderIdentity requirement for exact sender matching", err)
	}
}

func TestGatewayBindingsOverlapAtEqualPriority(t *testing.T) {
	left := &gatewayv1alpha1.GatewayBinding{Spec: gatewayv1alpha1.GatewayBindingSpec{
		GatewayRef:   gatewayv1alpha1.GatewayBindingReference{Name: "chat"},
		Match:        gatewayv1alpha1.GatewayBindingMatch{AccountID: "acct", ContextID: "room"},
		SenderPolicy: gatewayv1alpha1.GatewaySenderPolicy{AllowedSenderIDs: []string{"same"}},
	}}
	right := left.DeepCopy()
	right.Spec.Match.ThreadID = "thread"
	if !gatewayBindingsOverlap(left, right) {
		t.Fatal("broad and thread-specific bindings should overlap")
	}
	right.Spec.SenderPolicy.AllowedSenderIDs = []string{"different"}
	if gatewayBindingsOverlap(left, right) {
		t.Fatal("disjoint allowlists should not overlap")
	}

	left.Spec.Match.SenderID = "left"
	left.Spec.SenderPolicy = gatewayv1alpha1.GatewaySenderPolicy{Mode: gatewayv1alpha1.GatewaySenderPolicyAll}
	right.Spec.Match.SenderID = "right"
	right.Spec.SenderPolicy = gatewayv1alpha1.GatewaySenderPolicy{Mode: gatewayv1alpha1.GatewaySenderPolicyAll}
	if gatewayBindingsOverlap(left, right) {
		t.Fatal("disjoint exact-sender bindings should not overlap even with senderPolicy all")
	}
	right.Spec.Match.SenderID = "left"
	if !gatewayBindingsOverlap(left, right) {
		t.Fatal("matching exact-sender bindings should overlap")
	}
}
