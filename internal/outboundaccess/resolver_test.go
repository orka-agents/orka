/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"context"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

type captureExchanger struct {
	request tokenexchange.Request
	result  tokenexchange.Result
	err     error
}

func (e *captureExchanger) Exchange(_ context.Context, req tokenexchange.Request) (tokenexchange.Result, error) {
	e.request = req
	return e.result, e.err
}

func TestKubernetesResolverDirectSecretExchange(t *testing.T) {
	scheme := resolverScheme(t)
	policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
		Grant:         corev1alpha1.OutboundGrantTokenExchange,
		TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
		Subject: corev1alpha1.OutboundTokenSource{
			Source:    corev1alpha1.OutboundTokenSourceSecretRef,
			TokenType: "urn:example:assertion",
			SecretRef: secretRef("subject", "token"),
		},
		Scopes:                  []string{"api.read"},
		RequestedTokenType:      "urn:example:resource",
		ExpectedIssuedTokenType: "urn:example:resource",
	}})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"}, Data: map[string][]byte{"token": []byte("subject-assertion")}}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, secret).Build()
	exchanger := &captureExchanger{result: tokenexchange.Result{AccessToken: "resource-credential", IssuedTokenType: "urn:example:resource", TokenType: "Bearer"}}
	resolver := &KubernetesResolver{Reader: reader, Exchanger: exchanger}

	resolution, err := resolver.Resolve(context.Background(), ResolveRequest{Namespace: "tenant", PolicyName: "direct", TargetScheme: "https"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolution.Adapter != AdapterDirect || resolution.CredentialHeader != "Authorization" || resolution.CredentialValue != "Bearer resource-credential" {
		t.Fatalf("resolution = %#v", resolution)
	}
	if exchanger.request.SubjectToken != "subject-assertion" || exchanger.request.SubjectTokenType != "urn:example:assertion" {
		t.Fatalf("exchange request = %#v", exchanger.request)
	}
	if exchanger.request.RequiredTokenType != "Bearer" || exchanger.request.CacheNamespace == "" {
		t.Fatalf("exchange validation/cache = %#v", exchanger.request)
	}
}

func TestKubernetesResolverTransactionScopeSubset(t *testing.T) {
	scheme := resolverScheme(t)
	policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: validDirect()})
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	resolver := &KubernetesResolver{Reader: reader, Exchanger: &captureExchanger{result: tokenexchange.Result{AccessToken: "token", IssuedTokenType: tokenexchange.TokenTypeAccessToken, TokenType: "Bearer"}}}

	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Namespace:               "tenant",
		PolicyName:              "direct",
		TargetScheme:            "https",
		TransactionToken:        "transaction-token",
		ParentTransactionScopes: []string{"write"},
	})
	if err == nil || !containsFold(err.Error(), "not present") {
		t.Fatalf("Resolve() error = %v, want scope expansion denial", err)
	}
}

func TestKubernetesResolverActorTransactionScopeSubset(t *testing.T) {
	scheme := resolverScheme(t)
	policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
		Grant:         corev1alpha1.OutboundGrantTokenExchange,
		TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
		Subject: corev1alpha1.OutboundTokenSource{
			Source:    corev1alpha1.OutboundTokenSourceSecretRef,
			TokenType: tokenexchange.TokenTypeAccessToken,
			SecretRef: secretRef("subject", "token"),
		},
		Actor:                   &corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken},
		Scopes:                  []string{"api.read"},
		ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
	}})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"},
		Data:       map[string][]byte{"token": []byte("subject-token")},
	}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, secret).Build()
	resolver := &KubernetesResolver{
		Reader:    reader,
		Exchanger: &captureExchanger{result: tokenexchange.Result{AccessToken: "resource", IssuedTokenType: tokenexchange.TokenTypeAccessToken, TokenType: "Bearer"}},
	}

	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Namespace:               "tenant",
		PolicyName:              "direct",
		TargetScheme:            "https",
		TransactionToken:        "transaction-token",
		ParentTransactionScopes: []string{"api.write"},
	})
	if err == nil || !containsFold(err.Error(), "not present") {
		t.Fatalf("Resolve() error = %v, want actor scope expansion denial", err)
	}
}

func TestKubernetesResolverRejectsPlaintextDirectTarget(t *testing.T) {
	scheme := resolverScheme(t)
	policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: validDirect()})
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	_, err := (&KubernetesResolver{Reader: reader}).Resolve(context.Background(), ResolveRequest{Namespace: "tenant", PolicyName: "direct", TargetScheme: "http"})
	if err == nil || !containsFold(err.Error(), "HTTPS Tool URL") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestKubernetesResolverGatewayPreservesExactServiceTuple(t *testing.T) {
	scheme := resolverScheme(t)
	policy := readyPolicy("gateway", corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{
		ServiceRef: serviceRef("agentgateway", "", 8080),
		Scheme:     "http",
	}})
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agentgateway", Namespace: "tenant"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}}}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, service).Build()
	resolver := &KubernetesResolver{Reader: reader}
	resolution, err := resolver.Resolve(context.Background(), ResolveRequest{Namespace: "tenant", PolicyName: "gateway"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolution.Adapter != AdapterGateway || resolution.GatewayScheme != "http" || resolution.GatewayHost != "agentgateway.tenant.svc:8080" {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func resolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func readyPolicy(name string, spec corev1alpha1.OutboundAccessPolicySpec) *corev1alpha1.OutboundAccessPolicy {
	generation := int64(3)
	return &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant", Generation: generation},
		Spec:       spec,
		Status: corev1alpha1.OutboundAccessPolicyStatus{
			ObservedGeneration: generation,
			Conditions: []metav1.Condition{
				{Type: corev1alpha1.OutboundAccessPolicyConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: generation},
				{Type: corev1alpha1.OutboundAccessPolicyConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: generation},
			},
		},
	}
}

func TestKubernetesResolverServiceAccountSubject(t *testing.T) {
	scheme := resolverScheme(t)
	expires := metav1.NewTime(time.Now().Add(10 * time.Minute))
	policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
		Grant:         corev1alpha1.OutboundGrantTokenExchange,
		TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
		Subject: corev1alpha1.OutboundTokenSource{
			Source: corev1alpha1.OutboundTokenSourceServiceAccount,
			ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{
				Name:      "workload",
				Audiences: []string{"resource-api"},
			},
		},
		ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
	}})
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant"}}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, serviceAccount).Build()
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		create := action.(ktesting.CreateAction)
		request := create.GetObject().(*authenticationv1.TokenRequest)
		if len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != "resource-api" {
			t.Fatalf("TokenRequest audiences = %#v", request.Spec.Audiences)
		}
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "service-account-subject", ExpirationTimestamp: expires}}, nil
	})
	exchanger := &captureExchanger{result: tokenexchange.Result{AccessToken: "resource", IssuedTokenType: tokenexchange.TokenTypeAccessToken, TokenType: "Bearer"}}
	resolver := &KubernetesResolver{Reader: reader, KubeClient: clientset, Exchanger: exchanger}
	if _, err := resolver.Resolve(context.Background(), ResolveRequest{Namespace: "tenant", PolicyName: "direct", TargetScheme: "https"}); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if exchanger.request.SubjectToken != "service-account-subject" || exchanger.request.SubjectTokenType != tokenexchange.TokenTypeAccessToken {
		t.Fatalf("subject exchange = %#v", exchanger.request)
	}
	if !exchanger.request.SubjectExpiresAt.Equal(expires.Time) {
		t.Fatalf("subject expiry = %s, want %s", exchanger.request.SubjectExpiresAt, expires.Time)
	}
}

func TestKubernetesResolverServiceAccountActorExpiry(t *testing.T) {
	scheme := resolverScheme(t)
	expires := metav1.NewTime(time.Now().Add(10 * time.Minute))
	policy := readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
		Grant:         corev1alpha1.OutboundGrantTokenExchange,
		TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
		Subject: corev1alpha1.OutboundTokenSource{
			Source:    corev1alpha1.OutboundTokenSourceSecretRef,
			TokenType: tokenexchange.TokenTypeAccessToken,
			SecretRef: secretRef("subject", "token"),
		},
		Actor: &corev1alpha1.OutboundTokenSource{
			Source:            corev1alpha1.OutboundTokenSourceServiceAccount,
			ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{Name: "workload"},
		},
		ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
	}})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"},
		Data:       map[string][]byte{"token": []byte("subject-token")},
	}
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant"}}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, secret, serviceAccount).Build()
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token: "service-account-actor", ExpirationTimestamp: expires,
		}}, nil
	})
	exchanger := &captureExchanger{result: tokenexchange.Result{
		AccessToken: "resource", IssuedTokenType: tokenexchange.TokenTypeAccessToken, TokenType: "Bearer",
	}}
	resolver := &KubernetesResolver{Reader: reader, KubeClient: clientset, Exchanger: exchanger}
	if _, err := resolver.Resolve(context.Background(), ResolveRequest{Namespace: "tenant", PolicyName: "direct", TargetScheme: "https"}); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if exchanger.request.ActorToken != "service-account-actor" || !exchanger.request.ActorExpiresAt.Equal(expires.Time) {
		t.Fatalf("actor exchange = %#v", exchanger.request)
	}
}

func TestKubernetesResolverReusesDefaultExchanger(t *testing.T) {
	resolver := &KubernetesResolver{}
	first := resolver.exchanger()
	second := resolver.exchanger()
	if first == nil || first != second {
		t.Fatalf("default exchanger was not reused: %p != %p", first, second)
	}
}
