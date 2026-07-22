/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

func TestValidateSpecRequiresExactlyOneAdapter(t *testing.T) {
	for _, spec := range []corev1alpha1.OutboundAccessPolicySpec{
		{},
		{Direct: validDirect(), Gateway: &corev1alpha1.GatewayOutboundAccess{ServiceRef: serviceRef("gateway", "", 8080)}},
	} {
		policy := &corev1alpha1.OutboundAccessPolicy{Spec: spec}
		if issue := ValidateSpec(policy); issue == nil || issue.Reason != ReasonInvalidPolicy {
			t.Fatalf("ValidateSpec() = %#v, want invalid policy", issue)
		}
	}
}

func TestValidateSpecDirectSecurityRules(t *testing.T) {
	prefix := ""
	tests := []struct {
		name   string
		mutate func(*corev1alpha1.DirectOutboundAccess)
		want   string
	}{
		{name: "reserved parameter", mutate: func(d *corev1alpha1.DirectOutboundAccess) {
			d.AdditionalParameters = map[string]string{"client_secret": "x"}
		}, want: "reserved"},
		{name: "transaction output header", mutate: func(d *corev1alpha1.DirectOutboundAccess) {
			d.Output = &corev1alpha1.OutboundCredentialOutput{Header: "Txn-Token", Prefix: &prefix}
		}, want: "Txn-Token"},
		{name: "http public endpoint", mutate: func(d *corev1alpha1.DirectOutboundAccess) { d.TokenEndpoint.URL = "http://issuer.example.test/token" }, want: "HTTPS"},
		{name: "private HTTPS endpoint", mutate: func(d *corev1alpha1.DirectOutboundAccess) { d.TokenEndpoint.URL = "https://127.0.0.1/token" }, want: "private"},
		{name: "jwt actor", mutate: func(d *corev1alpha1.DirectOutboundAccess) {
			d.Grant = corev1alpha1.OutboundGrantJWTBearer
			d.Actor = &corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken}
		}, want: "actor"},
		{name: "secret source without type", mutate: func(d *corev1alpha1.DirectOutboundAccess) {
			d.Subject = corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceSecretRef, SecretRef: secretRef("subject", "token")}
		}, want: "tokenType"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			direct := validDirect()
			tt.mutate(direct)
			issue := ValidateSpec(&corev1alpha1.OutboundAccessPolicy{Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: direct}})
			if issue == nil || !containsFold(issue.Message, tt.want) {
				t.Fatalf("ValidateSpec() = %#v, want containing %q", issue, tt.want)
			}
		})
	}
}

func TestResolveReferencesServiceTrustAndSecrets(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: "identity"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8443}}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"}, Data: map[string][]byte{"token": []byte("assertion")}},
	).Build()
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "direct", Namespace: "tenant"},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
			Grant: corev1alpha1.OutboundGrantTokenExchange,
			TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{
				ServiceRef: new(serviceRef("token", "identity", 8443)),
				Scheme:     "https",
				Path:       "/oauth/token",
			},
			Subject: corev1alpha1.OutboundTokenSource{
				Source:    corev1alpha1.OutboundTokenSourceSecretRef,
				TokenType: tokenexchange.TokenTypeAccessToken,
				SecretRef: secretRef("subject", "token"),
			},
			ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
		}},
	}
	issue, err := ResolveReferences(context.Background(), reader, policy, TrustConfig{})
	if err != nil {
		t.Fatalf("ResolveReferences() error = %v", err)
	}
	if issue == nil || issue.Reason != ReasonReferenceDenied {
		t.Fatalf("ResolveReferences() issue = %#v, want cross-namespace denial", issue)
	}

	trusted, err := ParseTrustedServiceReferences("identity/token:8443")
	if err != nil {
		t.Fatal(err)
	}
	issue, err = ResolveReferences(context.Background(), reader, policy, TrustConfig{TokenEndpoints: trusted})
	if err != nil || issue != nil {
		t.Fatalf("ResolveReferences() = %#v, %v", issue, err)
	}

	policy.Spec.Direct.Subject.SecretRef.Namespace = "identity"
	issue, err = ResolveReferences(context.Background(), reader, policy, TrustConfig{TokenEndpoints: trusted})
	if err != nil {
		t.Fatalf("ResolveReferences() error = %v", err)
	}
	if issue == nil || issue.Reason != ReasonReferenceDenied {
		t.Fatalf("cross-namespace Secret issue = %#v", issue)
	}
}

func TestParseTrustedServiceReferencesRejectsWildcardsAndRequiresExactPort(t *testing.T) {
	for _, value := range []string{"identity/*:443", "identity/token", "identity/token:any", "identity/token:0"} {
		if _, err := ParseTrustedServiceReferences(value); err == nil {
			t.Fatalf("ParseTrustedServiceReferences(%q) error = nil", value)
		}
	}
	refs, err := ParseTrustedServiceReferences("identity/token:443")
	if err != nil {
		t.Fatal(err)
	}
	if !refs.Allows(serviceRef("token", "identity", 443), "tenant") || refs.Allows(serviceRef("token", "identity", 8443), "tenant") {
		t.Fatal("trusted Service allowlist did not enforce exact port")
	}
}

func validDirect() *corev1alpha1.DirectOutboundAccess {
	return &corev1alpha1.DirectOutboundAccess{
		Grant:                   corev1alpha1.OutboundGrantTokenExchange,
		TokenEndpoint:           corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
		Subject:                 corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken},
		Scopes:                  []string{"read"},
		RequestedTokenType:      tokenexchange.TokenTypeAccessToken,
		ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
	}
}

func serviceRef(name, namespace string, port int32) corev1alpha1.OutboundServiceReference {
	return corev1alpha1.OutboundServiceReference{Name: name, Namespace: namespace, Port: port}
}

//go:fix inline

func secretRef(name, key string) *corev1alpha1.NamespacedSecretKeySelector {
	return &corev1alpha1.NamespacedSecretKeySelector{Name: name, Key: key}
}

func containsFold(value, want string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(want))
}

func TestValidateSpecJWTBearerDoesNotRequireIssuedTokenType(t *testing.T) {
	direct := validDirect()
	direct.Grant = corev1alpha1.OutboundGrantJWTBearer
	direct.ExpectedIssuedTokenType = ""
	direct.Subject = corev1alpha1.OutboundTokenSource{
		Source:    corev1alpha1.OutboundTokenSourceSecretRef,
		SecretRef: secretRef("assertion", "jwt"),
	}
	if issue := ValidateSpec(&corev1alpha1.OutboundAccessPolicy{Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: direct}}); issue != nil {
		t.Fatalf("ValidateSpec() issue = %#v", issue)
	}
}

func TestResolveReferencesRejectsExternalNameService(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "tenant"},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "metadata.example.internal",
			Ports:        []corev1.ServicePort{{Port: 8443}},
		},
	}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "tenant"},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{
			ServiceRef: serviceRef("gateway", "", 8443),
		}},
	}
	issue, err := ResolveReferences(context.Background(), reader, policy, TrustConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if issue == nil || issue.Reason != ReasonReferenceDenied || !strings.Contains(issue.Message, "ExternalName") {
		t.Fatalf("ResolveReferences() issue = %#v", issue)
	}
}

func TestValidateSpecRejectsFragmentBearingTokenEndpoint(t *testing.T) {
	direct := validDirect()
	direct.TokenEndpoint.URL = "https://issuer.example.test/token#fragment"
	issue := ValidateSpec(&corev1alpha1.OutboundAccessPolicy{Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: direct}})
	if issue == nil || !strings.Contains(issue.Message, "fragment") {
		t.Fatalf("ValidateSpec() issue = %#v", issue)
	}
}
