package workspace

import (
	"context"
	"strings"
	"testing"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/protobuf/proto"
)

func githubCredential() *SubstrateEgressCredential {
	return &SubstrateEgressCredential{Header: "Authorization", Prefix: "Bearer ", Provider: SubstrateKubernetesSecretsCredentialProvider, Namespace: "team-a", Name: "github-token", Key: "token"}
}

func TestCompileSubstrateActorEgressCredentialsReferenceSecrets(t *testing.T) {
	policy, err := CompileSubstrateActorEgressPolicy("tools", []SubstrateEgressRule{
		{Scheme: "https", Host: "api.github.com", Credential: githubCredential()},
		{Scheme: "http", Host: "mirror.example.org"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &ateapipb.HttpRuleEffects{ReplaceHeaders: []*ateapipb.CredentialHeader{{Header: "Authorization", Prefix: "Bearer ", CredentialUri: "ate-secret://k8s.io/default/team-a/github-token/token"}}}
	if got := policy.GetRules()[0].GetHttps().GetEffects(); !proto.Equal(got, want) {
		t.Fatalf("effects = %v, want %v", got, want)
	}
	if policy.GetRules()[1].GetHttp().GetEffects() != nil {
		t.Fatal("rule without a credential gained gateway effects")
	}
}

func TestSubstrateActorEgressCredentialsFailClosed(t *testing.T) {
	for name, mutate := range map[string]func(*SubstrateEgressRule){
		"not configured":   func(r *SubstrateEgressRule) { r.Credential.Provider = "" },
		"other provider":   func(r *SubstrateEgressRule) { r.Credential.Provider = "vault.example.com" },
		"tls passthrough":  func(r *SubstrateEgressRule) { r.Scheme, r.Ports = "tls", []int32{443} },
		"wildcard host":    func(r *SubstrateEgressRule) { r.Host = "*.github.com" },
		"cookie header":    func(r *SubstrateEgressRule) { r.Credential.Header = "Cookie" },
		"hop-by-hop":       func(r *SubstrateEgressRule) { r.Credential.Header = "Connection" },
		"control prefix":   func(r *SubstrateEgressRule) { r.Credential.Prefix = "Bearer\n" },
		"missing secret":   func(r *SubstrateEgressRule) { r.Credential.Name = "" },
		"invalid key":      func(r *SubstrateEgressRule) { r.Credential.Key = "a/b" },
		"invalid namespace": func(r *SubstrateEgressRule) { r.Credential.Namespace = "Team_A" },
	} {
		t.Run(name, func(t *testing.T) {
			rule := SubstrateEgressRule{Scheme: "https", Host: "api.github.com", Credential: githubCredential()}
			mutate(&rule)
			if _, err := CompileSubstrateActorEgressPolicy("tools", []SubstrateEgressRule{rule}); err == nil {
				t.Fatal("credential rule compiled")
			}
		})
	}
}

func TestEnsureSubstrateActorEgressPolicyReplacesChangedCredentialEffects(t *testing.T) {
	existing, err := CompileSubstrateActorEgressPolicy("tools", []SubstrateEgressRule{{Scheme: "https", Host: "api.github.com", Credential: githubCredential()}})
	if err != nil {
		t.Fatal(err)
	}
	existing.Metadata.Uid, existing.Metadata.Version = "uid-1", 1
	control := &fakeEgressControl{policy: existing, nextVersion: 1}
	rotated := githubCredential()
	rotated.Name = "github-token-v2"
	if err := ensureSubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", []SubstrateEgressRule{{Scheme: "https", Host: "api.github.com", Credential: rotated}}); err != nil {
		t.Fatal(err)
	}
	if control.updates != 1 || !strings.Contains(control.policy.GetRules()[0].GetHttps().GetEffects().GetReplaceHeaders()[0].GetCredentialUri(), "github-token-v2") {
		t.Fatal("a changed credential reference was not re-applied")
	}
	// Dropping the credential also replaces the rule rather than leaving
	// the gateway injecting a Secret the Tool no longer names.
	if err := ensureSubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", []SubstrateEgressRule{{Scheme: "https", Host: "api.github.com"}}); err != nil {
		t.Fatal(err)
	}
	if control.updates != 2 || control.policy.GetRules()[0].GetHttps().GetEffects() != nil {
		t.Fatal("a removed credential reference survived re-application")
	}
}
