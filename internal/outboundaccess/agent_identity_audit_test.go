package outboundaccess

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	if !strings.Contains(refused, `"outcome"="refused"`) || !strings.Contains(refused, "expired") {
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
