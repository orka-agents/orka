package outboundaccess

import (
	"context"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type credentialReadRecorder struct {
	client.Reader
	reads int
}

func (r *credentialReadRecorder) Get(
	ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption,
) error {
	if _, secret := object.(*corev1.Secret); secret {
		r.reads++
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestNamedAuthSecretRejectsDirectBeforeExchange(t *testing.T) {
	policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
		Grant:         corev1alpha1.OutboundGrantTokenExchange,
		TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
		Subject:       corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken},
		Scopes:        []string{"read"}, ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
	}})
	exchanger := &captureExchanger{result: tokenexchange.Result{AccessToken: "fixture-resource", TokenType: "Bearer"}}
	resolver := &KubernetesResolver{
		Reader:    fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(policy).Build(),
		Exchanger: exchanger,
	}
	_, err := resolver.Resolve(t.Context(), ResolveRequest{
		Namespace: "tenant", PolicyName: policy.Name, TargetScheme: "https", HasAuthSecretRef: true,
		TransactionToken: "fixture-task", ParentTransactionScopes: []string{"read"},
		CredentialAuthorityEnforced: true, CredentialScopeAllowed: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot coexist with authSecretRef") {
		t.Fatal("named remote credential did not reject Direct preparation")
	}
	if exchanger.request.Endpoint != "" || exchanger.request.SubjectToken != "" {
		t.Fatal("Direct preparation attempted an exchange before rejecting the named credential")
	}
}

func TestDirectTLSCredentialAuthorityPrecedesReferenceSecretReads(t *testing.T) {
	for _, scopeAllowed := range []bool{false, true} {
		name, wantError := "denied scope", "not authorized"
		if scopeAllowed {
			name, wantError = "different Secret", "does not match"
		}
		t.Run(name, func(t *testing.T) {
			policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
				Grant: corev1alpha1.OutboundGrantTokenExchange,
				TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{
					URL: "https://issuer.example.test/token",
					TLS: &corev1alpha1.OutboundTLSConfig{CASecretRef: secretRef("issuer-ca", "ca.crt")},
				},
				Subject: corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken},
				Scopes:  []string{"read"}, ExpectedIssuedTokenType: "urn:ietf:params:oauth:token-type:access_token",
			}})
			if issue := ValidateSpec(policy); issue != nil {
				t.Fatal(issue)
			}
			reader := &credentialReadRecorder{Reader: fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(policy).Build()}
			resolver := &KubernetesResolver{Reader: reader}
			_, err := resolver.Resolve(t.Context(), ResolveRequest{
				Namespace: "tenant", PolicyName: policy.Name, TargetScheme: "https",
				CredentialAuthorityEnforced: true, CredentialScopeAllowed: scopeAllowed, CredentialSecret: "different-secret",
			})
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatal("direct TLS selector did not enforce Task credential authority")
			}
			if reader.reads != 0 {
				t.Fatal("direct reference resolution read a Secret before authorization")
			}
		})
	}
}

func TestGatewayCredentialAuthorityPrecedesReferenceSecretReads(t *testing.T) {
	for _, scopeAllowed := range []bool{false, true} {
		name := "denied scope"
		if scopeAllowed {
			name = "different Secret"
		}
		t.Run(name, func(t *testing.T) {
			policy := readyPolicy("gateway", corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{
				ServiceRef: serviceRef("gateway", "", 8443), Scheme: "https",
				TLS: &corev1alpha1.OutboundTLSConfig{CASecretRef: secretRef("gateway-ca", "ca.crt")},
			}})
			service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "tenant"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8443}}}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-ca", Namespace: "tenant"}, Data: map[string][]byte{"ca.crt": []byte("must-not-read")}}
			reader := &credentialReadRecorder{Reader: fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(policy, service, secret).Build()}
			resolver := &KubernetesResolver{Reader: reader}
			_, err := resolver.Resolve(t.Context(), ResolveRequest{
				Namespace: "tenant", PolicyName: policy.Name, CredentialAuthorityEnforced: true,
				CredentialScopeAllowed: scopeAllowed, CredentialSecret: "different-secret",
			})
			if err == nil {
				t.Fatal("unavailable credential authority accepted")
			}
			if reader.reads != 0 {
				t.Fatalf("reference resolution read %d Secrets before Task credential authorization", reader.reads)
			}
		})
	}
}
