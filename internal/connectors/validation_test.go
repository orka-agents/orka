/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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
		{name: "name longer than a label value", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Name = strings.Repeat("a", 64) }, want: "provider name must be at most 63 characters"},
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
		{name: "cluster-local fqdn", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://kubernetes.default.svc.cluster.local/token"
		}, want: "tokenURL host is not allowed"},
		{name: "custom cluster domain svc", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://kubernetes.default.svc.example/token"
		}, want: "tokenURL host is not allowed"},
		{name: "any svc name", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AuthorizeURL = "https://orka-api.orka-system.svc/authorize"
		}, want: "authorizeURL host is not allowed"},
		{name: "subdomain of denied host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://api.localhost/token" }, want: "tokenURL host is not allowed"},
		{name: "internal suffix", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://oauth.corp.internal/token" }, want: "tokenURL host is not allowed"},
		{name: "mdns suffix", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://printer.local/token" }, want: "tokenURL host is not allowed"},
		{name: "http tool cluster-local", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.default.svc.cluster.local/x"
		}, want: "host is not allowed"},
		{name: "public host with local label", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://local.example.com/token" }},
		{name: "port too large", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example.com:99999/token" }, want: "port must be between"},
		{name: "port zero", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.AuthorizeURL = "https://example.com:0/authorize" }, want: "port must be between"},
		{name: "explicit port ok", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example.com:8443/token" }},
		{name: "http tool bad port", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.URL = "https://api.github.com:70000/x" }, want: "port must be between"},
		{name: "parameters not an object", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`"string"`)}
		}, want: "parameters must be a JSON Schema object"},
		{name: "parameters array", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`[]`)}
		}, want: "parameters must be a JSON Schema object"},
		{name: "parameters wrong type", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"string"}`)}
		}, want: `parameters must declare type "object"`},
		{name: "parameters missing type", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{}`)}
		}, want: `parameters must declare type "object"`},
		{name: "parameters properties without type", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"properties":{"q":{"type":"string"}}}`)}
		}, want: `parameters must declare type "object"`},
		{name: "parameters bad properties", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":[]}`)}
		}, want: "parameters.properties must be an object"},
		{name: "parameters bad required", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","required":"q"}`)}
		}, want: "parameters.required must be an array"},
		{name: "parameters ok", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)}
		}},
		{name: "uppercase denied host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://METADATA.GOOGLE.INTERNAL/token"
		}, want: "tokenURL host is not allowed"},
		{name: "http tool trailing dot", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.URL = "https://kubernetes.default./x" }, want: "must not be empty, end with a dot, or carry an IPv6 zone"},
		{name: "missing client id", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientID = " " }, want: "clientID is required"},
		{name: "missing secret key", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientSecretRef.Key = "" }, want: "clientSecretRef requires name and key"},
		{name: "bad client auth", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientAuthentication = "PrivateKeyJWT" }, want: "clientAuthentication must be"},
		{name: "client id control byte", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientID = "client\nid" }, want: "clientID"},
		{name: "client id non-ascii", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientID = "clïent" }, want: "clientID"},
		{name: "token url presets code", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example.com/token?code=abc" }, want: "must not preset reserved"},
		{name: "revocation url carries refresh token", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.RevocationURL = "https://example.com/revoke?refresh_token=abc"
		}, want: "must not carry credentials"},
		{name: "revocation url presets grant type", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.RevocationURL = "https://example.com/revoke?grant_type=x"
		}, want: "must not preset reserved"},
		{name: "http tool named like a builtin", builtin: func(name string) bool {
			return name == "file_read" || name == "create_pull_request" || name == "list_pull_requests"
		}, mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].Name = "file_read" }, want: "collides with a built-in"},
		{name: "scope with space", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Read = []string{"read user"} }, want: "scopes.read entries"},
		{name: "scope with control byte", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Read = []string{"read\x00profile"} }, want: "scopes.read entries"},
		{name: "scope with non-ascii", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Write = []string{"répo"} }, want: "scopes.write entries"},
		{name: "scope with quote", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Read = []string{`read"x`} }, want: "scopes.read entries"},
		{name: "authorize parameter with control byte", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"audience": "a\x00b"}
		}, want: "without control bytes"},
		{name: "credential-like authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"access_token": "x"}
		}, want: "must not carry credentials"},
		{name: "api key authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"api_key": "x"}
		}, want: "must not carry credentials"},
		{name: "client assertion authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"client_assertion": "x"}
		}, want: "must not carry credentials"},
		{name: "authorize url presets state", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AuthorizeURL = "https://example.com/authorize?state=fixed"
		}, want: "must not preset reserved"},
		{name: "authorize url presets redirect", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AuthorizeURL = "https://example.com/authorize?redirect_uri=https%3A%2F%2Fevil.example.test"
		}, want: "must not preset reserved"},
		{name: "authorize url benign query ok", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AuthorizeURL = "https://example.com/authorize?audience=api"
		}},
		{name: "token url credential query", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://example.com/token?client_secret=abc"
		}, want: "must not carry credentials"},
		{name: "malformed query", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example.com/token?a=%zz" }, want: "query must be well-formed"},
		{name: "semicolon query", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example.com/token?a=1;b=2" }, want: "semicolon"},
		{name: "http tool api key header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Api-Key": "abc"}
		}, want: "looks like a credential"},
		{name: "http tool auth token header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Auth-Token": "abc"}
		}, want: "looks like a credential"},
		{name: "http tool benign header ok", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Github-Api-Version": "2022-11-28", "Accept": "application/vnd.github+json"}
		}},
		{name: "http tool subscription key header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Ocp-Apim-Subscription-Key": "abc"}
		}, want: "looks like a credential"},
		{name: "http tool functions key header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Functions-Key": "abc"}
		}, want: "looks like a credential"},
		{name: "client key authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"client_key": "x"}
		}, want: "must not carry credentials"},
		{name: "http tool x-auth header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Auth": "abc"}
		}, want: "looks like a credential"},
		{name: "http tool access key header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Access-Key": "abc"}
		}, want: "looks like a credential"},
		{name: "http tool concatenated authentication header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Authentication": "abc"}
		}, want: "looks like a credential"},
		{name: "http tool concatenated access key header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Accesskey": "abc"}
		}, want: "looks like a credential"},
		{name: "concatenated access key query", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/x?awsaccesskeyid=abc"
		}, want: "must not carry credentials"},
		{name: "http tool concatenated benign header ok", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Request-Id": "abc", "If-None-Match": "abc"}
		}},
		{name: "auth authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"auth": "x"}
		}, want: "must not carry credentials"},
		{name: "loopback shorthand host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.AuthorizeURL = "https://127.1/authorize" }, want: "canonical IP"},
		{name: "decimal loopback host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.AuthorizeURL = "https://2130706433/authorize" }, want: "canonical IP"},
		{name: "octal loopback host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://0177.0.0.1/token" }, want: "canonical IP"},
		{name: "hex loopback host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://0x7f000001/token" }, want: "canonical IP"},
		{name: "numeric tld host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example.123/token" }, want: "canonical IP"},
		{name: "percent-encoded ideographic dots", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://127%E3%80%820%E3%80%820%E3%80%821/token"
		}, want: "host must be ASCII"},
		{name: "fullwidth digits host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AuthorizeURL = "https://%EF%BC%91%EF%BC%92%EF%BC%97.0.0.1/authorize"
		}, want: "host must be ASCII"},
		{name: "single-label host", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://kubernetes/token" }, want: "host is not allowed"},
		{name: "single-label tool host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://orka-api/x"
		}, want: "host is not allowed"},
		{name: "oversized endpoint url", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://github.com/" + strings.Repeat("a", 2048)
		}, want: "tokenURL must be at most 2048 bytes"},
		{name: "oversized tool url", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/" + strings.Repeat("a", 2048)
		}, want: "must be at most 2048 bytes"},
		{name: "oversized client id", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.ClientID = strings.Repeat("c", 257)
		}, want: "clientID must be at most 256 bytes"},
		{name: "unicode tool host", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://127%E3%80%820%E3%80%820%E3%80%821/x"
		}, want: "host must be ASCII"},
		{name: "punycode hostname ok", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://xn--bcher-kva.example/token" }},
		{name: "hexish hostname ok", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://ab12.cafe.example.com/token" }},
		{name: "http tool credential query", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/x?token=abc"
		}, want: "must not carry credentials"},
		{name: "parameters nested property not a schema", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"q":1}}`)}
		}, want: "valid JSON Schema"},
		{name: "parameters credential-bearing remote ref", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"q":{"$ref":"https://schemas.example.test/q.json?access_token=s3cr3t"}}}`)}
		}, want: "resolvable JSON Schema with only local references"},
		{name: "parameters unresolvable ref", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"q":{"$ref":"#/$defs/missing"}}}`)}
		}, want: "resolvable JSON Schema"},
		{name: "parameters nested ok", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"q":{"type":"string","minLength":1},"tags":{"type":"array","items":{"type":"string"}}},"required":["q"]}`)}
		}},
		{name: "duplicate scope", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Write = []string{"repo", "repo"} }, want: "duplicate scope"},
		{name: "duplicate credential-shaped scope is not echoed", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.Scopes.Read = []string{"access_token.s3cr3t", "access_token.s3cr3t"}
		}, want: "duplicate scope"},
		{name: "invalid client secret name", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.ClientSecretRef.Name = "bad/name"
		}, want: "valid Secret and data key"},
		{name: "invalid client secret key", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.ClientSecretRef.Key = "bad key"
		}, want: "valid Secret and data key"},
		{name: "oversized endpoint query", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AuthorizeURL = "https://example.com/authorize?prompt=" + strings.Repeat("a", 1100)
		}, want: "query must be at most 1024 bytes"},
		{name: "oversized tool url query", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/x?filter=" + strings.Repeat("a", 1100)
		}, want: "query must be at most 1024 bytes"},
		{name: "oversized authorize parameter value", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"audience": strings.Repeat("a", 513)}
		}, want: "at most 512 bytes"},
		{name: "oversized authorize parameters in total", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{}
			for i := range 5 {
				p.Spec.OAuth.AdditionalAuthorizeParameters["hint"+strconv.Itoa(i)] = strings.Repeat("a", 500)
			}
		}, want: "encode to at most 2048 bytes"},
		{name: "credential-shaped header name is not echoed", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-S3cr3t-Key": "v"}
		}, want: "header whose name looks like a credential"},
		{name: "oversized scope set in total", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.Scopes.Read, p.Spec.OAuth.Scopes.Write = nil, nil
			for i := range 12 {
				p.Spec.OAuth.Scopes.Read = append(p.Spec.OAuth.Scopes.Read, strconv.Itoa(i)+strings.Repeat("r", 200))
			}
		}, want: "oauth.scopes must encode to at most 2048 bytes"},
		{name: "templated tool url", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/repos/{{repo}}/issues"
		}, want: "absolute HTTPS URL"},
		{name: "templated tool url query", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/search/issues?q={{q}}"
		}, want: "must not contain template placeholders"},
		{name: "oversized tool header value", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Accept": strings.Repeat("a", 1025)}
		}, want: "values at most 1024 bytes"},
		{name: "oversized tool headers in total", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{}
			for i := range 5 {
				p.Spec.Tools[2].HTTP.Headers["X-Hint-"+strconv.Itoa(i)] = strings.Repeat("a", 1000)
			}
		}, want: "headers must total at most 4096 bytes"},
		{name: "oversized scope", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.Scopes.Read = []string{strings.Repeat("a", 257)}
		}, want: "at most 256 bytes"},
		{name: "token url presets code verifier", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://example.com/token?code_verifier=abc"
		}, want: "must not preset reserved"},
		{name: "authorize parameter code verifier", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"code_verifier": "abc"}
		}, want: "reserved OAuth fields"},
		{name: "empty dns label", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example..com/token" }, want: "valid DNS name"},
		{name: "label starts with hyphen", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://-bad.example/token" }, want: "valid DNS name"},
		{name: "overlong dns label", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://" + strings.Repeat("a", 64) + ".example/token"
		}, want: "valid DNS name"},
		{name: "tool connection header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Connection": "Authorization"}
		}, want: "may not set the Connection header"},
		{name: "tool upgrade header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Upgrade": "h2c"}
		}, want: "may not set the Upgrade header"},
		{name: "tool url api filter named state ok", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/repos/o/r/pulls?state=open"
		}},
		{name: "tool url credential query still rejected", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.URL = "https://api.github.com/x?access_token=abc"
		}, want: "must not carry credentials"},
		{name: "reserved authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"redirect_uri": "https://evil.example"}
		}, want: "reserved OAuth fields"},
		{name: "uppercase authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"Prompt": "consent"}
		}, want: "keys must be lowercase"},
		{name: "newline authorize parameter", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"prompt": "a\nb"}
		}, want: "without control bytes"},
		{name: "no tools", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools = nil }, want: "at least one"},
		{name: "bad tool name", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[0].Name = "List-PRs" }, want: "snake_case"},
		{name: "duplicate tool", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[1].Name = p.Spec.Tools[0].Name }, want: "more than once"},
		{name: "bad class", mutate: func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[0].Class = "coordination" }, want: "class must be read or write"},
		{name: "builtin with http", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[0].HTTP = &corev1alpha1.ConnectorHTTPTool{URL: "https://example.test"}
		}, want: "must not set http"},
		{name: "unknown builtin", builtin: func(name string) bool { return name == "list_pull_requests" }, want: `builtin tool "create_pull_request" is not a known`},
		{name: "known builtin", builtin: func(name string) bool { return name == "create_pull_request" || name == "list_pull_requests" }},
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
		}, want: "header names must be in canonical form"},
		{name: "http header with space", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X Bad": "v"}
		}, want: "header names must be valid HTTP tokens"},
		{name: "http header with colon", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Bad:Header": "v"}
		}, want: "header names must be valid HTTP tokens"},
		{name: "http header non-ascii", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Ünicode": "v"}
		}, want: "header names must be valid HTTP tokens"},
		{name: "http content-length header", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"Content-Length": "0"}
		}, want: "may not set the Content-Length header"},
		{name: "http header newline", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Custom": "a\r\nb"}
		}, want: "header values must not contain control bytes"},
		{name: "http header nul", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Custom": "a\x00b"}
		}, want: "header values must not contain control bytes"},
		{name: "http header del", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Custom": "a\x7fb"}
		}, want: "header values must not contain control bytes"},
		{name: "http header tab ok", mutate: func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Custom": "a\tb"}
		}},
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
			// The message becomes public provider status: it never repeats
			// a credential-bearing value from the spec.
			if strings.Contains(strings.ToLower(issue.Message), "s3cr3t") || strings.Contains(issue.Message, "access_token") {
				t.Fatalf("issue echoed a credential-bearing value: %s", issue.Message)
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

func TestScopesCoverAndConnectionLinked(t *testing.T) {
	if !ScopesCover([]string{"a", "b"}, []string{"a"}) || ScopesCover([]string{"a"}, []string{"a", "b"}) || !ScopesCover(nil, nil) || ScopesCover(nil, []string{"a"}) {
		t.Fatal("ScopesCover is wrong")
	}
	connection := &corev1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
	if ConnectionLinked(connection) || ConnectionLinked(nil) {
		t.Fatal("no conditions must not be linked")
	}
	connection.Status.Conditions = []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue}}
	if ConnectionLinked(connection) {
		t.Fatal("Ready alone is not linked")
	}
	connection.Status.Conditions = append(connection.Status.Conditions, metav1.Condition{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, ObservedGeneration: 2})
	if ConnectionLinked(connection) {
		t.Fatal("an unresolved provider is not linked")
	}
	connection.Status.Conditions = append(connection.Status.Conditions, metav1.Condition{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, ObservedGeneration: 2})
	if !ConnectionLinked(connection) {
		t.Fatal("Ready plus current ProviderResolved and ScopesGranted is linked")
	}
	stale := connection.DeepCopy()
	stale.Generation = 3
	if ConnectionLinked(stale) {
		t.Fatal("controller conditions from an older generation are not linked")
	}
	unresolved := connection.DeepCopy()
	meta.SetStatusCondition(&unresolved.Status.Conditions, metav1.Condition{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionFalse, ObservedGeneration: 2})
	if ConnectionLinked(unresolved) {
		t.Fatal("a lost provider is not linked even with Ready and ScopesGranted preserved")
	}
	deleting := connection.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if ConnectionLinked(deleting) {
		t.Fatal("a deleting connection is not linked")
	}
}

func TestProviderAuthorityDigestAndConsent(t *testing.T) {
	provider := validProvider()
	provider.UID = "provider-uid"
	digest := ProviderAuthorityDigest(provider)
	if len(digest) != 64 || ProviderAuthorityDigest(nil) != "" {
		t.Fatalf("digest = %q", digest)
	}
	connection := &corev1alpha1.Connection{}
	if ConsentMatchesProvider(connection, provider) {
		t.Fatal("a missing consent record must not match")
	}
	connection.Status.Consent = ConsentFor(provider)
	if !ConsentMatchesProvider(connection, provider) {
		t.Fatal("the recorded consent must match the same provider")
	}
	for name, mutate := range map[string]func(p *corev1alpha1.ConnectorProvider){
		"uid": func(p *corev1alpha1.ConnectorProvider) { p.UID = "recreated" },
		"audience": func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"audience": "https://other.example.test"}
		},
		"http tool url":    func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.URL = "https://api.github.com/elsewhere" },
		"http tool method": func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].HTTP.Method = "DELETE" },
		"http tool header": func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools[2].HTTP.Headers = map[string]string{"X-Http-Method-Override": "DELETE"}
		},
		"http tool class": func(p *corev1alpha1.ConnectorProvider) { p.Spec.Tools[2].Class = corev1alpha1.ConnectorToolClassWrite },
		"new http tool": func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.Tools = append(p.Spec.Tools, corev1alpha1.ConnectorTool{Name: "extra", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "x", HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/extra"}})
		},
		"client id":  func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientID = "other" },
		"token url":  func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.TokenURL = "https://example.com/other-token" },
		"secret ref": func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientSecretRef.Key = "other" },
		"client auth": func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.ClientAuthentication = corev1alpha1.ConnectorClientAuthSecretPost
		},
	} {
		changed := provider.DeepCopy()
		mutate(changed)
		if ConsentMatchesProvider(connection, changed) {
			t.Fatalf("%s change must require a new consent", name)
		}
	}
	scopesOnly := provider.DeepCopy()
	scopesOnly.Spec.OAuth.Scopes.Read = append(scopesOnly.Spec.OAuth.Scopes.Read, "read:org")
	if !ConsentMatchesProvider(connection, scopesOnly) {
		t.Fatal("a scope-only change is judged by ScopesGranted, not the authority digest")
	}
	// The parameter encoding is deterministic regardless of map order.
	withParams := provider.DeepCopy()
	withParams.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"audience": "api", "prompt": "consent"}
	again := withParams.DeepCopy()
	if ProviderAuthorityDigest(withParams) != ProviderAuthorityDigest(again) || ProviderAuthorityDigest(withParams) == digest {
		t.Fatal("authorize parameters must change the digest deterministically")
	}
	// The encoding is injective: values that embed delimiters cannot collide
	// with a different parameter set.
	collide := provider.DeepCopy()
	collide.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"a": "x\x00param:b\x00v"}
	split := provider.DeepCopy()
	split.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{"a": "x", "b": "v"}
	if ProviderAuthorityDigest(collide) == ProviderAuthorityDigest(split) {
		t.Fatal("delimiter-bearing values must not collide with a different parameter set")
	}
	// The issuer digest ignores tool destinations but follows the client.
	retargeted := provider.DeepCopy()
	retargeted.Spec.Tools[2].HTTP.URL = "https://api.github.com/elsewhere"
	if ProviderIssuerDigest(retargeted) != ProviderIssuerDigest(provider) || ProviderAuthorityDigest(retargeted) == ProviderAuthorityDigest(provider) {
		t.Fatal("a retargeted tool must move the authority digest but not the issuer digest")
	}
	rotated := provider.DeepCopy()
	rotated.Spec.OAuth.ClientID = "other"
	if ProviderIssuerDigest(rotated) == ProviderIssuerDigest(provider) || ProviderIssuerDigest(nil) != "" {
		t.Fatal("a rotated client must move the issuer digest")
	}
	// Authorize-only parameters shape consent, not the issued token's
	// authority: they move the consent fence but neither block a refresh nor
	// skip revocation.
	prompted := provider.DeepCopy()
	if prompted.Spec.OAuth.AdditionalAuthorizeParameters == nil {
		prompted.Spec.OAuth.AdditionalAuthorizeParameters = map[string]string{}
	}
	prompted.Spec.OAuth.AdditionalAuthorizeParameters["prompt"] = "select_account"
	if ProviderIssuerDigest(prompted) != ProviderIssuerDigest(provider) || ProviderAuthorityDigest(prompted) == ProviderAuthorityDigest(provider) {
		t.Fatal("an authorize parameter must move the authority digest but not the issuer digest")
	}
	relocated := provider.DeepCopy()
	relocated.Spec.OAuth.AuthorizeURL = "https://github.example.test/login/oauth/authorize-v2"
	if ProviderIssuerDigest(relocated) != ProviderIssuerDigest(provider) || ProviderAuthorityDigest(relocated) == ProviderAuthorityDigest(provider) {
		t.Fatal("the authorize URL must move the authority digest but not the issuer digest")
	}
	moved := provider.DeepCopy()
	moved.Spec.OAuth.TokenURL = "https://github.example.test/login/oauth/token-v2"
	if ProviderIssuerDigest(moved) == ProviderIssuerDigest(provider) {
		t.Fatal("the token URL must move the issuer digest")
	}
	// The effective PKCE setting is part of what the browser's consent was
	// started with; flipping it mid-flow must fail the callback's fence.
	noPKCE := provider.DeepCopy()
	off := false
	noPKCE.Spec.OAuth.PKCE = &off
	if ProviderAuthorityDigest(noPKCE) == ProviderAuthorityDigest(provider) || ProviderIssuerDigest(noPKCE) != ProviderIssuerDigest(provider) {
		t.Fatal("the PKCE setting must move the authority digest but not the issuer digest")
	}
	// Built-in declarations do not carry a destination and do not move the digest.
	builtinOnly := provider.DeepCopy()
	builtinOnly.Spec.Tools = append(builtinOnly.Spec.Tools, corev1alpha1.ConnectorTool{Name: "list_issues", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin})
	if ProviderAuthorityDigest(builtinOnly) != digest {
		t.Fatal("a new built-in declaration must not require consent again")
	}
}

func TestValidScopeTokenRefusesCommas(t *testing.T) {
	// Comma-delimited scope lists (GitHub) are split on the comma, so a
	// configured scope must never contain one.
	for scope, want := range map[string]bool{"repo": true, "read:user": true, "repo,gist": false, "": false, "a b": false, "ünï": false} {
		if got := validScopeToken(scope); got != want {
			t.Fatalf("validScopeToken(%q) = %t, want %t", scope, got, want)
		}
	}
}
