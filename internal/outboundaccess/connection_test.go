/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type fakeConnectionSource struct {
	request    ConnectionCredentialRequest
	credential ConnectionCredential
	err        error
	calls      int
}

func (f *fakeConnectionSource) ResolveConnectionCredential(_ context.Context, req ConnectionCredentialRequest) (ConnectionCredential, error) {
	f.calls++
	f.request = req
	return f.credential, f.err
}

func connectionPolicySpec(provider string) corev1alpha1.OutboundAccessPolicySpec {
	return corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{
		ProviderRef: corev1alpha1.LocalObjectReference{Name: provider},
	}}
}

func acceptedProvider() *corev1alpha1.ConnectorProvider {
	return &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "tenant", Generation: 1},
		Status: corev1alpha1.ConnectorProviderStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{
			{Type: corev1alpha1.ConnectorProviderConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1},
			{Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: 1},
		}},
	}
}

func TestValidateSpecConnectionMode(t *testing.T) {
	prefix := "Token "
	for name, tt := range map[string]struct {
		spec    corev1alpha1.OutboundAccessPolicySpec
		wantErr string
	}{
		"valid":          {spec: connectionPolicySpec("github")},
		"custom output":  {spec: corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Output: &corev1alpha1.OutboundCredentialOutput{Header: "X-Token", Prefix: &prefix}}}},
		"empty provider": {spec: connectionPolicySpec(""), wantErr: "providerRef name is required"},
		"bad provider":   {spec: connectionPolicySpec("Not Valid"), wantErr: "valid resource name"},
		"txn header":     {spec: corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Output: &corev1alpha1.OutboundCredentialOutput{Header: "Txn-Token"}}}, wantErr: "header"},
		"with direct": {spec: corev1alpha1.OutboundAccessPolicySpec{
			Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}},
			Direct:     &corev1alpha1.DirectOutboundAccess{},
		}, wantErr: "exactly one of direct, gateway, or connection"},
		"with gateway": {spec: corev1alpha1.OutboundAccessPolicySpec{
			Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}},
			Gateway:    &corev1alpha1.GatewayOutboundAccess{},
		}, wantErr: "exactly one of direct, gateway, or connection"},
	} {
		t.Run(name, func(t *testing.T) {
			issue := ValidateSpec(&corev1alpha1.OutboundAccessPolicy{Spec: tt.spec})
			if tt.wantErr == "" {
				if issue != nil {
					t.Fatalf("unexpected issue %v", issue)
				}
				return
			}
			if issue == nil || !strings.Contains(issue.Message, tt.wantErr) {
				t.Fatalf("issue = %v, want %q", issue, tt.wantErr)
			}
		})
	}
}

func TestResolveReferencesConnectionProvider(t *testing.T) {
	scheme := resolverScheme(t)
	policy := readyPolicy("conn", connectionPolicySpec("github"))
	ctx := context.Background()

	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	issue, err := ResolveReferences(ctx, reader, policy, TrustConfig{})
	if err != nil || issue == nil || issue.Reason != ReasonReferenceNotFound {
		t.Fatalf("missing provider: issue = %v err = %v", issue, err)
	}

	pending := acceptedProvider()
	pending.Status.Conditions = nil
	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, pending).Build()
	issue, err = ResolveReferences(ctx, reader, policy, TrustConfig{})
	if err != nil || issue == nil || issue.Reason != ReasonReferenceInvalid {
		t.Fatalf("unaccepted provider: issue = %v err = %v", issue, err)
	}

	reader = ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, acceptedProvider()).Build()
	issue, err = ResolveReferences(ctx, reader, policy, TrustConfig{})
	if err != nil || issue != nil {
		t.Fatalf("accepted provider: issue = %v err = %v", issue, err)
	}
}

type connectionModeFixture struct {
	scheme    *runtime.Scheme
	policy    *corev1alpha1.OutboundAccessPolicy
	requester *corev1alpha1.RequestedBy
	frozen    map[string]FrozenConnection
	base      ResolveRequest
}

func newConnectionModeFixture(t *testing.T) connectionModeFixture {
	t.Helper()
	scheme := resolverScheme(t)
	policy := readyPolicy("conn", connectionPolicySpec("github"))
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	frozen := map[string]FrozenConnection{"conn": {UID: "uid-1", Generation: 2}}
	return connectionModeFixture{
		scheme: scheme, policy: policy, requester: requester, frozen: frozen,
		base: ResolveRequest{Namespace: "tenant", PolicyName: "conn", TargetScheme: "https", Requester: requester, FrozenConnections: frozen},
	}
}

func (f connectionModeFixture) newReader() *ctrlfake.ClientBuilder {
	return ctrlfake.NewClientBuilder().WithScheme(f.scheme).WithObjects(f.policy, acceptedProvider())
}

func TestKubernetesResolverConnectionModePreconditions(t *testing.T) {
	f := newConnectionModeFixture(t)
	base, newReader := f.base, f.newReader

	t.Run("no source in this process fails closed", func(t *testing.T) {
		resolver := &KubernetesResolver{Reader: newReader().Build()}
		_, err := resolver.Resolve(context.Background(), base)
		if err == nil || !strings.Contains(err.Error(), "only in the controller") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("requires a verified requester", func(t *testing.T) {
		source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho"}}
		resolver := &KubernetesResolver{Reader: newReader().Build(), Connections: source}
		for name, r := range map[string]*corev1alpha1.RequestedBy{
			"nil": nil, "no subject": {Issuer: "https://issuer.example.test"}, "no issuer": {Subject: "alice"},
		} {
			req := base
			req.Requester = r
			if _, err := resolver.Resolve(context.Background(), req); err == nil || !strings.Contains(err.Error(), "verified requester") {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
		if source.calls != 0 {
			t.Fatal("source must not be consulted without a requester")
		}
	})
	t.Run("requires a frozen binding", func(t *testing.T) {
		source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho"}}
		resolver := &KubernetesResolver{Reader: newReader().Build(), Connections: source}
		req := base
		req.FrozenConnections = nil
		if _, err := resolver.Resolve(context.Background(), req); err == nil || !strings.Contains(err.Error(), "frozen into the execution snapshot") {
			t.Fatalf("err = %v", err)
		}
		req.FrozenConnections = map[string]FrozenConnection{"other": {UID: "x"}}
		if _, err := resolver.Resolve(context.Background(), req); err == nil {
			t.Fatal("binding for another policy must not count")
		}
		if source.calls != 0 {
			t.Fatal("source must not be consulted without a frozen binding")
		}
	})
	t.Run("requires https and no authSecretRef", func(t *testing.T) {
		source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho"}}
		resolver := &KubernetesResolver{Reader: newReader().Build(), Connections: source}
		req := base
		req.TargetScheme = "http"
		if _, err := resolver.Resolve(context.Background(), req); err == nil || !strings.Contains(err.Error(), "HTTPS") {
			t.Fatalf("http err = %v", err)
		}
		req = base
		req.HasAuthSecretRef = true
		if _, err := resolver.Resolve(context.Background(), req); err == nil || !strings.Contains(err.Error(), "authSecretRef") {
			t.Fatalf("authSecretRef err = %v", err)
		}
	})
}

func TestKubernetesResolverConnectionModeInjection(t *testing.T) {
	f := newConnectionModeFixture(t)
	base, newReader, scheme, policy, requester, frozen := f.base, f.newReader, f.scheme, f.policy, f.requester, f.frozen
	t.Run("injects the credential", func(t *testing.T) {
		source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho_secret", TokenType: "bearer", ConnectionUID: "uid-1", Generation: 2, Mode: "readOnly"}}
		resolver := &KubernetesResolver{Reader: newReader().Build(), Connections: source}
		resolution, err := resolver.Resolve(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		if resolution.Adapter != AdapterConnection || resolution.CredentialHeader != "Authorization" || resolution.CredentialValue != "Bearer gho_secret" || resolution.ConnectionUID != "uid-1" {
			t.Fatalf("resolution = %#v", resolution)
		}
		if len(resolution.SensitiveValues) != 1 || resolution.SensitiveValues[0] != "gho_secret" {
			t.Fatalf("sensitive = %v", resolution.SensitiveValues)
		}
		if source.request.Provider != "github" || source.request.Issuer != requester.Issuer || source.request.Subject != "alice" || source.request.Frozen != frozen["conn"] || source.request.Namespace != "tenant" {
			t.Fatalf("source request = %+v", source.request)
		}
	})
	t.Run("honors output settings", func(t *testing.T) {
		prefix := "token "
		custom := readyPolicy("conn", corev1alpha1.OutboundAccessPolicySpec{Connection: &corev1alpha1.ConnectionOutboundAccess{
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"},
			Output:      &corev1alpha1.OutboundCredentialOutput{Header: "x-github-token", Prefix: &prefix},
		}})
		reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(custom, acceptedProvider()).Build()
		resolver := &KubernetesResolver{Reader: reader, Connections: &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho"}}}
		resolution, err := resolver.Resolve(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		if resolution.CredentialHeader != "X-Github-Token" || resolution.CredentialValue != "token gho" {
			t.Fatalf("resolution = %#v", resolution)
		}
	})
	t.Run("source failures and empty tokens fail closed", func(t *testing.T) {
		resolver := &KubernetesResolver{Reader: newReader().Build(), Connections: &fakeConnectionSource{err: errors.New("revoked")}}
		if _, err := resolver.Resolve(context.Background(), base); err == nil || !strings.Contains(err.Error(), "revoked") {
			t.Fatalf("err = %v", err)
		}
		resolver = &KubernetesResolver{Reader: newReader().Build(), Connections: &fakeConnectionSource{}}
		if _, err := resolver.Resolve(context.Background(), base); err == nil || !strings.Contains(err.Error(), "empty credential") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unaccepted provider fails closed", func(t *testing.T) {
		pending := acceptedProvider()
		pending.Status.Conditions = nil
		reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, pending).Build()
		resolver := &KubernetesResolver{Reader: reader, Connections: &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho"}}}
		if _, err := resolver.Resolve(context.Background(), base); err == nil {
			t.Fatal("unaccepted provider must fail")
		}
	})
}
