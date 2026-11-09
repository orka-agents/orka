package outboundaccess

import (
	"context"
	"errors"
	"strings"
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

	// The claim is granted again only when the Agent reconciles. Until then a
	// Task from before the current enforcement period, which may have been
	// created while the check was off, is still refused.
	taskCreated := agentIdentityRequest().AgentIdentity.TaskCreated
	for name, tc := range map[string]struct {
		since   time.Time
		started bool
		wantErr string
	}{
		"period began after the Task":       {since: taskCreated.Add(time.Minute), started: true, wantErr: "current period of Agent use enforcement"},
		"period began in the Task's second": {since: taskCreated, started: true, wantErr: "current period of Agent use enforcement"},
		"period not started yet":            {started: false, wantErr: "has not started yet"},
		"period began before the Task":      {since: taskCreated.Add(-time.Second), started: true},
		"check off, so no period":           {started: true},
	} {
		t.Run("enforcement "+name, func(t *testing.T) {
			resolver := reader()
			resolver.AgentIdentities = &fakeAgentIdentitySource{token: "t"}
			resolver.IdentityEnforcementStart = func() (time.Time, bool) { return tc.since, tc.started }
			_, err := resolver.Resolve(context.Background(), agentIdentityRequest())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Resolve() error = %v, want the call allowed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Resolve() error = %v, want %q", err, tc.wantErr)
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

// TestSelfModeRechecksTheAgentAfterIssuing covers the owner removing the
// identity or deleting the Agent while Entra answers: no credential is
// returned, so the executor never sends the identity-backed request.
func TestSelfModeRechecksTheAgentAfterIssuing(t *testing.T) {
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
