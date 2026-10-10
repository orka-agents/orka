/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"net"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestValidateToolsBuiltinDeclarationsFollowTheCatalog(t *testing.T) {
	known := func(name string) bool {
		if name == "web_search" {
			return true
		}
		_, ok := BuiltinConnectorToolClass(name)
		return ok
	}
	for _, tc := range []struct {
		name string
		tool corev1alpha1.ConnectorTool
		want string
	}{
		{name: "read tool", tool: corev1alpha1.ConnectorTool{Name: "list_pull_requests", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin}},
		{name: "write tool", tool: corev1alpha1.ConnectorTool{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin}},
		// A write tool declared as read would skip approval and readOnly hiding.
		{name: "write declared read", tool: corev1alpha1.ConnectorTool{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin}, want: "must be declared with class write"},
		{name: "read declared write", tool: corev1alpha1.ConnectorTool{Name: "get_issue", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin}, want: "must be declared with class read"},
		// A built-in that ignores the credential must not be declared at all.
		{name: "not linked", tool: corev1alpha1.ConnectorTool{Name: "web_search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin}, want: "cannot use a linked account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := validProvider()
			provider.Spec.Tools = []corev1alpha1.ConnectorTool{tc.tool}
			issue := validateTools(provider, known)
			switch {
			case tc.want == "" && issue != nil:
				t.Fatalf("unexpected issue: %s", issue.Message)
			case tc.want != "" && (issue == nil || !strings.Contains(issue.Message, tc.want)):
				t.Fatalf("issue = %v, want %q", issue, tc.want)
			}
		})
	}
	// Only github.com's OAuth endpoints issue tokens for the built-ins'
	// fixed audience; GitHub Enterprise or any other issuer is refused.
	for name, mutate := range map[string]func(*corev1alpha1.ConnectorProvider){
		"enterprise": func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AuthorizeURL = "https://github.example.com/login/oauth/authorize"
			p.Spec.OAuth.TokenURL = "https://github.example.com/login/oauth/access_token"
		},
		"token url elsewhere": func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://oauth.example.com/token" },
		"port": func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://github.com:8443/login/oauth/access_token"
		},
		"lookalike": func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://github.com.example.com/token"
		},
	} {
		provider := validProvider()
		mutate(provider)
		if ProviderIssuesGitHubCredentials(provider) {
			t.Fatalf("%s: provider must not count as github.com", name)
		}
		if issue := ValidateProviderSpec(provider, known); issue == nil || !strings.Contains(issue.Message, "only a github.com OAuth provider") {
			t.Fatalf("%s: issue = %v, want built-in declarations refused", name, issue)
		}
		provider.Spec.Tools = provider.Spec.Tools[2:]
		if issue := ValidateProviderSpec(provider, known); issue != nil {
			t.Fatalf("%s: HTTP-only provider must stay valid: %s", name, issue.Message)
		}
	}
	if !ProviderIssuesGitHubCredentials(validProvider()) || ProviderIssuesGitHubCredentials(nil) {
		t.Fatal("github.com provider must count, nil must not")
	}
	plain := validProvider()
	plain.Spec.OAuth.TokenURL = "http://github.com/login/oauth/access_token"
	if ProviderIssuesGitHubCredentials(plain) {
		t.Fatal("a plain-http github.com endpoint must not count")
	}
	explicitPort := validProvider()
	explicitPort.Spec.OAuth.TokenURL = "https://github.com:443/login/oauth/access_token"
	if !ProviderIssuesGitHubCredentials(explicitPort) {
		t.Fatal("github.com on its default port must count")
	}
	if names := BuiltinConnectorToolNames(); len(names) != 8 || names[0] != "check_pull_request_ci" {
		t.Fatalf("catalog names = %v", names)
	}
	if _, ok := DeclaresBuiltinTool(nil, "get_issue"); ok {
		t.Fatal("a nil provider declares nothing")
	}
	provider := &corev1alpha1.ConnectorProvider{Spec: corev1alpha1.ConnectorProviderSpec{Tools: []corev1alpha1.ConnectorTool{
		{Name: "get_issue", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP},
		{Name: "list_issues", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin},
	}}}
	if _, ok := DeclaresBuiltinTool(provider, "get_issue"); ok {
		t.Fatal("an HTTP declaration is not a built-in declaration")
	}
	if tool, ok := DeclaresBuiltinTool(provider, "list_issues"); !ok || tool.Class != corev1alpha1.ConnectorToolClassRead {
		t.Fatalf("declared = %+v %t", tool, ok)
	}
}

func TestPrivateEndpointsAllowedRelaxesOnlyHostRules(t *testing.T) {
	t.Cleanup(func() { SetAllowPrivateEndpoints(false) })
	provider := validProvider()
	provider.Spec.Tools = provider.Spec.Tools[2:] // HTTP tools only; built-ins need github.com
	provider.Spec.OAuth.AuthorizeURL = "https://fixture.orka-system.svc:8443/oauth/authorize"
	provider.Spec.OAuth.TokenURL = "https://fixture.orka-system.svc:8443/oauth/token"
	provider.Spec.Tools[0].HTTP.URL = "https://127.0.0.1:8443/api/items"
	if issue := ValidateProviderSpec(provider, nil); issue == nil || !strings.Contains(issue.Message, "host is not allowed") {
		t.Fatalf("issue = %v, want cluster-local hosts refused by default", issue)
	}
	SetAllowPrivateEndpoints(true)
	if !PrivateEndpointsAllowed() {
		t.Fatal("toggle must report on")
	}
	if issue := ValidateProviderSpec(provider, nil); issue != nil {
		t.Fatalf("private endpoints allowed: %s", issue.Message)
	}
	// The fixed infrastructure hosts stay denied under the allowance: an
	// OAuth client secret or a person's token must never go to metadata or
	// the Kubernetes API, fixture mode or not.
	for _, host := range []string{"metadata.google.internal", "KUBERNETES.DEFAULT.SVC", "kubernetes.default.svc.cluster.local", "kubernetes.default", "169.254.169.254", "169.254.170.23", "100.100.100.200", "[fe80::1]", "[fd00:ec2::254]"} {
		provider.Spec.OAuth.TokenURL = "https://" + host + "/oauth/token"
		if issue := ValidateProviderSpec(provider, nil); issue == nil || !strings.Contains(issue.Message, "host is not allowed") {
			t.Fatalf("token host %s under the allowance: %v", host, issue)
		}
		provider.Spec.OAuth.TokenURL = "https://fixture.orka-system.svc:8443/oauth/token"
		provider.Spec.Tools[0].HTTP.URL = "https://" + host + "/api/items"
		if issue := ValidateProviderSpec(provider, nil); issue == nil || !strings.Contains(issue.Message, "not allowed") {
			t.Fatalf("tool host %s under the allowance: %v", host, issue)
		}
		provider.Spec.Tools[0].HTTP.URL = "https://127.0.0.1:8443/api/items"
	}
	// The API server's literal address, as the Pod sees it, is refused too.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	provider.Spec.OAuth.TokenURL = "https://10.96.0.1/oauth/token"
	if issue := ValidateProviderSpec(provider, nil); issue == nil || !strings.Contains(issue.Message, "host is not allowed") {
		t.Fatalf("API service address under the allowance: %v", issue)
	}
	if !InfrastructureAddressDenied(net.ParseIP("10.96.0.1")) || InfrastructureAddressDenied(net.ParseIP("10.96.0.2")) {
		t.Fatal("the API service address alone is denied by address")
	}
	if _, err := PrivateEndpointDialContext(context.Background(), "tcp", "10.96.0.1:443"); err == nil || !strings.Contains(err.Error(), "infrastructure") {
		t.Fatalf("dialing the API service address under the allowance: %v", err)
	}
	if _, err := PrivateEndpointDialContext(context.Background(), "tcp", "169.254.169.254:80"); err == nil || !strings.Contains(err.Error(), "infrastructure") {
		t.Fatalf("dialing metadata under the allowance: %v", err)
	}
	// Plain http stays refused even for fixtures.
	provider.Spec.OAuth.TokenURL = "http://fixture.orka-system.svc:8080/oauth/token"
	if issue := ValidateProviderSpec(provider, nil); issue == nil || !strings.Contains(issue.Message, "HTTPS") {
		t.Fatalf("issue = %v, want https still required", issue)
	}
}
