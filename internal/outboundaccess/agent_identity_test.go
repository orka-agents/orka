package outboundaccess

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type fakeAgentIdentitySource struct {
	token   string
	err     error
	request AgentIdentityTokenRequest
	// during runs while the token is issued, as a slow exchange would let
	// other writes land.
	during func()
}

func (f *fakeAgentIdentitySource) AgentIdentityToken(_ context.Context, req AgentIdentityTokenRequest) (string, error) {
	f.request = req
	if f.during != nil {
		f.during()
	}
	return f.token, f.err
}

// resolveOnlyConnectionSource cannot re-check a Connection.
type resolveOnlyConnectionSource struct{ source *fakeConnectionSource }

func (f resolveOnlyConnectionSource) ResolveConnectionCredential(ctx context.Context, req ConnectionCredentialRequest) (ConnectionCredential, error) {
	return f.source.ResolveConnectionCredential(ctx, req)
}

// endingConnectionSource ends the delegation while it refreshes the
// requester's token, as cancelling a parent Task during a slow refresh does.
type endingConnectionSource struct {
	*fakeConnectionSource
	end func()
}

func (f *endingConnectionSource) ResolveConnectionCredential(ctx context.Context, req ConnectionCredentialRequest) (ConnectionCredential, error) {
	f.end()
	return f.fakeConnectionSource.ResolveConnectionCredential(ctx, req)
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
			TaskCreated: testClaimGrantedAt.Add(time.Minute),
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
		"query": func(a *corev1alpha1.AgentIdentityOutboundAccess) {
			a.Entra.Resource = "https://graph.microsoft.com/?x=1"
		},
		"fragment": func(a *corev1alpha1.AgentIdentityOutboundAccess) { a.Entra.Resource = "api://orka-resource#part" },
		"no host":  func(a *corev1alpha1.AgentIdentityOutboundAccess) { a.Entra.Resource = "https://:123" },
		"no app":   func(a *corev1alpha1.AgentIdentityOutboundAccess) { a.Entra.Resource = "api:///path" },
		"userinfo": func(a *corev1alpha1.AgentIdentityOutboundAccess) {
			a.Entra.Resource = "https://user@graph.microsoft.com"
		},
		"bad port": func(a *corev1alpha1.AgentIdentityOutboundAccess) {
			a.Entra.Resource = "https://graph.microsoft.com:port"
		},
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

var testClaimGrantedAt = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func identityHolder(uid string, claimed bool) *corev1alpha1.Agent {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "reviewer", UID: types.UID(uid)},
		Spec: corev1alpha1.AgentSpec{Identity: &corev1alpha1.AgentIdentity{
			Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: "33333333-3333-3333-3333-33333333AAAA"},
		}},
	}
	if claimed {
		granted := metav1.NewTime(testClaimGrantedAt)
		agent.Status.IdentityClaim = &corev1alpha1.AgentIdentityClaim{
			Issuer: "entra", ID: "33333333-3333-3333-3333-33333333aaaa", GrantedAt: &granted,
		}
	}
	return agent
}

func withIdentityVerified(agent *corev1alpha1.Agent, status metav1.ConditionStatus, reason string) *corev1alpha1.Agent {
	agent.Status.Conditions = append(agent.Status.Conditions, metav1.Condition{
		Type: AgentConditionIdentityVerified, Status: status, Reason: reason, Message: "m", LastTransitionTime: metav1.Now(),
	})
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
		// The claim is marked releasing only when the Agent next
		// reconciles; the edit alone must stop the calls.
		"identity removed, claim not yet released": func() *corev1alpha1.Agent {
			agent := identityHolder("agent-uid", true)
			agent.Spec.Identity = nil
			return agent
		}(),
		// The claim was granted again after the Task was created, so the
		// Task's creation was never checked against this grant.
		"claim granted again after the Task was created": func() *corev1alpha1.Agent {
			agent := identityHolder("agent-uid", true)
			regranted := metav1.NewTime(testClaimGrantedAt.Add(time.Hour))
			agent.Status.IdentityClaim.GrantedAt = &regranted
			return agent
		}(),
		// Times have one-second precision, so a Task from the grant's own
		// second may predate the grant.
		"Task created in the grant's second": func() *corev1alpha1.Agent {
			agent := identityHolder("agent-uid", true)
			sameSecond := metav1.NewTime(testClaimGrantedAt.Add(time.Minute))
			agent.Status.IdentityClaim.GrantedAt = &sameSecond
			return agent
		}(),
		// A finalizer keeps a deleted Agent readable; deletion still stops
		// its Tasks' calls.
		"Agent being deleted": func() *corev1alpha1.Agent {
			agent := identityHolder("agent-uid", true)
			deleting := metav1.Now()
			agent.DeletionTimestamp = &deleting
			agent.Finalizers = []string{"example.com/hold"}
			return agent
		}(),
		// A definitive refusal at the last verification, such as a disabled
		// identity, stops calls even while a policy token is still cached.
		"issuer refused the identity": func() *corev1alpha1.Agent {
			return withIdentityVerified(identityHolder("agent-uid", true), metav1.ConditionFalse, AgentReasonIdentityVerificationFailed)
		}(),
		"claim without a grant time": func() *corev1alpha1.Agent {
			agent := identityHolder("agent-uid", true)
			agent.Status.IdentityClaim.GrantedAt = nil
			return agent
		}(),
		"identity changed, claim not yet released": func() *corev1alpha1.Agent {
			agent := identityHolder("agent-uid", true)
			agent.Spec.Identity.Entra.AgentIdentityID = "44444444-4444-4444-4444-444444444444"
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

	// The issuer's own answer decides calls while it is unreachable or the
	// identity is not verified yet.
	for name, agent := range map[string]*corev1alpha1.Agent{
		"issuer unavailable":   withIdentityVerified(identityHolder("agent-uid", true), metav1.ConditionUnknown, "IssuerUnavailable"),
		"not verified yet":     withIdentityVerified(identityHolder("agent-uid", true), metav1.ConditionFalse, "Declared"),
		"verified":             withIdentityVerified(identityHolder("agent-uid", true), metav1.ConditionTrue, "Verified"),
		"refused, now unknown": withIdentityVerified(identityHolder("agent-uid", true), metav1.ConditionUnknown, AgentReasonIdentityVerificationFailed),
	} {
		t.Run(name, func(t *testing.T) {
			resolver := resolverWith(agent)
			resolver.AgentIdentities = &fakeAgentIdentitySource{token: "t"}
			if _, err := resolver.Resolve(context.Background(), agentIdentityRequest()); err != nil {
				t.Fatalf("Resolve() error = %v, want the issuer to decide", err)
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

// TestKubernetesResolverDelegatedRechecksTheRequesterLink covers a requester
// who unlinks, or whose link is re-consented or stops being Ready, while the
// on-behalf-of exchange runs: the issued credential is never returned.
func TestKubernetesResolverDelegatedRechecksTheRequesterLink(t *testing.T) {
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

	t.Run("refuses a credential whose requester unlinked during the exchange", func(t *testing.T) {
		source := connection()
		identity := &fakeAgentIdentitySource{token: "obo-token", during: func() {
			source.liveErr = errors.New("the requester has no connection to this provider")
		}}
		resolution, err := resolver(delegatedPolicySpec(nil), source, identity).Resolve(context.Background(), delegatedRequest(now.Add(-time.Minute)))
		if !errors.Is(err, ErrRequesterLinkChanged) {
			t.Fatalf("Resolve() = %v, want the changed link refused", err)
		}
		if resolution.CredentialValue != "" || len(resolution.SensitiveValues) != 0 {
			t.Fatalf("no credential may be returned: %#v", resolution)
		}
		if len(source.liveChecks) != 1 || source.liveChecks[0].Frozen.UID != source.request.Frozen.UID ||
			source.liveChecks[0].Provider != source.request.Provider || source.liveChecks[0].Subject != source.request.Subject {
			t.Fatalf("re-check = %+v, want the Connection the exchange used (%+v)", source.liveChecks, source.request)
		}
	})

	t.Run("judges the delegation after the Connection re-check", func(t *testing.T) {
		for name, during := range map[string]func(*KubernetesResolver){
			"ancestor cancelled": func(r *KubernetesResolver) {
				r.DelegationLive = func(context.Context, AgentIdentityBinding) error { return errors.New("an ancestor Task was cancelled") }
			},
			"delegation expired": func(r *KubernetesResolver) {
				r.Now = func() time.Time { return now.Add(48 * time.Hour) }
			},
		} {
			source := connection()
			r := resolver(delegatedPolicySpec(nil), source, &fakeAgentIdentitySource{token: "obo-token"})
			source.duringLive = func() { during(r) }
			resolution, err := r.Resolve(context.Background(), delegatedRequest(now.Add(-time.Minute)))
			if err == nil || resolution.CredentialValue != "" {
				t.Fatalf("%s while the Connection was re-checked: Resolve() = %#v, %v; want a refusal", name, resolution, err)
			}
		}
	})

	t.Run("refuses when the source cannot re-check the Connection", func(t *testing.T) {
		r := resolver(delegatedPolicySpec(nil), nil, &fakeAgentIdentitySource{token: "obo-token"})
		r.Connections = resolveOnlyConnectionSource{connection()}
		if _, err := r.Resolve(context.Background(), delegatedRequest(now.Add(-time.Minute))); !errors.Is(err, ErrRequesterLinkChanged) {
			t.Fatalf("Resolve() = %v, want a fail-closed refusal", err)
		}
	})

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

	t.Run("refuses a delegation that expired during the exchange", func(t *testing.T) {
		identity := &fakeAgentIdentitySource{token: "obo-token"}
		r := resolver(delegatedPolicySpec(nil), nil, identity)
		r.Connections = connection()
		r.DelegationLive = func(context.Context, AgentIdentityBinding) error { return nil }
		start := now.Add(-time.Hour + time.Second)
		calls := 0
		// The first clock reading is before the exchange; the second, after
		// it, is past the policy's one-hour maxDelegationAge.
		r.Now = func() time.Time {
			if calls++; calls > 1 {
				return start.Add(time.Hour + time.Second)
			}
			return now
		}
		resolution, err := r.Resolve(context.Background(), delegatedRequest(start))
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("Resolve() = %v, want the expired delegation refused", err)
		}
		if resolution.CredentialValue != "" || len(resolution.SensitiveValues) != 0 {
			t.Fatalf("no credential may be returned: %#v", resolution)
		}
	})

	t.Run("refuses a delegation that ended during a slow refresh", func(t *testing.T) {
		var ended atomic.Bool
		refreshing := &endingConnectionSource{fakeConnectionSource: connection(), end: func() { ended.Store(true) }}
		identity := &fakeAgentIdentitySource{token: "obo-token"}
		r := resolver(delegatedPolicySpec(nil), nil, identity)
		r.Connections = refreshing
		r.DelegationLive = func(context.Context, AgentIdentityBinding) error {
			if ended.Load() {
				return errors.New("a Task it was delegated from was cancelled")
			}
			return nil
		}
		resolution, err := r.Resolve(context.Background(), delegatedRequest(now.Add(-time.Minute)))
		if err == nil || !strings.Contains(err.Error(), "ended while its credential was issued") {
			t.Fatalf("Resolve() = %v, want the ended delegation refused", err)
		}
		if resolution.CredentialValue != "" || len(resolution.SensitiveValues) != 0 {
			t.Fatalf("no credential may be returned: %#v", resolution)
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
		"no frozen Connection":     {spec: delegatedPolicySpec(nil), request: func(r *ResolveRequest) { r.FrozenConnections = nil }, want: "link to"},
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

// TestKubernetesResolverRechecksTheAgentBeforeReleasingAnIdentityCredential
// covers an Agent that loses its identity, or is deleted, while the issuer
// answers: the credential it issued is not released.
func TestKubernetesResolverRechecksTheAgentBeforeReleasingAnIdentityCredential(t *testing.T) {
	scheme := resolverScheme(t)
	for name, change := range map[string]func(context.Context, ctrlclient.Client, *corev1alpha1.Agent) error{
		"identity removed": func(ctx context.Context, c ctrlclient.Client, agent *corev1alpha1.Agent) error {
			agent.Spec.Identity = nil
			return c.Update(ctx, agent)
		},
		"agent deleted": func(ctx context.Context, c ctrlclient.Client, agent *corev1alpha1.Agent) error {
			return c.Delete(ctx, agent)
		},
	} {
		t.Run(name, func(t *testing.T) {
			reader := ctrlfake.NewClientBuilder().WithScheme(scheme).
				WithObjects(readyPolicy("agent", agentIdentityPolicySpec()), identityHolder("agent-uid", true)).
				WithStatusSubresource(&corev1alpha1.Agent{}).Build()
			identity := &fakeAgentIdentitySource{token: "agent-token", during: func() {
				agent := &corev1alpha1.Agent{}
				if err := reader.Get(context.Background(), ctrlclient.ObjectKey{Namespace: "tenant", Name: "reviewer"}, agent); err != nil {
					t.Fatal(err)
				}
				if err := change(context.Background(), reader, agent); err != nil {
					t.Fatal(err)
				}
			}}
			resolution, err := (&KubernetesResolver{Reader: reader, AgentIdentities: identity}).Resolve(context.Background(), agentIdentityRequest())
			if err == nil || resolution.CredentialValue != "" || len(resolution.SensitiveValues) != 0 {
				t.Fatalf("Resolve() = %#v, %v; want no credential once the Agent changed", resolution, err)
			}
		})
	}
}

// TestKubernetesResolverRechecksThePolicyBeforeReleasingAnIdentityCredential
// covers an outbound access policy revoked or edited while the issuer
// answers: the credential is released only under the policy as it now is.
func TestKubernetesResolverRechecksThePolicyBeforeReleasingAnIdentityCredential(t *testing.T) {
	scheme := resolverScheme(t)
	for name, change := range map[string]func(context.Context, ctrlclient.Client, *corev1alpha1.OutboundAccessPolicy) error{
		"policy deleted": func(ctx context.Context, c ctrlclient.Client, policy *corev1alpha1.OutboundAccessPolicy) error {
			return c.Delete(ctx, policy)
		},
		"policy edited": func(ctx context.Context, c ctrlclient.Client, policy *corev1alpha1.OutboundAccessPolicy) error {
			policy.Generation++
			return c.Update(ctx, policy)
		},
		"policy no longer accepted": func(ctx context.Context, c ctrlclient.Client, policy *corev1alpha1.OutboundAccessPolicy) error {
			for i := range policy.Status.Conditions {
				if policy.Status.Conditions[i].Type == corev1alpha1.OutboundAccessPolicyConditionAccepted {
					policy.Status.Conditions[i].Status = metav1.ConditionFalse
				}
			}
			return c.Status().Update(ctx, policy)
		},
	} {
		t.Run(name, func(t *testing.T) {
			policy := readyPolicy("agent", agentIdentityPolicySpec())
			reader := ctrlfake.NewClientBuilder().WithScheme(scheme).
				WithObjects(policy, identityHolder("agent-uid", true)).
				WithStatusSubresource(&corev1alpha1.Agent{}, &corev1alpha1.OutboundAccessPolicy{}).Build()
			identity := &fakeAgentIdentitySource{token: "agent-token", during: func() {
				live := &corev1alpha1.OutboundAccessPolicy{}
				if err := reader.Get(context.Background(), ctrlclient.ObjectKeyFromObject(policy), live); err != nil {
					t.Fatal(err)
				}
				if err := change(context.Background(), reader, live); err != nil {
					t.Fatal(err)
				}
			}}
			resolution, err := (&KubernetesResolver{Reader: reader, AgentIdentities: identity}).Resolve(context.Background(), agentIdentityRequest())
			if err == nil || resolution.CredentialValue != "" || len(resolution.SensitiveValues) != 0 {
				t.Fatalf("Resolve() = %#v, %v; want no credential once the policy changed", resolution, err)
			}
		})
	}
	// An unchanged policy still releases it.
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(readyPolicy("agent", agentIdentityPolicySpec()), identityHolder("agent-uid", true)).
		WithStatusSubresource(&corev1alpha1.Agent{}, &corev1alpha1.OutboundAccessPolicy{}).Build()
	resolution, err := (&KubernetesResolver{Reader: reader, AgentIdentities: &fakeAgentIdentitySource{token: "agent-token"}}).Resolve(context.Background(), agentIdentityRequest())
	if err != nil || resolution.CredentialValue == "" {
		t.Fatalf("an unchanged policy: Resolve() = %#v, %v", resolution, err)
	}
}
