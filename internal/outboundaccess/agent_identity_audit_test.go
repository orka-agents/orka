package outboundaccess

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestAgentIdentityCallsWriteUseRecords(t *testing.T) {
	scheme := resolverScheme(t)
	now := time.Now()
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(readyPolicy("agent", delegatedPolicySpec(nil)), acceptedProvider(), identityHolder("agent-uid", true)).Build()
	resolver := &KubernetesResolver{
		Reader: reader, AuditLog: logger, AuditPseudonymKey: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return now },
		Connections:     &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "user-token", ConnectionUID: "uid-1", Mode: "readOnly"}},
		AgentIdentities: &fakeAgentIdentitySource{token: "obo-token"},
		DelegationLive:  func(context.Context, AgentIdentityBinding) error { return nil },
	}

	if _, err := resolver.Resolve(context.Background(), delegatedRequest(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	expired := delegatedRequest(now.Add(-2 * time.Hour))
	if _, err := resolver.Resolve(context.Background(), expired); err == nil {
		t.Fatal("an expired delegation must be refused")
	}
	if len(lines) != 2 {
		t.Fatalf("records = %d, want one per call: %v", len(lines), lines)
	}
	issued, refused := lines[0], lines[1]
	for _, want := range []string{`"outcome"="issued"`, `"agentUID"="agent-uid"`, `"identityID"="33333333-3333-3333-3333-33333333aaaa"`,
		`"mode"="Delegated"`, `"tool"="gh_search"`, `"policyName"="agent"`, `"requester"="hmac-sha256:`, `"argumentsDigest"="sha256:`} {
		if !strings.Contains(issued, want) {
			t.Fatalf("issued record lacks %s: %s", want, issued)
		}
	}
	if !strings.Contains(refused, `"outcome"="refused"`) || !strings.Contains(refused, `"reason"="delegation"`) {
		t.Fatalf("refused record = %s", refused)
	}
	for _, line := range lines {
		for _, secret := range []string{"user-token", "obo-token", "alice", "https://issuer.example.test"} {
			if strings.Contains(line, secret) {
				t.Fatalf("record leaks %q: %s", secret, line)
			}
		}
	}
	pseudonym := func(line string) string {
		_, after, _ := strings.Cut(line, `"requester"="`)
		value, _, _ := strings.Cut(after, `"`)
		return value
	}
	if pseudonym(issued) == "" || pseudonym(issued) != pseudonym(refused) {
		t.Fatal("one requester must map to one stable pseudonym")
	}
}

// auditedResolver returns a Delegated-mode resolver whose identity use
// records are captured in lines.
func auditedResolver(t *testing.T, now time.Time, source AgentIdentityTokenSource, lines *[]string) *KubernetesResolver {
	t.Helper()
	reader := ctrlfake.NewClientBuilder().WithScheme(resolverScheme(t)).
		WithObjects(readyPolicy("agent", delegatedPolicySpec(nil)), acceptedProvider(), identityHolder("agent-uid", true),
			readyPolicy("plain", connectionPolicySpec("github"))).Build()
	return &KubernetesResolver{
		Reader: reader, AuditPseudonymKey: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return now },
		AuditLog:        funcr.New(func(_, args string) { *lines = append(*lines, args) }, funcr.Options{}),
		Connections:     &fakeConnectionSource{credential: ConnectionCredential{AccessToken: "user-assertion-secret", ConnectionUID: "uid-1", Mode: "readOnly"}},
		AgentIdentities: source,
		DelegationLive:  func(context.Context, AgentIdentityBinding) error { return nil },
	}
}

func TestAgentIdentityUseRecordsCarryOnlyAClassifiedReason(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		source  AgentIdentityTokenSource
		request func(*ResolveRequest)
		reason  string
		leaks   []string
	}{
		"argument values in the validation error": {
			source:  &fakeAgentIdentitySource{token: "t"},
			request: func(r *ResolveRequest) { r.Arguments = []byte(`{"q":9007199254740993}`) },
			reason:  agentIdentityRefusalDelegation, leaks: []string{"9007199254740993"},
		},
		"issuer error quoting the assertion": {
			source: &fakeAgentIdentitySource{err: errors.New("AADSTS50013: assertion user-assertion-secret rejected")},
			reason: agentIdentityRefusalIssuer, leaks: []string{"user-assertion-secret", "AADSTS50013"},
		},
		"empty credential": {source: &fakeAgentIdentitySource{}, reason: agentIdentityRefusalIssuer},
		"MCP-backed Tool": {
			source: &fakeAgentIdentitySource{token: "t"}, request: func(r *ResolveRequest) { r.MCPBacked = true },
			reason: agentIdentityRefusalBinding, leaks: []string{"MCP"},
		},
		"Task created before the identity grant": {
			source:  &fakeAgentIdentitySource{token: "t"},
			request: func(r *ResolveRequest) { r.AgentIdentity.TaskCreated = testClaimGrantedAt.Add(-time.Minute) },
			reason:  agentIdentityRefusalBinding,
		},
		"canceled": {
			source: &fakeAgentIdentitySource{err: context.Canceled},
			reason: agentIdentityRefusalCanceled,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var lines []string
			resolver := auditedResolver(t, now, tc.source, &lines)
			request := delegatedRequest(now.Add(-time.Minute))
			if tc.request != nil {
				tc.request(&request)
			}
			_, err := resolver.Resolve(context.Background(), request)
			if err == nil {
				t.Fatal("Resolve() = nil error, want the call refused")
			}
			if len(lines) != 1 {
				t.Fatalf("records = %d, want exactly one: %v", len(lines), lines)
			}
			if want := `"reason"="` + tc.reason + `"`; !strings.Contains(lines[0], want) {
				t.Fatalf("record lacks %s: %s", want, lines[0])
			}
			for _, leak := range append(tc.leaks, err.Error()) {
				if strings.Contains(lines[0], leak) {
					t.Fatalf("record carries %q: %s", leak, lines[0])
				}
			}
		})
	}
}

func TestAgentIdentityDelegationEndedWhileIssuingIsADelegationRefusal(t *testing.T) {
	now := time.Now()
	var lines []string
	resolver := auditedResolver(t, now, &fakeAgentIdentitySource{token: "obo-token"}, &lines)
	checks := 0
	resolver.DelegationLive = func(context.Context, AgentIdentityBinding) error {
		if checks++; checks > 1 {
			return errors.New("a Task it was delegated from was cancelled")
		}
		return nil
	}
	if _, err := resolver.Resolve(context.Background(), delegatedRequest(now.Add(-time.Minute))); err == nil {
		t.Fatal("Resolve() = nil error, want the call refused")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"reason"="`+agentIdentityRefusalDelegation+`"`) || strings.Contains(lines[0], "obo-token") {
		t.Fatalf("records = %v, want one delegation refusal without the credential", lines)
	}
}

func TestAgentIdentityRefusalsBeforeDispatchAreRecordedOnce(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*ResolveRequest){
		"plain http Tool":  func(r *ResolveRequest) { r.TargetScheme = "http" },
		"authSecretRef":    func(r *ResolveRequest) { r.HasAuthSecretRef = true },
		"policy changed":   func(r *ResolveRequest) { r.CheckedPolicy = &PolicyIdentity{UID: "other", Generation: 3} },
		"stale generation": func(r *ResolveRequest) { r.CheckedPolicy = &PolicyIdentity{Generation: 1} },
	} {
		t.Run(name, func(t *testing.T) {
			var lines []string
			source := &fakeAgentIdentitySource{token: "t"}
			resolver := auditedResolver(t, now, source, &lines)
			request := delegatedRequest(now.Add(-time.Minute))
			mutate(&request)
			if _, err := resolver.Resolve(context.Background(), request); err == nil {
				t.Fatal("Resolve() = nil error, want the call refused")
			}
			if len(lines) != 1 || !strings.Contains(lines[0], `"outcome"="refused"`) ||
				!strings.Contains(lines[0], `"reason"="`+agentIdentityRefusalPolicy+`"`) || !strings.Contains(lines[0], `"policyName"="agent"`) {
				t.Fatalf("records = %v, want one policy refusal", lines)
			}
			if source.request.Identity.ID != "" {
				t.Fatal("the issuer was asked for a credential")
			}
		})
	}

	t.Run("policies without an identity leave no record", func(t *testing.T) {
		var lines []string
		resolver := auditedResolver(t, now, &fakeAgentIdentitySource{token: "t"}, &lines)
		request := delegatedRequest(now.Add(-time.Minute))
		request.PolicyName, request.TargetScheme = "plain", "http"
		if _, err := resolver.Resolve(context.Background(), request); err == nil {
			t.Fatal("Resolve() = nil error, want the call refused")
		}
		if len(lines) != 0 {
			t.Fatalf("records = %v, want none", lines)
		}
	})
}

func TestAgentIdentityRefusalWithoutBindingNamesTheTask(t *testing.T) {
	now := time.Now()
	var lines []string
	resolver := auditedResolver(t, now, &fakeAgentIdentitySource{token: "t"}, &lines)
	request := delegatedRequest(now.Add(-time.Minute))
	request.AgentIdentity, request.TaskUID = nil, "plain-task-uid"
	if _, err := resolver.Resolve(context.Background(), request); err == nil {
		t.Fatal("Resolve() = nil error, want the call refused")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"taskUID"="plain-task-uid"`) ||
		!strings.Contains(lines[0], `"reason"="`+agentIdentityRefusalBinding+`"`) {
		t.Fatalf("records = %v, want one binding refusal naming the Task", lines)
	}
}

func TestRequesterPseudonymKeepsOpaqueClaimsExact(t *testing.T) {
	resolver := &KubernetesResolver{AuditPseudonymKey: []byte(strings.Repeat("k", 32))}
	pseudonym := func(issuer, subject string) string {
		return resolver.requesterPseudonym(&corev1alpha1.RequestedBy{Issuer: issuer, Subject: subject})
	}
	base := pseudonym("https://issuer.example.test", "alice")
	if base == "" || base != pseudonym("https://issuer.example.test", "alice") {
		t.Fatal("one requester must map to one stable pseudonym")
	}
	for _, other := range []string{pseudonym("https://issuer.example.test", " alice "), pseudonym(" https://issuer.example.test", "alice")} {
		if other == base {
			t.Fatal("distinct verified claims must not share a pseudonym")
		}
	}
}

// A resolver built like an AI worker's (no AuditLog, no identity source, no
// process logger) still writes exactly one record for a refused call.
func TestAgentIdentityRefusalWithoutAuditLogIsRecorded(t *testing.T) {
	var output bytes.Buffer
	previous := agentIdentityAuditOutput
	agentIdentityAuditOutput = &output
	t.Cleanup(func() { agentIdentityAuditOutput = previous })
	reader := ctrlfake.NewClientBuilder().WithScheme(resolverScheme(t)).
		WithObjects(readyPolicy("agent", agentIdentityPolicySpec()), identityHolder("agent-uid", true)).Build()
	resolver := &KubernetesResolver{Reader: reader}
	if _, err := resolver.Resolve(context.Background(), agentIdentityRequest()); err == nil {
		t.Fatal("Resolve() = nil error, want the call refused outside the controller")
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("records = %q, want exactly one", output.String())
	}
	for _, want := range []string{`"logger":"agent-identity-audit"`, `"outcome":"refused"`,
		`"reason":"` + agentIdentityRefusalBinding + `"`, `"mode":"Self"`, `"taskUID":"task-uid"`} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("record lacks %s: %s", want, lines[0])
		}
	}
}
