/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package connectors validates ConnectorProvider and Connection resources and
// resolves their references. Everything here is status-safe: no Issue message
// carries a credential or a credential-bearing URL.
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/jsonschema-go/jsonschema"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

const (
	ReasonAccepted          = "Accepted"
	ReasonInvalidProvider   = "InvalidProvider"
	ReasonResolvedRefs      = "ResolvedRefs"
	ReasonReferenceNotFound = "ReferenceNotFound"
	ReasonReferenceInvalid  = "ReferenceInvalid"
	ReasonResolutionFailed  = "ResolutionFailed"

	maxToolCount = 64
	maxScopes    = 32
	// maxScopeBytes bounds one configured scope, matching the bound applied
	// to scopes a provider reports, so a scope can never push the
	// authorization URL past what browsers and proxies accept.
	maxScopeBytes = 256
	// maxAuthorizeParameterValueBytes and maxAuthorizeParametersEncodedBytes
	// keep static authorize parameters from pushing the authorization URL
	// past what browsers, proxies, and providers accept.
	maxAuthorizeParameterValueBytes    = 512
	maxAuthorizeParametersEncodedBytes = 2048
	maxEndpointQueryBytes              = 1024
	maxScopesEncodedBytes              = 2048
	// maxClientIDBytes bounds the client_id every authorization URL carries.
	maxClientIDBytes = 256
	// maxEndpointURLBytes bounds every endpoint and HTTP tool URL. With the
	// client ID, scope, and authorize-parameter bounds and the flow's own
	// parameters, an authorization URL stays under the 8 KiB request
	// target common servers and proxies accept.
	maxEndpointURLBytes = 2048
	// Static tool headers stay well inside common server header limits.
	maxToolHeaderNameBytes  = 128
	maxToolHeaderValueBytes = 1024
	maxToolHeadersBytes     = 4096

	// MaxHTTPToolTimeout bounds curated HTTP tool requests.
	MaxHTTPToolTimeout = 10 * time.Minute
)

var (
	toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	// reservedAuthorizeParameters are set by the consent flow and may not be
	// overridden by a provider.
	reservedAuthorizeParameters = map[string]struct{}{
		"client_id": {}, "client_secret": {}, "redirect_uri": {}, "response_type": {},
		"scope": {}, "state": {}, "code_challenge": {}, "code_challenge_method": {},
		"code": {}, "code_verifier": {}, "grant_type": {}, "refresh_token": {},
	}

	// deniedHosts are cluster-internal or metadata endpoints that must never be
	// an OAuth or tool endpoint. A host equal to one of these, or under it,
	// is rejected.
	deniedHosts = []string{"metadata.google.internal", "kubernetes.default", "localhost"}

	// deniedHostSuffixes cover cluster-local service names under any cluster
	// domain, mDNS, and other non-public name spaces. Names under these never
	// resolve to a public provider.
	deniedHostSuffixes = []string{".svc", ".cluster.local", ".local", ".localhost", ".internal", ".localdomain", ".home.arpa"}

	// reservedToolHeaders may not be set by a provider tool definition.
	reservedToolHeaders = map[string]struct{}{
		"Authorization": {}, "Cookie": {}, "Host": {}, "Txn-Token": {}, "Proxy-Authorization": {},
		"Content-Length": {}, "Transfer-Encoding": {},
		// Hop-by-hop fields: Connection could nominate the injected
		// Authorization header for removal by an intermediary, and all of
		// them are invalid on HTTP/2.
		"Connection": {}, "Keep-Alive": {}, "Proxy-Connection": {}, "Te": {}, "Trailer": {}, "Upgrade": {},
	}
)

// hostDenied reports whether host is a denied name, lies under one, extends
// one with more labels (kubernetes.default.svc.example), carries a
// cluster-local service label, or is a single-label name, which a Pod's DNS
// search domains complete to a cluster Service (kubernetes, orka-api). It
// expects a lowercase host without a trailing dot.
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
	// <service>.<namespace>.svc.<cluster-domain> under any cluster domain.
	return strings.Contains(host, ".svc.")
}

// validHeaderToken reports whether name is an RFC 9110 token, which is what
// net/http requires of a header field name. http.CanonicalHeaderKey returns
// invalid names unchanged, so canonical-form equality alone is not enough.
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

// validHeaderValue reports whether value contains only bytes net/http will
// serialize: visible ASCII, space, tab, and obs-text; no other control bytes.
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

// Issue is a status-safe validation failure.
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

// BuiltinToolCheck reports whether name is a known Orka built-in tool. A nil
// check accepts any well-formed name.
type BuiltinToolCheck func(name string) bool

// ValidateProviderSpec validates structural and security invariants of a
// ConnectorProvider without reading referenced objects.
func ValidateProviderSpec(provider *corev1alpha1.ConnectorProvider, knownBuiltin BuiltinToolCheck) *Issue {
	if provider == nil {
		return invalid("provider is required")
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
	if !validClientID(oauth.ClientID) {
		return invalid("oauth.clientID is required and must be printable ASCII without surrounding whitespace")
	}
	if len(oauth.ClientID) > maxClientIDBytes {
		return invalid(fmt.Sprintf("oauth.clientID must be at most %d bytes", maxClientIDBytes))
	}
	if strings.TrimSpace(oauth.ClientSecretRef.Name) == "" || strings.TrimSpace(oauth.ClientSecretRef.Key) == "" {
		return invalid("oauth.clientSecretRef requires name and key")
	}
	// A reference no Secret could ever have would fail every resolution
	// as a transient error; it is an invalid spec instead.
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
	// A readWrite consent requests both groups in one scope parameter of
	// the browser's authorization URL, so their encoded total is bounded.
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
	// The parameters ride in the browser's authorization URL, so their
	// encoded total is bounded like the scopes.
	encoded := url.Values{}
	for key, value := range oauth.AdditionalAuthorizeParameters {
		encoded.Set(key, value)
	}
	if len(encoded.Encode()) > maxAuthorizeParametersEncodedBytes {
		return invalid(fmt.Sprintf("oauth.additionalAuthorizeParameters must encode to at most %d bytes", maxAuthorizeParametersEncodedBytes))
	}
	return validateTools(provider.Spec.Tools, knownBuiltin)
}

func validateEndpointURL(field, raw string, required bool) *Issue {
	return validateURL(field, raw, required, true)
}

// validateURL checks an OAuth endpoint (oauthEndpoint) or an HTTP tool URL.
// Both follow the same host rules; only OAuth endpoints refuse flow-owned
// query names, since a tool's API may use names such as state as filters.
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
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(raw, "#") || parsed.String() != raw {
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
	// Clients IDNA-normalize a non-ASCII host before resolving it, so
	// 127。0。0。1 (ideographic full stops, possibly percent-encoded)
	// dials loopback while matching none of the checks below. Hosts must
	// arrive in their ASCII (punycode) form.
	if !asciiHost(host) {
		return invalid(fmt.Sprintf("oauth.%s host must be ASCII; use the punycode (xn--) form of an internationalized name", field))
	}
	if net.ParseIP(host) == nil && !validDNSName(host) {
		return invalid(fmt.Sprintf("oauth.%s host must be a valid DNS name", field))
	}
	if ip := net.ParseIP(host); ip == nil && nonCanonicalNumericHost(host) {
		return invalid(fmt.Sprintf("oauth.%s host must be a hostname or a canonical IP address", field))
	}
	if hostDenied(strings.ToLower(host)) {
		return invalid(fmt.Sprintf("oauth.%s host is not allowed", field))
	}
	if ip := net.ParseIP(host); ip != nil && !tokenexchange.IsPublicAddress(ip) {
		return invalid(fmt.Sprintf("oauth.%s must not target private, loopback, or link-local addresses", field))
	}
	return validateEndpointQuery(field, parsed.RawQuery, oauthEndpoint)
}

// validDNSName reports whether host is a syntactically valid DNS name:
// at most 253 bytes of dot-separated labels of 1 to 63 letters, digits,
// hyphens, or underscores, none starting or ending with a hyphen. A name
// that fails this can never resolve, so it is refused rather than accepted
// and left permanently unusable.
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

// asciiHost reports whether host is printable ASCII.
func asciiHost(host string) bool {
	for i := 0; i < len(host); i++ {
		if host[i] <= ' ' || host[i] > '~' {
			return false
		}
	}
	return true
}

// nonCanonicalNumericHost reports whether host is a numeric spelling that
// net.ParseIP rejects but resolvers accept, such as 127.1, 2130706433,
// 0177.0.0.1, or 0x7f000001. The authorize URL is opened by the person's
// browser, not the guarded dialer, so such spellings must not slip past the
// loopback check. Hostnames whose last label contains a non-hex letter, and
// labels without digits, are ordinary names.
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

// validateEndpointQuery rejects malformed queries and credential-like query
// parameters on every endpoint, and flow-owned OAuth parameters on the
// authorization endpoint, so a preconfigured state, redirect URI, or secret
// can never ride along in public configuration.
func validateEndpointQuery(field, rawQuery string, oauthEndpoint bool) *Issue {
	if rawQuery == "" {
		return nil
	}
	// The authorization URL is opened by the browser and tool URLs carry
	// the person's token; a static query is bounded so neither can exceed
	// what browsers, proxies, and providers accept.
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
		// Flow-owned fields (code, code_verifier, grant_type, state, ...) are
		// never legitimate static configuration on an OAuth endpoint.
		if _, reserved := reservedAuthorizeParameters[normalized]; reserved && oauthEndpoint {
			return invalid(fmt.Sprintf("oauth.%s query must not preset reserved OAuth fields", field))
		}
	}
	return nil
}

// credentialLikeParameter reports whether a query or authorization parameter
// name looks like it carries a credential. Names are compared lowercased with
// underscores folded to hyphens.
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
		// The value is not repeated: this message becomes public status.
		if _, dup := seen[scope]; dup {
			return invalid(fmt.Sprintf("oauth.scopes.%s contains a duplicate scope", group))
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// validAuthorizeParameterValue rejects control bytes (including NUL, CR, and
// LF) in a static authorize parameter value.
func validAuthorizeParameterValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7F {
			return false
		}
	}
	return true
}

// validClientID applies the RFC 6749 client identifier grammar (VSCHAR,
// %x20-7E) and forbids surrounding whitespace.
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

// validScopeToken applies the RFC 6749 scope-token grammar: one or more bytes
// in %x21 / %x23-5B / %x5D-7E, which excludes whitespace, quotes, backslashes,
// control bytes, and non-ASCII text.
func validScopeToken(scope string) bool {
	if scope == "" {
		return false
	}
	for i := 0; i < len(scope); i++ {
		b := scope[i]
		if b == 0x21 || (b >= 0x23 && b <= 0x5B) || (b >= 0x5D && b <= 0x7E) {
			continue
		}
		return false
	}
	return true
}

func validateTools(tools []corev1alpha1.ConnectorTool, knownBuiltin BuiltinToolCheck) *Issue {
	if len(tools) == 0 {
		return invalid("tools requires at least one entry")
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
		case corev1alpha1.ConnectorToolSourceHTTP:
			if tool.HTTP == nil {
				return invalid(fmt.Sprintf("HTTP tool %q requires http", tool.Name))
			}
			// Tool selection and execution are name-based: an HTTP tool
			// named like a built-in would shadow or be shadowed by it.
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
		default:
			return invalid(fmt.Sprintf("tool %q source must be Builtin or HTTP", tool.Name))
		}
	}
	return nil
}

// validateToolParameters requires an object-shaped JSON Schema, which is the
// only shape LLM tool definitions accept.
func validateToolParameters(name string, parameters *apiextensionsv1.JSON) *Issue {
	if parameters == nil || len(parameters.Raw) == 0 {
		return nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(parameters.Raw, &schema); err != nil || schema == nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a JSON Schema object", name))
	}
	// The root type is required: without it JSON Schema accepts non-object
	// inputs, which the HTTP executor cannot map onto request arguments.
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
	// The execution path resolves the whole schema, so a nested shape it
	// would reject must not be accepted here. The library's error text is
	// not repeated: it can quote schema values and whole $ref URLs, and
	// this message becomes public provider status.
	var full jsonschema.Schema
	if err := json.Unmarshal(parameters.Raw, &full); err != nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a valid JSON Schema", name))
	}
	if _, err := full.Resolve(nil); err != nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a resolvable JSON Schema with only local references", name))
	}
	return nil
}

func validateHTTPTool(name string, spec corev1alpha1.ConnectorHTTPTool) *Issue {
	if issue := validateURL("tools."+name+".url", spec.URL, true, false); issue != nil {
		return &Issue{Reason: ReasonInvalidProvider, Message: strings.Replace(issue.Message, "oauth.", "", 1)}
	}
	// The curated URL is the exact destination the person consented to:
	// Tool URL templates would let call arguments rewrite its path or query.
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
			// The header name is not repeated: a credential pasted into it
			// must not travel into public provider status.
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

// ResolveProviderReferences verifies that the client secret Secret exists in
// the provider namespace and carries a non-empty key. The value is never
// returned.
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
	return nil, nil
}

// ProviderReferencesSecret reports whether the provider's client secret lives
// in the given Secret.
func ProviderReferencesSecret(provider *corev1alpha1.ConnectorProvider, secret client.Object) bool {
	if provider == nil || secret == nil {
		return false
	}
	return secret.GetNamespace() == provider.Namespace && secret.GetName() == provider.Spec.OAuth.ClientSecretRef.Name
}

// ProviderAccepted reports whether a provider's Accepted and ResolvedRefs
// conditions are True for its current generation and it is not being deleted.
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

// ToolsForMode returns the provider tools a Connection in the given mode may
// use: read tools always, write tools only for readWrite.
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

// ScopesCover reports whether every required scope was granted. No required
// scopes means any grant suffices.
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

// ConnectionLinked reports whether a Connection currently holds usable
// material for its mode: consent completed (Ready), the provider resolves
// (ProviderResolved), and the granted scopes cover the mode (ScopesGranted).
// The two controller-owned conditions must have observed the current
// generation, so a mode change or provider loss fails closed until the
// controller has judged it.
//
// This is a status view, and status trails the provider by one reconcile:
// after a provider change the old conditions stay True until the Connection's
// watch-driven reconcile runs. A caller that releases token material must
// also load the provider and verify ProviderAccepted, the authority digest
// sealed with the credential, and the scopes the mode requires; the
// credential-injection path does exactly that.
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

// ProviderAuthorityDigest is a hex SHA-256 digest of everything a consent
// authorizes a token to reach: the provider UID, client ID, client secret
// reference, authentication method, endpoints, the static authorize
// parameters (which can select the resource server a token targets, such as
// an Auth0 audience), and the curated HTTP tool destinations the credential
// is sent to, including their static headers (which can change request
// semantics, such as a tenant-routing or method-override header). It carries no secret material and changes whenever a held token
// would belong to a different client, target a different resource, or be sent
// to a different endpoint, so such a change asks for consent again. Scopes
// are judged separately by ScopesGranted. Every field is length-prefixed, so
// the encoding is injective regardless of field contents.
func ProviderAuthorityDigest(provider *corev1alpha1.ConnectorProvider) string {
	if provider == nil {
		return ""
	}
	oauth := provider.Spec.OAuth
	parts := []string{
		"uid", string(provider.UID), "clientID", oauth.ClientID,
		"secretName", oauth.ClientSecretRef.Name, "secretKey", oauth.ClientSecretRef.Key,
		"clientAuthentication", oauth.ClientAuthentication,
		"authorizeURL", oauth.AuthorizeURL, "tokenURL", oauth.TokenURL, "revocationURL", oauth.RevocationURL,
	}
	keys := make([]string, 0, len(oauth.AdditionalAuthorizeParameters))
	for key := range oauth.AdditionalAuthorizeParameters {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		parts = append(parts, "param", key, oauth.AdditionalAuthorizeParameters[key])
	}
	tools := make([]corev1alpha1.ConnectorTool, 0, len(provider.Spec.Tools))
	for _, tool := range provider.Spec.Tools {
		if tool.Source == corev1alpha1.ConnectorToolSourceHTTP && tool.HTTP != nil {
			tools = append(tools, tool)
		}
	}
	slices.SortFunc(tools, func(a, b corev1alpha1.ConnectorTool) int { return strings.Compare(a.Name, b.Name) })
	for _, tool := range tools {
		parts = append(parts, "tool", tool.Name, string(tool.Class), tool.HTTP.URL, tool.HTTP.Method)
		headerNames := make([]string, 0, len(tool.HTTP.Headers))
		for name := range tool.HTTP.Headers {
			headerNames = append(headerNames, name)
		}
		slices.Sort(headerNames)
		for _, name := range headerNames {
			parts = append(parts, "header", name, tool.HTTP.Headers[name])
		}
	}
	sum := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(sum, "%d:", len(part))
		_, _ = sum.Write([]byte(part))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// ConsentFor returns the consent record to store when a consent completes
// against provider.
func ConsentFor(provider *corev1alpha1.ConnectorProvider) *corev1alpha1.ConnectionConsent {
	if provider == nil {
		return nil
	}
	return &corev1alpha1.ConnectionConsent{ProviderUID: string(provider.UID), AuthorityDigest: ProviderAuthorityDigest(provider)}
}

// ConsentMatchesProvider reports whether the Connection's recorded consent
// was granted against exactly this provider OAuth client. A missing record
// never matches.
func ConsentMatchesProvider(connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider) bool {
	if connection == nil || provider == nil || connection.Status.Consent == nil {
		return false
	}
	consent := connection.Status.Consent
	return consent.ProviderUID == string(provider.UID) && consent.AuthorityDigest == ProviderAuthorityDigest(provider)
}

// ScopesForMode returns the OAuth scopes to request for a Connection mode.
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
