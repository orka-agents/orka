/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/jsonschema-go/jsonschema"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agenthost"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

const (
	ReasonAccepted          = "Accepted"
	ReasonConnectionsRemain = "ConnectionsRemain"
	ReasonInvalidProvider   = "InvalidProvider"
	ReasonResolvedRefs      = "ResolvedRefs"
	ReasonReferenceNotFound = "ReferenceNotFound"
	ReasonReferenceInvalid  = "ReferenceInvalid"
	ReasonResolutionFailed  = "ResolutionFailed"

	maxToolCount                       = 64
	maxScopes                          = 32
	maxScopeBytes                      = 256
	maxAuthorizeParameterValueBytes    = 512
	maxAuthorizeParametersEncodedBytes = 2048
	maxEndpointQueryBytes              = 1024
	maxScopesEncodedBytes              = 2048
	maxClientIDBytes                   = 256
	maxEndpointURLBytes                = 2048
	maxToolHeaderNameBytes             = 128
	maxToolHeaderValueBytes            = 1024
	maxToolHeadersBytes                = 4096

	MaxHTTPToolTimeout = 10 * time.Minute
)

var (
	toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	reservedAuthorizeParameters = map[string]struct{}{
		"client_id": {}, "client_secret": {}, "redirect_uri": {}, "response_type": {},
		"scope": {}, "state": {}, "code_challenge": {}, "code_challenge_method": {},
		"code": {}, "code_verifier": {}, "grant_type": {}, "refresh_token": {},
	}

	deniedHosts = []string{"metadata.google.internal", "kubernetes.default", "localhost"}

	deniedHostSuffixes = []string{".svc", ".cluster.local", ".local", ".localhost", ".internal", ".localdomain", ".home.arpa"}

	reservedToolHeaders = map[string]struct{}{
		"Authorization": {}, "Cookie": {}, "Host": {}, "Txn-Token": {}, "Proxy-Authorization": {},
		"Content-Length": {}, "Transfer-Encoding": {},
		"Connection": {}, "Keep-Alive": {}, "Proxy-Connection": {}, "Te": {}, "Trailer": {}, "Upgrade": {},
	}
)

var allowPrivateEndpoints atomic.Bool

func SetAllowPrivateEndpoints(allowed bool) { allowPrivateEndpoints.Store(allowed) }

func PrivateEndpointsAllowed() bool { return allowPrivateEndpoints.Load() }

func hostDenied(host string) bool {
	if net.ParseIP(host) == nil && !strings.Contains(host, ".") {
		return true
	}
	for _, denied := range deniedHosts {
		if host == denied || strings.HasSuffix(host, "."+denied) || strings.HasPrefix(host, denied+".") {
			return true
		}
	}
	for _, suffix := range deniedHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return strings.Contains(host, ".svc.")
}

func InfrastructureHostDenied(host string) bool {
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	for _, denied := range []string{"metadata.google.internal", "metadata", "kubernetes.default", "kubernetes.default.svc"} {
		if host == denied || strings.HasPrefix(host, "kubernetes.default.svc.") {
			return true
		}
	}
	if apiHost := strings.ToLower(strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))); apiHost != "" && host == strings.Trim(apiHost, "[]") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return InfrastructureAddressDenied(ip)
	}
	return false
}

func InfrastructureAddressDenied(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	if ip.Equal(net.ParseIP("100.100.100.200")) {
		return true
	}
	if apiHost := net.ParseIP(strings.Trim(strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")), "[]")); apiHost != nil && ip.Equal(apiHost) {
		return true
	}
	return false
}

func PrivateEndpointDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if InfrastructureHostDenied(host) {
		return nil, fmt.Errorf("refusing to dial infrastructure host %q", host)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, candidate := range addresses {
		if InfrastructureAddressDenied(candidate.IP) {
			return nil, fmt.Errorf("refusing to dial %q: it resolves to the infrastructure address %s", host, candidate.IP)
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, candidate := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses for %q", host)
	}
	return nil, lastErr
}

func validHeaderToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

type Issue struct {
	Reason  string
	Message string
}

func (i *Issue) Error() string {
	if i == nil {
		return "connector validation failed"
	}
	return i.Message
}

func invalid(message string) *Issue {
	return &Issue{Reason: ReasonInvalidProvider, Message: message}
}

func unresolved(reason, message string) *Issue {
	return &Issue{Reason: reason, Message: message}
}

type BuiltinToolCheck func(name string) bool

const maxProviderNameLength = 63

func ValidateProviderSpec(provider *corev1alpha1.ConnectorProvider, knownBuiltin BuiltinToolCheck) *Issue {
	if provider == nil {
		return invalid("provider is required")
	}
	if len(provider.Name) > maxProviderNameLength {
		return invalid("provider name must be at most 63 characters")
	}
	oauth := provider.Spec.OAuth
	for _, endpoint := range []struct{ name, value string }{
		{"authorizeURL", oauth.AuthorizeURL},
		{"tokenURL", oauth.TokenURL},
	} {
		if issue := validateEndpointURL(endpoint.name, endpoint.value, true); issue != nil {
			return issue
		}
	}
	if issue := validateEndpointURL("revocationURL", oauth.RevocationURL, false); issue != nil {
		return issue
	}
	if strings.TrimSpace(oauth.RevocationURL) != "" && !SameEndpointHost(oauth.TokenURL, oauth.RevocationURL) {
		return invalid("oauth.revocationURL must be on the token endpoint's host; tokens are only sent back to their issuer")
	}
	if !validClientID(oauth.ClientID) {
		return invalid("oauth.clientID is required and must be printable ASCII without surrounding whitespace")
	}
	if len(oauth.ClientID) > maxClientIDBytes {
		return invalid(fmt.Sprintf("oauth.clientID must be at most %d bytes", maxClientIDBytes))
	}
	if strings.TrimSpace(oauth.ClientSecretRef.Name) == "" || strings.TrimSpace(oauth.ClientSecretRef.Key) == "" {
		return invalid("oauth.clientSecretRef requires name and key")
	}
	if len(k8svalidation.IsDNS1123Subdomain(oauth.ClientSecretRef.Name)) > 0 || len(k8svalidation.IsConfigMapKey(oauth.ClientSecretRef.Key)) > 0 {
		return invalid("oauth.clientSecretRef must name a valid Secret and data key")
	}
	switch oauth.ClientAuthentication {
	case "", corev1alpha1.ConnectorClientAuthSecretBasic, corev1alpha1.ConnectorClientAuthSecretPost:
	default:
		return invalid("oauth.clientAuthentication must be ClientSecretBasic or ClientSecretPost")
	}
	if issue := validateScopes("read", oauth.Scopes.Read); issue != nil {
		return issue
	}
	if issue := validateScopes("write", oauth.Scopes.Write); issue != nil {
		return issue
	}
	if len(url.QueryEscape(strings.Join(ScopesForMode(provider, corev1alpha1.ConnectionModeReadWrite), " "))) > maxScopesEncodedBytes {
		return invalid(fmt.Sprintf("oauth.scopes must encode to at most %d bytes in total", maxScopesEncodedBytes))
	}
	for key, value := range oauth.AdditionalAuthorizeParameters {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" || normalized != key {
			return invalid("oauth.additionalAuthorizeParameters keys must be lowercase without whitespace")
		}
		if _, reserved := reservedAuthorizeParameters[normalized]; reserved {
			return invalid("oauth.additionalAuthorizeParameters must not contain reserved OAuth fields")
		}
		if credentialLikeParameter(normalized) {
			return invalid("oauth.additionalAuthorizeParameters must not carry credentials; the spec is public configuration")
		}
		if !validAuthorizeParameterValue(value) {
			return invalid("oauth.additionalAuthorizeParameters values must be printable text without control bytes")
		}
		if len(value) > maxAuthorizeParameterValueBytes {
			return invalid(fmt.Sprintf("oauth.additionalAuthorizeParameters values must be at most %d bytes", maxAuthorizeParameterValueBytes))
		}
	}
	encoded := url.Values{}
	for key, value := range oauth.AdditionalAuthorizeParameters {
		encoded.Set(key, value)
	}
	if len(encoded.Encode()) > maxAuthorizeParametersEncodedBytes {
		return invalid(fmt.Sprintf("oauth.additionalAuthorizeParameters must encode to at most %d bytes", maxAuthorizeParametersEncodedBytes))
	}
	return validateTools(provider, knownBuiltin)
}

func validateEndpointURL(field, raw string, required bool) *Issue {
	return validateURL(field, raw, required, true)
}

func validateURL(field, raw string, required, oauthEndpoint bool) *Issue {
	if strings.TrimSpace(raw) == "" {
		if required {
			return invalid(fmt.Sprintf("oauth.%s is required", field))
		}
		return nil
	}
	if raw != strings.TrimSpace(raw) {
		return invalid(fmt.Sprintf("oauth.%s must not contain surrounding whitespace", field))
	}
	if len(raw) > maxEndpointURLBytes {
		return invalid(fmt.Sprintf("oauth.%s must be at most %d bytes", field, maxEndpointURLBytes))
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != schemeHTTPS || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(raw, "#") || parsed.String() != raw {
		return invalid(fmt.Sprintf("oauth.%s must be an absolute HTTPS URL without userinfo or fragment", field))
	}
	host := parsed.Hostname()
	if host == "" || strings.HasSuffix(host, ".") || strings.Contains(host, "%") {
		return invalid(fmt.Sprintf("oauth.%s host must not be empty, end with a dot, or carry an IPv6 zone", field))
	}
	if port := parsed.Port(); port != "" {
		if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
			return invalid(fmt.Sprintf("oauth.%s port must be between 1 and 65535", field))
		}
	}
	if !asciiHost(host) {
		return invalid(fmt.Sprintf("oauth.%s host must be ASCII; use the punycode (xn--) form of an internationalized name", field))
	}
	if net.ParseIP(host) == nil && !validDNSName(host) {
		return invalid(fmt.Sprintf("oauth.%s host must be a valid DNS name", field))
	}
	if ip := net.ParseIP(host); ip == nil && nonCanonicalNumericHost(host) {
		return invalid(fmt.Sprintf("oauth.%s host must be a hostname or a canonical IP address", field))
	}
	if !PrivateEndpointsAllowed() {
		if hostDenied(strings.ToLower(host)) {
			return invalid(fmt.Sprintf("oauth.%s host is not allowed", field))
		}
		if ip := net.ParseIP(host); ip != nil && !tokenexchange.IsPublicAddress(ip) {
			return invalid(fmt.Sprintf("oauth.%s must not target private, loopback, or link-local addresses", field))
		}
	}
	if InfrastructureHostDenied(host) {
		return invalid(fmt.Sprintf("oauth.%s host is not allowed", field))
	}
	return validateEndpointQuery(field, parsed.RawQuery, oauthEndpoint)
}

func validDNSName(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !dnsLabelByte(label[i]) {
				return false
			}
		}
	}
	return true
}

func dnsLabelByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		return true
	}
	return false
}

func asciiHost(host string) bool {
	for i := 0; i < len(host); i++ {
		if host[i] <= ' ' || host[i] > '~' {
			return false
		}
	}
	return true
}

func nonCanonicalNumericHost(host string) bool {
	labels := strings.Split(strings.ToLower(host), ".")
	last := labels[len(labels)-1]
	if last == "" {
		return false
	}
	allDigits := true
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return true
	}
	hexOnly := true
	hasDigit := false
	for _, label := range labels {
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= '0' && c <= '9':
				hasDigit = true
			case (c >= 'a' && c <= 'f') || c == 'x':
			default:
				hexOnly = false
			}
		}
	}
	return hexOnly && hasDigit && strings.HasPrefix(last, "0x")
}

func validateEndpointQuery(field, rawQuery string, oauthEndpoint bool) *Issue {
	if rawQuery == "" {
		return nil
	}
	if len(rawQuery) > maxEndpointQueryBytes {
		return invalid(fmt.Sprintf("oauth.%s query must be at most %d bytes", field, maxEndpointQueryBytes))
	}
	if strings.Contains(rawQuery, ";") {
		return invalid(fmt.Sprintf("oauth.%s query must not use semicolon separators", field))
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return invalid(fmt.Sprintf("oauth.%s query must be well-formed", field))
	}
	for key := range values {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if credentialLikeParameter(normalized) {
			return invalid(fmt.Sprintf("oauth.%s query must not carry credentials; the spec is public configuration", field))
		}
		if _, reserved := reservedAuthorizeParameters[normalized]; reserved && oauthEndpoint {
			return invalid(fmt.Sprintf("oauth.%s query must not preset reserved OAuth fields", field))
		}
	}
	return nil
}

func credentialLikeParameter(name string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	if slices.Contains([]string{"sig", "signature", "assertion", "key", "auth", "x-auth", "pin", "passcode"}, normalized) {
		return true
	}
	for _, fragment := range []string{
		"secret", "password", "passwd", "credential", "authorization", "api-key", "apikey", "token", "assertion", "signature",
		"auth-", "-auth", "access-key", "private-key", "secret-key", "session", "cookie", "bearer", "jwt",
		"-key", "key-",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	compact := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, normalized)
	if slices.Contains([]string{"xauth", "authkey", "authcode"}, compact) {
		return true
	}
	for _, fragment := range []string{"authentication", "accesskey", "privatekey", "secretkey", "signingkey", "passphrase"} {
		if strings.Contains(compact, fragment) {
			return true
		}
	}
	return false
}

func validateScopes(group string, scopes []string) *Issue {
	if len(scopes) > maxScopes {
		return invalid(fmt.Sprintf("oauth.scopes.%s has too many entries", group))
	}
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		if !validScopeToken(scope) {
			return invalid(fmt.Sprintf("oauth.scopes.%s entries must be non-empty RFC 6749 scope tokens", group))
		}
		if len(scope) > maxScopeBytes {
			return invalid(fmt.Sprintf("oauth.scopes.%s entries must be at most %d bytes", group, maxScopeBytes))
		}
		if _, dup := seen[scope]; dup {
			return invalid(fmt.Sprintf("oauth.scopes.%s contains a duplicate scope", group))
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func validAuthorizeParameterValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7F {
			return false
		}
	}
	return true
}

func validClientID(clientID string) bool {
	if clientID == "" || clientID != strings.TrimSpace(clientID) {
		return false
	}
	for i := 0; i < len(clientID); i++ {
		if clientID[i] < 0x20 || clientID[i] > 0x7E {
			return false
		}
	}
	return true
}

func validScopeToken(scope string) bool {
	if scope == "" {
		return false
	}
	for i := 0; i < len(scope); i++ {
		b := scope[i]
		if b == ',' {
			return false
		}
		if b == 0x21 || (b >= 0x23 && b <= 0x5B) || (b >= 0x5D && b <= 0x7E) {
			continue
		}
		return false
	}
	return true
}

func validateTools(provider *corev1alpha1.ConnectorProvider, knownBuiltin BuiltinToolCheck) *Issue {
	tools := provider.Spec.Tools
	if len(tools) == 0 && provider.Spec.AgentHost == nil {
		return invalid("tools requires at least one entry unless the provider serves agent hosts")
	}
	if host := provider.Spec.AgentHost; host != nil {
		if strings.TrimSpace(host.TemplateRef.Name) == "" {
			return invalid("agentHost.templateRef.name is required")
		}
		if endpoint, err := url.Parse(provider.Spec.OAuth.TokenURL); err != nil || endpoint.Host != "github.com" {
			return invalid("agentHost requires a github.com provider: oauth.tokenURL must be on github.com")
		}
	}
	if len(tools) > maxToolCount {
		return invalid("tools has too many entries")
	}
	seen := map[string]struct{}{}
	for _, tool := range tools {
		if !toolNamePattern.MatchString(tool.Name) {
			return invalid("tool names must be lowercase snake_case identifiers")
		}
		if _, dup := seen[tool.Name]; dup {
			return invalid(fmt.Sprintf("tool %q is declared more than once", tool.Name))
		}
		seen[tool.Name] = struct{}{}
		switch tool.Class {
		case corev1alpha1.ConnectorToolClassRead, corev1alpha1.ConnectorToolClassWrite:
		default:
			return invalid(fmt.Sprintf("tool %q class must be read or write", tool.Name))
		}
		switch tool.Source {
		case corev1alpha1.ConnectorToolSourceBuiltin:
			if tool.HTTP != nil || tool.Description != "" || tool.Parameters != nil {
				return invalid(fmt.Sprintf("builtin tool %q must not set http, description, or parameters", tool.Name))
			}
			if knownBuiltin != nil && !knownBuiltin(tool.Name) {
				return invalid(fmt.Sprintf("builtin tool %q is not a known Orka tool", tool.Name))
			}
			class, linked := BuiltinConnectorToolClass(tool.Name)
			if !linked {
				return invalid(fmt.Sprintf("builtin tool %q cannot use a linked account; only %s can", tool.Name, strings.Join(BuiltinConnectorToolNames(), ", ")))
			}
			if tool.Class != class {
				return invalid(fmt.Sprintf("builtin tool %q must be declared with class %s", tool.Name, class))
			}
			if !ProviderIssuesGitHubCredentials(provider) {
				return invalid(fmt.Sprintf("builtin tool %q sends its credential to %s; only a %s OAuth provider may declare it", tool.Name, BuiltinConnectorToolAudience, builtinConnectorIssuerHost))
			}
		case corev1alpha1.ConnectorToolSourceHTTP:
			if tool.HTTP == nil {
				return invalid(fmt.Sprintf("HTTP tool %q requires http", tool.Name))
			}
			if knownBuiltin != nil && knownBuiltin(tool.Name) {
				return invalid(fmt.Sprintf("HTTP tool %q collides with a built-in Orka tool name", tool.Name))
			}
			if strings.TrimSpace(tool.Description) == "" {
				return invalid(fmt.Sprintf("HTTP tool %q requires a description", tool.Name))
			}
			if issue := validateToolParameters(tool.Name, tool.Parameters); issue != nil {
				return issue
			}
			if issue := validateHTTPTool(tool.Name, *tool.HTTP); issue != nil {
				return issue
			}
			if issue := validateForwardingTool(tool); issue != nil {
				return issue
			}
		default:
			return invalid(fmt.Sprintf("tool %q source must be Builtin or HTTP", tool.Name))
		}
	}
	return nil
}

func validateForwardingTool(tool corev1alpha1.ConnectorTool) *Issue {
	forward := tool.HTTP.Forward
	if forward == nil {
		return nil
	}
	if forward.Protocol != corev1alpha1.HTTPForwardProtocolAzureResourceManager {
		return invalid(fmt.Sprintf("HTTP tool %q names an unsupported forwarding protocol", tool.Name))
	}
	if tool.Class != corev1alpha1.ConnectorToolClassRead {
		return invalid(fmt.Sprintf("forwarding tool %q must be class read", tool.Name))
	}
	if tool.Parameters != nil && len(tool.Parameters.Raw) > 0 {
		return invalid(fmt.Sprintf("forwarding tool %q declares no parameters", tool.Name))
	}
	if len(tool.HTTP.Headers) > 0 {
		return invalid(fmt.Sprintf("forwarding tool %q sets no headers", tool.Name))
	}
	if _, err := harnessv2.ForwardOrigin(tool.HTTP.URL); err != nil {
		return invalid(fmt.Sprintf("forwarding tool %q url must be an https origin with no path", tool.Name))
	}
	return nil
}

func validateToolParameters(name string, parameters *apiextensionsv1.JSON) *Issue {
	if parameters == nil || len(parameters.Raw) == 0 {
		return nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(parameters.Raw, &schema); err != nil || schema == nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a JSON Schema object", name))
	}
	var typeName string
	if raw, ok := schema["type"]; !ok || json.Unmarshal(raw, &typeName) != nil || typeName != "object" {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must declare type \"object\"", name))
	}
	if raw, ok := schema["properties"]; ok {
		var properties map[string]json.RawMessage
		if err := json.Unmarshal(raw, &properties); err != nil {
			return invalid(fmt.Sprintf("HTTP tool %q parameters.properties must be an object", name))
		}
	}
	if raw, ok := schema["required"]; ok {
		var required []string
		if err := json.Unmarshal(raw, &required); err != nil {
			return invalid(fmt.Sprintf("HTTP tool %q parameters.required must be an array of strings", name))
		}
	}
	var full jsonschema.Schema
	if err := json.Unmarshal(parameters.Raw, &full); err != nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a valid JSON Schema", name))
	}
	if _, err := full.Resolve(nil); err != nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a resolvable JSON Schema with only local references", name))
	}
	if numbersExactlyRepresentable(parameters.Raw) != nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must use only numbers a float64 holds exactly", name))
	}
	return nil
}

func validateHTTPTool(name string, spec corev1alpha1.ConnectorHTTPTool) *Issue {
	if issue := validateURL("tools."+name+".url", spec.URL, true, false); issue != nil {
		return &Issue{Reason: ReasonInvalidProvider, Message: strings.Replace(issue.Message, "oauth.", "", 1)}
	}
	if strings.Contains(spec.URL, "{{") || strings.Contains(spec.URL, "}}") || strings.Contains(spec.URL, "%7B%7B") || strings.Contains(spec.URL, "%7b%7b") {
		return invalid(fmt.Sprintf("HTTP tool %q url must not contain template placeholders; it is the exact destination", name))
	}
	switch spec.Method {
	case "", http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return invalid(fmt.Sprintf("HTTP tool %q method is not supported", name))
	}
	headerBytes := 0
	for key, value := range spec.Headers {
		if !validHeaderToken(key) {
			return invalid(fmt.Sprintf("HTTP tool %q header names must be valid HTTP tokens", name))
		}
		canonical := http.CanonicalHeaderKey(key)
		if canonical != key {
			return invalid(fmt.Sprintf("HTTP tool %q header names must be in canonical form", name))
		}
		if _, reserved := reservedToolHeaders[canonical]; reserved {
			return invalid(fmt.Sprintf("HTTP tool %q may not set the %s header", name, canonical))
		}
		if credentialLikeParameter(canonical) {
			return invalid(fmt.Sprintf("HTTP tool %q has a header whose name looks like a credential; the linked account is the only credential", name))
		}
		if !validHeaderValue(value) {
			return invalid(fmt.Sprintf("HTTP tool %q header values must not contain control bytes", name))
		}
		if len(key) > maxToolHeaderNameBytes || len(value) > maxToolHeaderValueBytes {
			return invalid(fmt.Sprintf("HTTP tool %q header names must be at most %d bytes and values at most %d bytes", name, maxToolHeaderNameBytes, maxToolHeaderValueBytes))
		}
		headerBytes += len(key) + len(value)
	}
	if headerBytes > maxToolHeadersBytes {
		return invalid(fmt.Sprintf("HTTP tool %q headers must total at most %d bytes", name, maxToolHeadersBytes))
	}
	if spec.Timeout != nil && (spec.Timeout.Duration <= 0 || spec.Timeout.Duration > MaxHTTPToolTimeout) {
		return invalid(fmt.Sprintf("HTTP tool %q timeout must be positive and at most %s", name, MaxHTTPToolTimeout))
	}
	return nil
}

func ResolveProviderReferences(ctx context.Context, reader client.Reader, provider *corev1alpha1.ConnectorProvider) (*Issue, error) {
	if reader == nil {
		return nil, errors.New("connector reference reader is required")
	}
	if provider == nil {
		return invalid("provider is required"), nil
	}
	ref := provider.Spec.OAuth.ClientSecretRef
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: provider.Namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return unresolved(ReasonReferenceNotFound, "client secret Secret was not found"), nil
		}
		return nil, fmt.Errorf("resolve connector client secret: %w", err)
	}
	if value, ok := secret.Data[ref.Key]; !ok || len(strings.TrimSpace(string(value))) == 0 {
		return unresolved(ReasonReferenceInvalid, "client secret Secret key was not found or is empty"), nil
	}
	if host := provider.Spec.AgentHost; host != nil {
		template := &corev1alpha1.AgentHostTemplate{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: provider.Namespace, Name: host.TemplateRef.Name}, template); err != nil {
			if apierrors.IsNotFound(err) {
				return unresolved(ReasonReferenceNotFound, "agentHost template was not found"), nil
			}
			return nil, fmt.Errorf("resolve agent host template: %w", err)
		}
		if template.Spec.Access != corev1alpha1.AgentHostAccessRelay {
			return unresolved(ReasonReferenceInvalid, "agentHost template must use access: Relay"), nil
		}
		if err := agenthost.ValidateTemplate(template); err != nil {
			return unresolved(ReasonReferenceInvalid, fmt.Sprintf("agentHost template is invalid: %v", err)), nil
		}
	}
	return nil, nil
}

func ProviderReferencesSecret(provider *corev1alpha1.ConnectorProvider, secret client.Object) bool {
	if provider == nil || secret == nil {
		return false
	}
	return secret.GetNamespace() == provider.Namespace && secret.GetName() == provider.Spec.OAuth.ClientSecretRef.Name
}

func ProviderAccepted(provider *corev1alpha1.ConnectorProvider) bool {
	if provider == nil || !provider.DeletionTimestamp.IsZero() {
		return false
	}
	for _, conditionType := range []string{
		corev1alpha1.ConnectorProviderConditionAccepted,
		corev1alpha1.ConnectorProviderConditionResolvedRefs,
	} {
		condition := meta.FindStatusCondition(provider.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != provider.Generation {
			return false
		}
	}
	return true
}

func ToolsForMode(provider *corev1alpha1.ConnectorProvider, mode string) []corev1alpha1.ConnectorTool {
	if provider == nil {
		return nil
	}
	result := make([]corev1alpha1.ConnectorTool, 0, len(provider.Spec.Tools))
	for _, tool := range provider.Spec.Tools {
		if tool.Class == corev1alpha1.ConnectorToolClassWrite && mode != corev1alpha1.ConnectionModeReadWrite {
			continue
		}
		result = append(result, tool)
	}
	return result
}

func ScopesCover(granted, required []string) bool {
	have := make(map[string]struct{}, len(granted))
	for _, scope := range granted {
		have[scope] = struct{}{}
	}
	for _, scope := range required {
		if _, ok := have[scope]; !ok {
			return false
		}
	}
	return true
}

func ConnectionLinked(connection *corev1alpha1.Connection) bool {
	if connection == nil || !connection.DeletionTimestamp.IsZero() {
		return false
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return false
	}
	for _, conditionType := range []string{corev1alpha1.ConnectionConditionProviderResolved, corev1alpha1.ConnectionConditionScopesGranted} {
		condition := meta.FindStatusCondition(connection.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != connection.Generation {
			return false
		}
	}
	return true
}

func ProviderAuthorityDigest(provider *corev1alpha1.ConnectorProvider) string {
	if provider == nil {
		return ""
	}
	parts := providerConsentParts(provider)
	tools := make([]corev1alpha1.ConnectorTool, 0, len(provider.Spec.Tools))
	for _, tool := range provider.Spec.Tools {
		if tool.Source == corev1alpha1.ConnectorToolSourceHTTP && tool.HTTP != nil {
			tools = append(tools, tool)
		}
	}
	slices.SortFunc(tools, func(a, b corev1alpha1.ConnectorTool) int { return strings.Compare(a.Name, b.Name) })
	for _, tool := range tools {
		parts = append(parts, "tool", tool.Name, string(tool.Class), tool.HTTP.URL, tool.HTTP.Method, "parameters", canonicalSchema(tool.Parameters))
		if tool.HTTP.Forward != nil {
			parts = append(parts, "forward", string(tool.HTTP.Forward.Protocol))
			if tool.HTTP.Forward.Writes != nil {
				parts = append(parts, "forward-writes")
			}
		}
		headerNames := make([]string, 0, len(tool.HTTP.Headers))
		for name := range tool.HTTP.Headers {
			headerNames = append(headerNames, name)
		}
		slices.Sort(headerNames)
		for _, name := range headerNames {
			parts = append(parts, "header", name, tool.HTTP.Headers[name])
		}
	}
	builtins := make([]corev1alpha1.ConnectorTool, 0, len(provider.Spec.Tools))
	for _, tool := range provider.Spec.Tools {
		if tool.Source == corev1alpha1.ConnectorToolSourceBuiltin {
			builtins = append(builtins, tool)
		}
	}
	slices.SortFunc(builtins, func(a, b corev1alpha1.ConnectorTool) int { return strings.Compare(a.Name, b.Name) })
	for _, tool := range builtins {
		parts = append(parts, "builtin", tool.Name, string(tool.Class), BuiltinConnectorToolAudience)
	}
	if host := provider.Spec.AgentHost; host != nil {
		parts = append(parts, "agentHost", host.TemplateRef.Name)
	}
	return lengthPrefixedDigest(parts)
}

func ProviderIssuerDigest(provider *corev1alpha1.ConnectorProvider) string {
	if provider == nil {
		return ""
	}
	return lengthPrefixedDigest(providerIssuerParts(provider))
}

func ProviderRevocationDigest(provider *corev1alpha1.ConnectorProvider) string {
	if provider == nil {
		return ""
	}
	oauth := provider.Spec.OAuth
	return lengthPrefixedDigest([]string{
		"revocation", "uid", string(provider.UID), "clientID", oauth.ClientID,
		"secretName", oauth.ClientSecretRef.Name, "secretKey", oauth.ClientSecretRef.Key,
		"clientAuthentication", oauth.ClientAuthentication, "revocationURL", oauth.RevocationURL,
	})
}

func providerIssuerParts(provider *corev1alpha1.ConnectorProvider) []string {
	oauth := provider.Spec.OAuth
	return []string{
		"uid", string(provider.UID), "clientID", oauth.ClientID,
		"secretName", oauth.ClientSecretRef.Name, "secretKey", oauth.ClientSecretRef.Key,
		"clientAuthentication", oauth.ClientAuthentication,
		"tokenURL", oauth.TokenURL,
	}
}

func providerConsentParts(provider *corev1alpha1.ConnectorProvider) []string {
	oauth := provider.Spec.OAuth
	pkce := true
	if oauth.PKCE != nil {
		pkce = *oauth.PKCE
	}
	parts := append(providerIssuerParts(provider), "authorizeURL", oauth.AuthorizeURL, "pkce", strconv.FormatBool(pkce))
	keys := make([]string, 0, len(oauth.AdditionalAuthorizeParameters))
	for key := range oauth.AdditionalAuthorizeParameters {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	parts = slices.Grow(parts, 3*len(keys))
	for _, key := range keys {
		parts = append(parts, "param", key, oauth.AdditionalAuthorizeParameters[key])
	}
	return parts
}

func canonicalSchema(schema *apiextensionsv1.JSON) string {
	if schema == nil || len(schema.Raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(schema.Raw, &value); err != nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func lengthPrefixedDigest(parts []string) string {
	sum := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(sum, "%d:", len(part))
		_, _ = sum.Write([]byte(part))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func ConsentFor(provider *corev1alpha1.ConnectorProvider) *corev1alpha1.ConnectionConsent {
	if provider == nil {
		return nil
	}
	return &corev1alpha1.ConnectionConsent{ProviderUID: string(provider.UID), AuthorityDigest: ProviderAuthorityDigest(provider)}
}

func ConsentMatchesProvider(connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider) bool {
	if connection == nil || provider == nil || connection.Status.Consent == nil {
		return false
	}
	consent := connection.Status.Consent
	return consent.ProviderUID == string(provider.UID) && consent.AuthorityDigest == ProviderAuthorityDigest(provider)
}

func ScopesForMode(provider *corev1alpha1.ConnectorProvider, mode string) []string {
	if provider == nil {
		return nil
	}
	scopes := append([]string(nil), provider.Spec.OAuth.Scopes.Read...)
	if mode == corev1alpha1.ConnectionModeReadWrite {
		for _, scope := range provider.Spec.OAuth.Scopes.Write {
			duplicate := slices.Contains(scopes, scope)
			if !duplicate {
				scopes = append(scopes, scope)
			}
		}
	}
	return scopes
}
