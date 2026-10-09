/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
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
		Spec: corev1alpha1.ConnectorProviderSpec{Tools: []corev1alpha1.ConnectorTool{
			{Name: "gh_search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "search",
				HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET"}},
			{Name: "gh_comment", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "comment",
				HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/comments"}},
			{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin},
		}},
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
		base: ResolveRequest{
			Namespace: "tenant", PolicyName: "conn", TargetScheme: "https", Requester: requester, FrozenConnections: frozen,
			Tool: ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead},
		},
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
	t.Run("requires the policy the Connection was frozen under", func(t *testing.T) {
		source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho"}}
		resolver := &KubernetesResolver{Reader: newReader().Build(), Connections: source}
		req := base
		req.FrozenConnections = map[string]FrozenConnection{}
		for name, frozen := range base.FrozenConnections {
			// The snapshot pinned another policy object (or generation): the
			// policy's credential output may differ from what was dispatched.
			frozen.PolicyUID, frozen.PolicyGeneration = "another-policy-uid", 7
			req.FrozenConnections[name] = frozen
		}
		if _, err := resolver.Resolve(context.Background(), req); err == nil || !strings.Contains(err.Error(), "changed since the task was dispatched") {
			t.Fatalf("err = %v", err)
		}
		if source.calls != 0 {
			t.Fatal("source must not be consulted under a policy the snapshot did not pin")
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

func TestKubernetesResolverConnectionModeBindsDeclaredTool(t *testing.T) {
	f := newConnectionModeFixture(t)
	source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho", Mode: corev1alpha1.ConnectionModeReadOnly}}
	resolver := &KubernetesResolver{Reader: f.newReader().Build(), Connections: source}
	for name, tt := range map[string]struct {
		tool ToolBinding
		want string
	}{
		"undeclared":   {tool: ToolBinding{Name: "gh_other", URL: "https://api.github.com/search/issues", Method: "GET", Class: "read"}, want: "not declared"},
		"url mismatch": {tool: ToolBinding{Name: "gh_search", URL: "https://evil.example.test/steal", Method: "GET", Class: "read"}, want: "does not match the endpoint"},
		"method":       {tool: ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "POST", Class: "read"}, want: "does not match the endpoint"},
		"class":        {tool: ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: "write"}, want: "class does not match"},
		"builtin":      {tool: ToolBinding{Name: "create_pull_request", URL: "https://api.github.com/pulls", Class: "write"}, want: "not a curated HTTP tool"},
		"no tool":      {tool: ToolBinding{}, want: "executing tool identity"},
		"write on readOnly link": {
			tool: ToolBinding{Name: "gh_comment", URL: "https://api.github.com/comments", Class: "write"}, want: "readOnly",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := f.base
			req.Tool = tt.tool
			_, err := resolver.Resolve(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	// A readWrite link may use the declared write tool with the default method.
	source.credential.Mode = corev1alpha1.ConnectionModeReadWrite
	req := f.base
	req.Tool = ToolBinding{Name: "gh_comment", URL: "https://api.github.com/comments", Class: corev1alpha1.AgentRuntimeBrokeredToolClassWrite}
	if resolution, err := resolver.Resolve(context.Background(), req); err != nil || resolution.CredentialValue != "Bearer gho" {
		t.Fatalf("declared write tool on readWrite link: %+v err = %v", resolution, err)
	}
}

func TestDeclaredConnectorToolRequiresDeclaredHeaders(t *testing.T) {
	provider := &corev1alpha1.ConnectorProvider{Spec: corev1alpha1.ConnectorProviderSpec{Tools: []corev1alpha1.ConnectorTool{{
		Name: "gh_search", Source: corev1alpha1.ConnectorToolSourceHTTP, Class: corev1alpha1.ConnectorToolClassRead,
		HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET", Headers: map[string]string{"Accept": "application/vnd.github+json"}},
	}}}}
	binding := ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead}
	// The declared header set must match exactly; a canonical spelling is fine.
	binding.Headers = map[string]string{"accept": "application/vnd.github+json"}
	if _, err := DeclaredConnectorTool(provider, binding); err != nil {
		t.Fatalf("declared headers must be accepted: %v", err)
	}
	for name, headers := range map[string]map[string]string{
		"missing":   nil,
		"extra":     {"Accept": "application/vnd.github+json", "X-HTTP-Method-Override": "DELETE"},
		"different": {"Accept": "text/plain"},
		"aliases":   {"accept": "application/vnd.github+json", "ACCEPT": "application/vnd.github+json"},
	} {
		binding.Headers = headers
		if _, err := DeclaredConnectorTool(provider, binding); err == nil || !strings.Contains(err.Error(), "headers") {
			t.Fatalf("%s headers err = %v, want refusal", name, err)
		}
	}
}

func TestDeclaredConnectorToolRequiresDeclaredSchema(t *testing.T) {
	schema := `{"type":"object","required":["q"],"properties":{"q":{"type":"string"}},"additionalProperties":false}`
	provider := &corev1alpha1.ConnectorProvider{Spec: corev1alpha1.ConnectorProviderSpec{Tools: []corev1alpha1.ConnectorTool{{
		Name: "gh_search", Source: corev1alpha1.ConnectorToolSourceHTTP, Class: corev1alpha1.ConnectorToolClassRead,
		Parameters: &apiextensionsv1.JSON{Raw: []byte(schema)},
		HTTP:       &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET"},
	}}}}
	binding := ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead}
	// The same schema, however formatted, is accepted.
	binding.Parameters = &apiextensionsv1.JSON{Raw: []byte(`{
		"additionalProperties": false, "properties": {"q": {"type": "string"}}, "required": ["q"], "type": "object"}`)}
	if _, err := DeclaredConnectorTool(provider, binding); err != nil {
		t.Fatalf("equivalent schema must be accepted: %v", err)
	}
	// A weaker or absent Tool schema cannot replace the curated one.
	for name, parameters := range map[string]*apiextensionsv1.JSON{
		"absent": nil,
		"weaker": {Raw: []byte(`{"type":"object"}`)},
		"broken": {Raw: []byte(`{`)},
	} {
		binding.Parameters = parameters
		if _, err := DeclaredConnectorTool(provider, binding); err == nil || !strings.Contains(err.Error(), "parameters") {
			t.Fatalf("%s schema err = %v, want refusal", name, err)
		}
	}
}

func TestDeclaredConnectorToolEmptyDeclaredHeaderStillRequiresPresence(t *testing.T) {
	provider := &corev1alpha1.ConnectorProvider{Spec: corev1alpha1.ConnectorProviderSpec{Tools: []corev1alpha1.ConnectorTool{{
		Name: "gh_search", Source: corev1alpha1.ConnectorToolSourceHTTP, Class: corev1alpha1.ConnectorToolClassRead,
		HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET", Headers: map[string]string{"Accept": ""}},
	}}}}
	binding := ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead}
	// A declared header with an empty value is still a declared header: an
	// unrelated header of the same count cannot stand in for it.
	binding.Headers = map[string]string{"X-HTTP-Method-Override": "DELETE"}
	if _, err := DeclaredConnectorTool(provider, binding); err == nil {
		t.Fatal("an undeclared header must not replace a declared empty one")
	}
	binding.Headers = map[string]string{"Accept": ""}
	if _, err := DeclaredConnectorTool(provider, binding); err != nil {
		t.Fatalf("the declared empty header must be accepted: %v", err)
	}
}

// A Tool may not keep a credential-bearing request open longer than the
// provider's curated timeout; both sides default to 30s.
func TestDeclaredConnectorToolComparesTimeouts(t *testing.T) {
	provider := &corev1alpha1.ConnectorProvider{ObjectMeta: metav1.ObjectMeta{Name: "github"}, Spec: corev1alpha1.ConnectorProviderSpec{
		Tools: []corev1alpha1.ConnectorTool{{
			Name: "gh_search", Source: corev1alpha1.ConnectorToolSourceHTTP, Class: corev1alpha1.ConnectorToolClassRead,
			HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET", Timeout: &metav1.Duration{Duration: time.Second}},
		}, {
			Name: "gh_default", Source: corev1alpha1.ConnectorToolSourceHTTP, Class: corev1alpha1.ConnectorToolClassRead,
			HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/user", Method: "GET"},
		}},
	}}
	binding := ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead}
	if _, err := DeclaredConnectorTool(provider, binding); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("a Tool defaulting to 30s against a 1s curated bound err = %v, want refusal", err)
	}
	binding.Timeout = time.Second
	if _, err := DeclaredConnectorTool(provider, binding); err != nil {
		t.Fatalf("matching timeout: %v", err)
	}
	defaulted := ToolBinding{Name: "gh_default", URL: "https://api.github.com/user", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead}
	if _, err := DeclaredConnectorTool(provider, defaulted); err != nil {
		t.Fatalf("both sides defaulting to 30s: %v", err)
	}
	defaulted.Timeout = 30 * time.Second
	if _, err := DeclaredConnectorTool(provider, defaulted); err != nil {
		t.Fatalf("an explicit 30s equals the default: %v", err)
	}
	defaulted.Timeout = 10 * time.Minute
	if _, err := DeclaredConnectorTool(provider, defaulted); err == nil {
		t.Fatal("a longer Tool timeout must be refused")
	}
	// An explicit 0s or negative timeout would run without a deadline; it
	// is refused rather than read as the default.
	for _, explicit := range []time.Duration{0, -time.Second} {
		defaulted.Timeout, defaulted.TimeoutSet = explicit, true
		if _, err := DeclaredConnectorTool(provider, defaulted); err == nil || !strings.Contains(err.Error(), "positive") {
			t.Fatalf("explicit %s timeout err = %v, want refusal", explicit, err)
		}
	}
}

func TestDeclaredConnectorToolBuiltinBindings(t *testing.T) {
	provider := &corev1alpha1.ConnectorProvider{ObjectMeta: metav1.ObjectMeta{Name: "github"}, Spec: corev1alpha1.ConnectorProviderSpec{OAuth: corev1alpha1.ConnectorOAuthConfig{
		AuthorizeURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token",
	}, Tools: []corev1alpha1.ConnectorTool{
		{Name: "list_pull_requests", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin},
		{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin},
		{Name: "gh_search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP,
			HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET"}},
	}}}
	read := ToolBinding{Name: "list_pull_requests", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true, Timeout: connectors.BuiltinToolTimeout, TimeoutSet: true}
	if declared, err := DeclaredConnectorTool(provider, read); err != nil || declared.Name != "list_pull_requests" {
		t.Fatalf("declared = %+v err = %v", declared, err)
	}
	write := ToolBinding{Name: "create_pull_request", Class: corev1alpha1.AgentRuntimeBrokeredToolClassWrite, Builtin: true, Timeout: connectors.BuiltinToolTimeout, TimeoutSet: true}
	if _, err := DeclaredConnectorTool(provider, write); err != nil {
		t.Fatalf("write built-in: %v", err)
	}
	// A provider that does not issue github.com credentials never serves a
	// built-in, even when its declaration slipped past validation.
	enterprise := provider.DeepCopy()
	enterprise.Spec.OAuth.TokenURL = "https://github.example.com/login/oauth/access_token"
	if _, err := DeclaredConnectorTool(enterprise, read); err == nil || !strings.Contains(err.Error(), "does not issue credentials for") {
		t.Fatalf("enterprise err = %v", err)
	}
	for name, binding := range map[string]ToolBinding{
		// The class is fixed by the catalog, not by the caller.
		"class":      {Name: "create_pull_request", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true},
		"http":       {Name: "gh_search", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true},
		"unknown":    {Name: "get_issue", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true},
		"not linked": {Name: "web_search", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true},
		"url":        {Name: "list_pull_requests", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true, URL: "https://evil.example.test", Timeout: connectors.BuiltinToolTimeout, TimeoutSet: true},
		// The binding must carry exactly the catalog bound: no timeout, or a caller's own, is refused.
		"no timeout": {Name: "list_pull_requests", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true},
		"timeout":    {Name: "list_pull_requests", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true, TimeoutSet: true, Timeout: 30 * time.Minute},
		// A custom Tool never matches a built-in declaration.
		"custom": {Name: "list_pull_requests", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, URL: "https://api.github.com/pulls", Method: "GET"},
	} {
		if _, err := DeclaredConnectorTool(provider, binding); err == nil {
			t.Fatalf("%s: want refusal", name)
		}
	}
}

func TestResolveConnectionRefusesBuiltinBindingsAndForeignProviders(t *testing.T) {
	provider := acceptedProvider()
	policy := &corev1alpha1.OutboundAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "github-conn", Namespace: "tenant", UID: "policy-uid"}, Spec: connectionPolicySpec("github")}
	source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "tok", Mode: corev1alpha1.ConnectionModeReadWrite}}
	resolver := &KubernetesResolver{Reader: ctrlfake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(policy, provider).Build(), Connections: source}
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	tool := ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead}
	frozen := map[string]FrozenConnection{"github-conn": {UID: "conn-uid", Generation: 1, GrantSequence: 1, Provider: "github"}}
	if _, err := resolver.resolveConnection(context.Background(), policy, ResolveRequest{Requester: requester, FrozenConnections: frozen, Tool: tool}); err != nil {
		t.Fatalf("matching provider: %v", err)
	}
	foreign := map[string]FrozenConnection{"github-conn": {UID: "conn-uid", Generation: 1, GrantSequence: 1, Provider: "gitlab"}}
	if _, err := resolver.resolveConnection(context.Background(), policy, ResolveRequest{Requester: requester, FrozenConnections: foreign, Tool: tool}); err == nil || !strings.Contains(err.Error(), "different provider") {
		t.Fatalf("foreign provider err = %v", err)
	}
	builtin := ToolBinding{Name: "list_pull_requests", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Builtin: true, Timeout: connectors.BuiltinToolTimeout, TimeoutSet: true}
	if _, err := resolver.resolveConnection(context.Background(), policy, ResolveRequest{Requester: requester, FrozenConnections: frozen, Tool: builtin}); err == nil || !strings.Contains(err.Error(), "no outbound access policy") {
		t.Fatalf("built-in binding err = %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("source calls = %d, want only the matching resolution", source.calls)
	}
}

// TestKubernetesResolverConnectionModeJudgesArgumentsFirst covers a call
// whose arguments the curated schema rejects: it is refused before the
// credential source runs, so it can never refresh or shred custody.
func TestKubernetesResolverConnectionModeJudgesArgumentsFirst(t *testing.T) {
	f := newConnectionModeFixture(t)
	source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho", Mode: corev1alpha1.ConnectionModeReadOnly}}
	resolver := &KubernetesResolver{Reader: f.newReader().Build(), Connections: source}
	rejected := f.base
	rejected.Arguments = json.RawMessage(`"not an object"`)
	if _, err := resolver.Resolve(context.Background(), rejected); err == nil || !strings.Contains(err.Error(), "arguments rejected") {
		t.Fatalf("err = %v, want the arguments rejected", err)
	}
	if source.calls != 0 {
		t.Fatalf("credential source calls = %d, want none for rejected arguments", source.calls)
	}
	accepted := f.base
	accepted.Arguments = json.RawMessage(`{}`)
	if _, err := resolver.Resolve(context.Background(), accepted); err != nil || source.calls != 1 {
		t.Fatalf("accepted arguments err = %v calls = %d", err, source.calls)
	}
}

// TestKubernetesResolverConnectionModeRefusesUnboundDestinationsFirst covers
// Tools whose requests could not reach the exact curated destination: an
// MCP-backed Tool and a templated URL are refused before the credential
// source runs.
func TestKubernetesResolverConnectionModeRefusesUnboundDestinationsFirst(t *testing.T) {
	f := newConnectionModeFixture(t)
	source := &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "gho", Mode: corev1alpha1.ConnectionModeReadOnly}}
	resolver := &KubernetesResolver{Reader: f.newReader().Build(), Connections: source}
	mcp := f.base
	mcp.MCPBacked = true
	if _, err := resolver.Resolve(context.Background(), mcp); err == nil || !strings.Contains(err.Error(), "MCP-backed") {
		t.Fatalf("MCP-backed err = %v", err)
	}
	templated := f.base
	templated.Tool.URL = "https://api.github.com/search/issues?q={{q}}"
	if _, err := resolver.Resolve(context.Background(), templated); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("templated err = %v", err)
	}
	if source.calls != 0 {
		t.Fatalf("credential source calls = %d, want none", source.calls)
	}
}
