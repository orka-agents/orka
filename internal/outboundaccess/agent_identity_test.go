package outboundaccess

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type fakeAgentIdentitySource struct {
	token   string
	err     error
	request AgentIdentityTokenRequest
}

func (f *fakeAgentIdentitySource) AgentIdentityToken(_ context.Context, req AgentIdentityTokenRequest) (string, error) {
	f.request = req
	return f.token, f.err
}

func agentIdentityPolicySpec() corev1alpha1.OutboundAccessPolicySpec {
	return corev1alpha1.OutboundAccessPolicySpec{AgentIdentity: &corev1alpha1.AgentIdentityOutboundAccess{
		Entra: &corev1alpha1.EntraOutboundTarget{Resource: "https://graph.microsoft.com"},
	}}
}

func agentIdentityRequest() ResolveRequest {
	return ResolveRequest{
		Namespace: "tenant", PolicyName: "agent", TargetScheme: "https",
		Tool: ToolBinding{Name: "graph_me", URL: "https://graph.microsoft.com/v1.0/me", Method: "GET"},
		AgentIdentity: &AgentIdentityBinding{
			Identity:       corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: "33333333-3333-3333-3333-33333333aaaa", Instance: "entra:t/b"},
			AgentNamespace: "tenant", AgentName: "reviewer", AgentUID: "agent-uid", AgentGeneration: 2, TaskUID: "task-uid",
		},
	}
}

func TestValidateSpecAgentIdentityMode(t *testing.T) {
	policy := readyPolicy("agent", agentIdentityPolicySpec())
	if issue := ValidateSpec(policy); issue != nil {
		t.Fatalf("valid policy: %v", issue)
	}
	for name, mutate := range map[string]func(*corev1alpha1.AgentIdentityOutboundAccess){
		"unknown mode":     func(a *corev1alpha1.AgentIdentityOutboundAccess) { a.Mode = "Delegated" },
		"no issuer target": func(a *corev1alpha1.AgentIdentityOutboundAccess) { a.Entra = nil },
		"scope suffix": func(a *corev1alpha1.AgentIdentityOutboundAccess) {
			a.Entra.Resource = "https://graph.microsoft.com/.default"
		},
		"plain http": func(a *corev1alpha1.AgentIdentityOutboundAccess) { a.Entra.Resource = "http://graph.microsoft.com" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := agentIdentityPolicySpec()
			mutate(spec.AgentIdentity)
			if issue := ValidateSpec(readyPolicy("agent", spec)); issue == nil {
				t.Fatal("ValidateSpec() = nil, want an issue")
			}
		})
	}
	both := agentIdentityPolicySpec()
	both.Connection = &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}}
	if issue := ValidateSpec(readyPolicy("agent", both)); issue == nil || !strings.Contains(issue.Message, "agentIdentity") {
		t.Fatalf("two adapters: %v", issue)
	}
}

func TestValidateSpecReservesEntraIdentityParameters(t *testing.T) {
	for _, name := range []string{"fmi_path", "user_id", "username", "user_federated_identity_credential"} {
		spec := corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
			Grant:                   corev1alpha1.OutboundGrantTokenExchange,
			TokenEndpoint:           corev1alpha1.OutboundTokenEndpoint{URL: "https://sts.example.com/token"},
			Subject:                 corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken},
			ExpectedIssuedTokenType: "urn:ietf:params:oauth:token-type:access_token",
			AdditionalParameters:    map[string]string{name: "x"},
		}}
		if issue := ValidateSpec(readyPolicy("direct", spec)); issue == nil || !strings.Contains(issue.Message, "reserved") {
			t.Fatalf("%s: issue = %v, want it reserved", name, issue)
		}
	}
}

func identityHolder(uid string, claimed bool) *corev1alpha1.Agent {
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "reviewer", UID: types.UID(uid)}}
	if claimed {
		agent.Status.IdentityClaim = &corev1alpha1.AgentIdentityClaim{Issuer: "entra", ID: "33333333-3333-3333-3333-33333333aaaa"}
	}
	return agent
}

func TestKubernetesResolverAgentIdentityMode(t *testing.T) {
	scheme := resolverScheme(t)
	resolverWith := func(agent *corev1alpha1.Agent) *KubernetesResolver {
		builder := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(readyPolicy("agent", agentIdentityPolicySpec()))
		if agent != nil {
			builder = builder.WithObjects(agent)
		}
		return &KubernetesResolver{Reader: builder.Build()}
	}
	reader := func() *KubernetesResolver { return resolverWith(identityHolder("agent-uid", true)) }

	for name, agent := range map[string]*corev1alpha1.Agent{
		"Agent deleted":           nil,
		"Agent recreated":         identityHolder("other-uid", true),
		"claim moved or released": identityHolder("agent-uid", false),
		"identity being released": func() *corev1alpha1.Agent {
			agent := identityHolder("agent-uid", true)
			agent.Status.IdentityClaim.Releasing = true
			return agent
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			resolver := resolverWith(agent)
			resolver.AgentIdentities = &fakeAgentIdentitySource{token: "t"}
			if _, err := resolver.Resolve(context.Background(), agentIdentityRequest()); err == nil {
				t.Fatal("Resolve() = nil error, want the call refused")
			}
		})
	}

	t.Run("injects the identity's token for the policy target", func(t *testing.T) {
		resolver := reader()
		source := &fakeAgentIdentitySource{token: "agent-token"}
		resolver.AgentIdentities = source
		resolution, err := resolver.Resolve(context.Background(), agentIdentityRequest())
		if err != nil {
			t.Fatal(err)
		}
		if resolution.Adapter != AdapterAgentIdentity || resolution.CredentialHeader != "Authorization" ||
			resolution.CredentialValue != "Bearer agent-token" || len(resolution.SensitiveValues) != 1 {
			t.Fatalf("resolution = %#v", resolution)
		}
		if source.request.Identity.ID != "33333333-3333-3333-3333-33333333aaaa" || source.request.Resource != "https://graph.microsoft.com" ||
			source.request.Mode != corev1alpha1.OutboundAgentIdentityModeSelf || source.request.Policy.Generation != 3 {
			t.Fatalf("source request = %+v", source.request)
		}
	})

	for name, tc := range map[string]struct {
		source  AgentIdentityTokenSource
		request func(*ResolveRequest)
		want    string
	}{
		"no source in this process": {want: "only by the controller"},
		"Task without an identity":  {source: &fakeAgentIdentitySource{token: "t"}, request: func(r *ResolveRequest) { r.AgentIdentity = nil }, want: "Agent with an identity"},
		"other issuer":              {source: &fakeAgentIdentitySource{token: "t"}, request: func(r *ResolveRequest) { r.AgentIdentity.Identity.Issuer = "okta" }, want: "no longer holds its identity"},
		"templated Tool URL":        {source: &fakeAgentIdentitySource{token: "t"}, request: func(r *ResolveRequest) { r.Tool.URL = "https://{{host}}/x" }, want: "template"},
		"MCP-backed Tool":           {source: &fakeAgentIdentitySource{token: "t"}, request: func(r *ResolveRequest) { r.MCPBacked = true }, want: "MCP-backed"},
		"plain http Tool":           {source: &fakeAgentIdentitySource{token: "t"}, request: func(r *ResolveRequest) { r.TargetScheme = "http" }, want: "HTTPS"},
		"issuer refusal":            {source: &fakeAgentIdentitySource{err: errors.New("denied")}, want: "issue agent identity credential"},
		"empty token":               {source: &fakeAgentIdentitySource{}, want: "empty credential"},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := reader()
			resolver.AgentIdentities = tc.source
			request := agentIdentityRequest()
			if tc.request != nil {
				tc.request(&request)
			}
			if _, err := resolver.Resolve(context.Background(), request); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func delegatedPolicySpec(age *metav1.Duration) corev1alpha1.OutboundAccessPolicySpec {
	spec := agentIdentityPolicySpec()
	spec.AgentIdentity.Mode = corev1alpha1.OutboundAgentIdentityModeDelegated
	spec.AgentIdentity.Delegated = &corev1alpha1.DelegatedAgentIdentity{
		ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, MaxDelegationAge: age,
	}
	return spec
}

func delegatedRequest(started time.Time) ResolveRequest {
	f := ResolveRequest{
		Namespace: "tenant", PolicyName: "agent", TargetScheme: "https",
		Requester:         &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"},
		FrozenConnections: map[string]FrozenConnection{"agent": {UID: "uid-1", Generation: 2}},
		Tool:              ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead},
		AgentIdentity:     agentIdentityRequest().AgentIdentity,
	}
	f.AgentIdentity.DelegationStart = started
	f.AgentIdentity.ManagedEgress = true
	return f
}

func TestValidateSpecDelegatedAgentIdentity(t *testing.T) {
	if issue := ValidateSpec(readyPolicy("agent", delegatedPolicySpec(nil))); issue != nil {
		t.Fatalf("valid delegated policy: %v", issue)
	}
	missing := delegatedPolicySpec(nil)
	missing.AgentIdentity.Delegated = nil
	if issue := ValidateSpec(readyPolicy("agent", missing)); issue == nil {
		t.Fatal("Delegated mode without delegated must be invalid")
	}
	stray := agentIdentityPolicySpec()
	stray.AgentIdentity.Delegated = &corev1alpha1.DelegatedAgentIdentity{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}}
	if issue := ValidateSpec(readyPolicy("agent", stray)); issue == nil {
		t.Fatal("delegated with mode Self must be invalid")
	}
	if name, ok := ConnectionProviderName(readyPolicy("agent", delegatedPolicySpec(nil))); !ok || name != "github" {
		t.Fatalf("ConnectionProviderName() = %q, %v", name, ok)
	}
}

func TestKubernetesResolverDelegatedAgentIdentityMode(t *testing.T) {
	scheme := resolverScheme(t)
	now := time.Now()
	resolver := func(spec corev1alpha1.OutboundAccessPolicySpec, connection *fakeConnectionSource, identity *fakeAgentIdentitySource) *KubernetesResolver {
		reader := ctrlfake.NewClientBuilder().WithScheme(scheme).
			WithObjects(readyPolicy("agent", spec), acceptedProvider(), identityHolder("agent-uid", true)).Build()
		return &KubernetesResolver{
			Reader: reader, Connections: connection, AgentIdentities: identity, Now: func() time.Time { return now },
			DelegationLive: func(context.Context, AgentIdentityBinding) error { return nil },
		}
	}
	connection := func() *fakeConnectionSource {
		return &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "user-token", ConnectionUID: "uid-1", Generation: 2, Mode: "readOnly"}}
	}

	t.Run("exchanges the requester's token for the Agent's", func(t *testing.T) {
		identity := &fakeAgentIdentitySource{token: "obo-token"}
		resolution, err := resolver(delegatedPolicySpec(nil), connection(), identity).Resolve(context.Background(), delegatedRequest(now.Add(-time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		if resolution.CredentialValue != "Bearer obo-token" || resolution.ConnectionUID != "uid-1" {
			t.Fatalf("resolution = %#v", resolution)
		}
		if identity.request.UserAssertion != "user-token" || identity.request.Mode != corev1alpha1.OutboundAgentIdentityModeDelegated {
			t.Fatalf("token request = %+v", identity.request)
		}
		if len(resolution.SensitiveValues) != 2 {
			t.Fatalf("both tokens must be redacted: %v", resolution.SensitiveValues)
		}
	})

	for name, tc := range map[string]struct {
		spec    corev1alpha1.OutboundAccessPolicySpec
		request func(*ResolveRequest)
		cap     time.Duration
		noLive  bool
		live    error
		want    string
	}{
		"runtime without managed egress":  {spec: delegatedPolicySpec(nil), request: func(r *ResolveRequest) { r.AgentIdentity.ManagedEgress = false }, want: "built-in RuntimePools"},
		"delegation past the default age": {spec: delegatedPolicySpec(nil), request: func(r *ResolveRequest) { r.AgentIdentity.DelegationStart = now.Add(-2 * time.Hour) }, want: "expired"},
		"delegation with no known start":  {spec: delegatedPolicySpec(nil), request: func(r *ResolveRequest) { r.AgentIdentity.DelegationStart = time.Time{} }, want: "expired"},
		"policy age above the cap": {
			spec:    delegatedPolicySpec(&metav1.Duration{Duration: 48 * time.Hour}),
			request: func(r *ResolveRequest) { r.AgentIdentity.DelegationStart = now.Add(-3 * time.Hour) },
			cap:     2 * time.Hour, want: "expired",
		},
		"no verified requester":    {spec: delegatedPolicySpec(nil), request: func(r *ResolveRequest) { r.Requester = nil }, want: "verified requester"},
		"no frozen Connection":     {spec: delegatedPolicySpec(nil), request: func(r *ResolveRequest) { r.FrozenConnections = nil }, want: "frozen"},
		"tool the provider lacks":  {spec: delegatedPolicySpec(nil), request: func(r *ResolveRequest) { r.Tool.Name = "other_tool" }, want: "tool"},
		"no liveness check":        {spec: delegatedPolicySpec(nil), noLive: true, want: "Task and Session check"},
		"Task or Session not live": {spec: delegatedPolicySpec(nil), live: errors.New("the Task's Session was cancelled"), want: "no longer live"},
	} {
		t.Run(name, func(t *testing.T) {
			r := resolver(tc.spec, connection(), &fakeAgentIdentitySource{token: "t"})
			if tc.noLive {
				r.DelegationLive = nil
			} else if tc.live != nil {
				live := tc.live
				r.DelegationLive = func(context.Context, AgentIdentityBinding) error { return live }
			}
			r.MaxDelegationAge = tc.cap
			request := delegatedRequest(now.Add(-time.Minute))
			if tc.request != nil {
				tc.request(&request)
			}
			if _, err := r.Resolve(context.Background(), request); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEffectiveMaxDelegationAge(t *testing.T) {
	for name, tc := range map[string]struct {
		age           *metav1.Duration
		limit         time.Duration
		want, wantAsk time.Duration
	}{
		"default age":                {want: DefaultMaxDelegationAge, wantAsk: DefaultMaxDelegationAge},
		"own age under the cap":      {age: &metav1.Duration{Duration: 2 * time.Hour}, want: 2 * time.Hour, wantAsk: 2 * time.Hour},
		"own age over the default":   {age: &metav1.Duration{Duration: 48 * time.Hour}, want: DefaultMaxDelegationAgeCap, wantAsk: 48 * time.Hour},
		"own age over a set cap":     {age: &metav1.Duration{Duration: 2 * time.Hour}, limit: time.Hour, want: time.Hour, wantAsk: 2 * time.Hour},
		"default age over a set cap": {limit: 10 * time.Minute, want: 10 * time.Minute, wantAsk: DefaultMaxDelegationAge},
	} {
		t.Run(name, func(t *testing.T) {
			spec := delegatedPolicySpec(tc.age)
			got, asked := EffectiveMaxDelegationAge(&corev1alpha1.OutboundAccessPolicy{Spec: spec}, tc.limit)
			if got != tc.want || asked != tc.wantAsk {
				t.Fatalf("EffectiveMaxDelegationAge() = %s, %s; want %s, %s", got, asked, tc.want, tc.wantAsk)
			}
		})
	}
}
