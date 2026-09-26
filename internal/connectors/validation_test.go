/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func validProvider() *corev1alpha1.ConnectorProvider {
	return &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "tenant", Generation: 1},
		Spec: corev1alpha1.ConnectorProviderSpec{
			OAuth: corev1alpha1.ConnectorOAuthConfig{
				AuthorizeURL:    "https://github.com/login/oauth/authorize",
				TokenURL:        "https://github.com/login/oauth/access_token",
				ClientID:        "client",
				ClientSecretRef: corev1alpha1.SecretKeySelector{Name: "oauth", Key: "clientSecret"},
				Scopes:          corev1alpha1.ConnectorScopes{Read: []string{"read:user"}, Write: []string{"repo"}},
			},
			Tools: []corev1alpha1.ConnectorTool{
				{Name: "list_pull_requests", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin},
				{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin},
				{
					Name: "search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP,
					Description: "Search",
					HTTP:        &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET"},
				},
			},
		},
	}
}

func TestValidateProviderSpec(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*corev1alpha1.ConnectorProvider)
		builtin BuiltinToolCheck
		want    string
	}{
		{name: "valid"},
		{name: "nil provider", mutate: nil, want: ""},
		{name: "http authorize url", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.AuthorizeURL = "http://github.com/authorize" }, want: "authorizeURL must be an absolute HTTPS URL"},
		{name: "userinfo in token url", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://user:pw@github.com/token" }, want: "tokenURL must be an absolute HTTPS URL"},
		{name: "fragment in token url", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://github.com/token#frag" }, want: "tokenURL must be an absolute HTTPS URL"},
		{name: "whitespace url", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = " https://github.com/token" }, want: "surrounding whitespace"},
		{name: "loopback ip", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://127.0.0.1/token" }, want: "must not target private"},
		{name: "private ip", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.AuthorizeURL = "https://10.0.0.5/authorize" }, want: "must not target private"},
		{name: "metadata host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.RevocationURL = "https://metadata.google.internal/revoke"
		}, want: "revocationURL host is not allowed"},
		{name: "kubernetes host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://kubernetes.default.svc/token"
		}, want: "tokenURL host is not allowed"},
		{name: "localhost", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://localhost/token" }, want: "tokenURL host is not allowed"},
		{name: "trailing dot metadata host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://metadata.google.internal./token"
		}, want: "must not be empty, end with a dot, or carry an IPv6 zone"},
		{name: "trailing dot public host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.AuthorizeURL = "https://github.com./authorize" }, want: "must not be empty, end with a dot, or carry an IPv6 zone"},
		{name: "ipv6 zone literal", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://[fe80::1%25eth0]/token" }, want: "carry an IPv6 zone"},
		{name: "ipv6 link-local literal", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://[fe80::1]/token" }, want: "must not target private"},
		{name: "ipv6 loopback literal", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.AuthorizeURL = "https://[::1]/authorize" }, want: "must not target private"},
		{name: "http tool ipv6 zone", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.URL = "https://[fe80::1%25en0]/x" }, want: "carry an IPv6 zone"},
		{name: "uppercase denied host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://METADATA.GOOGLE.INTERNAL/token"
		}, want: "tokenURL host is not allowed"},
		{name: "http tool trailing dot", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.URL = "https://kubernetes.default./x" }, want: "must not be empty, end with a dot, or carry an IPv6 zone"},
		{name: "missing client id", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientID = " " }, want: "clientID is required"},
		{name: "missing secret key", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientSecretRef.Key = "" }, want: "clientSecretRef requires name and key"},
		{name: "bad client auth", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientAuthentication = "PrivateKeyJWT" }, want: "clientAuthentication must be"},
		{name: "scope with space", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Read = []string{"read user"} }, want: "scopes.read entries"},
		{name: "duplicate scope", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Write = []string{"repo", "repo"} }, want: "duplicate scope"},
		{name: "reserved authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"redirect_uri": "https://evil.example"}
		}, want: "reserved OAuth fields"},
		{name: "uppercase authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"Prompt": "consent"}
		}, want: "keys must be lowercase"},
		{name: "newline authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"prompt": "a\nb"}
		}, want: "line breaks"},
		{name: "no tools", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools = nil }, want: "at least one"},
		{name: "bad tool name", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[0].Name = "List-PRs" }, want: "snake_case"},
		{name: "duplicate tool", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[1].Name = p.Spec.Tools[0].Name }, want: "more than once"},
		{name: "bad class", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[0].Class = "coordination" }, want: "class must be read or write"},
		{name: "builtin with http", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[0].HTTP = &corev1alpha1.ConnectorHTTPTool{URL: "https://example.test"}
		}, want: "must not set http"},
		{name: "unknown builtin", builtin: func(name string) bool { return name == "list_pull_requests" }, want: `builtin tool "create_pull_request" is not a known`},
		{name: "known builtin", builtin: func(string) bool { return true }},
		{name: "http without http", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP = nil }, want: "requires http"},
		{name: "http without description", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].Description = "" }, want: "requires a description"},
		{name: "http plain url", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.URL = "http://api.github.com/x" }, want: "tools.search.url must be an absolute HTTPS URL"},
		{name: "http private url", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.URL = "https://192.168.1.1/x" }, want: "must not target private"},
		{name: "http bad method", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.Method = "TRACE" }, want: "method is not supported"},
		{name: "http authorization header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Authorization": "Bearer x"}
		}, want: "may not set the Authorization header"},
		{name: "http txn-token header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Txn-Token": "x"}
		}, want: "may not set the Txn-Token header"},
		{name: "http non-canonical header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"x-custom": "v"}
		}, want: "header names must be canonical"},
		{name: "http header newline", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Custom": "a\r\nb"}
		}, want: "header values must not contain line breaks"},
		{name: "http zero timeout", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Timeout = &metav1.Duration{}
		}, want: "timeout must be positive and at most"},
		{name: "http timeout too long", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Timeout = &metav1.Duration{Duration: 11 * time.Minute}
		}, want: "timeout must be positive and at most"},
		{name: "http timeout at bound", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Timeout = &metav1.Duration{Duration: MaxHTTPToolTimeout}
		}},
		{name: "bad source", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[0].Source = "MCP" }, want: "source must be Builtin or HTTP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var provider *corev1alpha1.ConnectorProvider
			if tt.name != "nil provider" {
				provider = validProvider()
				if tt.mutate != nil {
					tt.mutate(provider)
				}
			}
			issue := ValidateProviderSpec(provider, tt.builtin)
			if tt.name == "nil provider" {
				if issue == nil {
					t.Fatal("expected issue for nil provider")
				}
				return
			}
			if tt.want == "" {
				if issue != nil {
					t.Fatalf("unexpected issue: %v", issue)
				}
				return
			}
			if issue == nil || !strings.Contains(issue.Message, tt.want) {
				t.Fatalf("issue = %v, want message containing %q", issue, tt.want)
			}
			if issue.Reason != ReasonInvalidProvider {
				t.Fatalf("reason = %q, want %q", issue.Reason, ReasonInvalidProvider)
			}
		})
	}
}

func TestResolveProviderReferences(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	provider := validProvider()
	tests := []struct {
		name       string
		objects    []runtime.Object
		reader     func(client.Client) client.Reader
		wantReason string
		wantErr    bool
	}{
		{name: "missing secret", wantReason: ReasonReferenceNotFound},
		{name: "empty key", objects: []runtime.Object{&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: "tenant"},
			Data:       map[string][]byte{"clientSecret": []byte("  ")},
		}}, wantReason: ReasonReferenceInvalid},
		{name: "wrong namespace", objects: []runtime.Object{&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: "other"},
			Data:       map[string][]byte{"clientSecret": []byte("s3cret")},
		}}, wantReason: ReasonReferenceNotFound},
		{name: "resolved", objects: []runtime.Object{&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: "tenant"},
			Data:       map[string][]byte{"clientSecret": []byte("s3cret")},
		}}},
		{name: "read error", reader: func(c client.Client) client.Reader {
			return ctrlfake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("boom")
				},
			}).Build()
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tt.objects...).Build()
			var reader client.Reader = c
			if tt.reader != nil {
				reader = tt.reader(c)
			}
			issue, err := ResolveProviderReferences(context.Background(), reader, provider)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantReason == "" {
				if issue != nil {
					t.Fatalf("unexpected issue: %v", issue)
				}
				return
			}
			if issue == nil || issue.Reason != tt.wantReason {
				t.Fatalf("issue = %v, want reason %q", issue, tt.wantReason)
			}
			if strings.Contains(issue.Message, "s3cret") {
				t.Fatalf("issue leaked secret: %s", issue.Message)
			}
		})
	}
	if _, err := ResolveProviderReferences(context.Background(), nil, provider); err == nil {
		t.Fatal("expected error for nil reader")
	}
	if issue, _ := ResolveProviderReferences(context.Background(), ctrlfake.NewClientBuilder().WithScheme(scheme).Build(), nil); issue == nil {
		t.Fatal("expected issue for nil provider")
	}
}

func TestProviderAccepted(t *testing.T) {
	provider := validProvider()
	provider.Generation = 2
	if ProviderAccepted(provider) {
		t.Fatal("provider without conditions must not be accepted")
	}
	provider.Status.Conditions = []metav1.Condition{
		{Type: corev1alpha1.ConnectorProviderConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 2},
		{Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: 1},
	}
	if ProviderAccepted(provider) {
		t.Fatal("stale ResolvedRefs generation must not be accepted")
	}
	provider.Status.Conditions[1].ObservedGeneration = 2
	if !ProviderAccepted(provider) {
		t.Fatal("expected accepted")
	}
	deleting := provider.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if ProviderAccepted(deleting) {
		t.Fatal("deleting provider must not be accepted")
	}
	if ProviderAccepted(nil) {
		t.Fatal("nil provider must not be accepted")
	}
}

func TestToolsAndScopesForMode(t *testing.T) {
	provider := validProvider()
	readOnly := ToolsForMode(provider, corev1alpha1.ConnectionModeReadOnly)
	if len(readOnly) != 2 {
		t.Fatalf("readOnly tools = %d, want 2", len(readOnly))
	}
	for _, tool := range readOnly {
		if tool.Class == corev1alpha1.ConnectorToolClassWrite {
			t.Fatalf("readOnly mode exposed write tool %q", tool.Name)
		}
	}
	if got := len(ToolsForMode(provider, corev1alpha1.ConnectionModeReadWrite)); got != 3 {
		t.Fatalf("readWrite tools = %d, want 3", got)
	}
	if got := ScopesForMode(provider, corev1alpha1.ConnectionModeReadOnly); len(got) != 1 || got[0] != "read:user" {
		t.Fatalf("readOnly scopes = %v", got)
	}
	provider.Spec.OAuth.Scopes.Write = []string{"repo", "read:user"}
	if got := ScopesForMode(provider, corev1alpha1.ConnectionModeReadWrite); len(got) != 2 || got[0] != "read:user" || got[1] != "repo" {
		t.Fatalf("readWrite scopes = %v", got)
	}
	if ToolsForMode(nil, "") != nil || ScopesForMode(nil, "") != nil {
		t.Fatal("nil provider must yield nil")
	}
}

func TestProviderReferencesSecret(t *testing.T) {
	provider := validProvider()
	same := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: "tenant"}}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: "other"}}
	if !ProviderReferencesSecret(provider, same) || ProviderReferencesSecret(provider, other) || ProviderReferencesSecret(nil, same) {
		t.Fatal("secret reference matching is wrong")
	}
}

func TestIssueError(t *testing.T) {
	var nilIssue *Issue
	if nilIssue.Error() == "" || (&Issue{Message: "m"}).Error() != "m" {
		t.Fatal("Issue.Error is wrong")
	}
}
