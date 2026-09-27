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
		"code": {}, "grant_type": {}, "refresh_token": {},
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
	}
)

// hostDenied reports whether host is a denied name, lies under one, extends
// one with more labels (kubernetes.default.svc.example), or carries a
// cluster-local service label. It expects a lowercase host without a
// trailing dot.
func hostDenied(host string) bool {
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
	if strings.TrimSpace(oauth.ClientID) == "" || oauth.ClientID != strings.TrimSpace(oauth.ClientID) {
		return invalid("oauth.clientID is required and must not contain surrounding whitespace")
	}
	if strings.TrimSpace(oauth.ClientSecretRef.Name) == "" || strings.TrimSpace(oauth.ClientSecretRef.Key) == "" {
		return invalid("oauth.clientSecretRef requires name and key")
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
		if strings.ContainsAny(value, "\r\n") {
			return invalid("oauth.additionalAuthorizeParameters values must not contain line breaks")
		}
	}
	return validateTools(provider.Spec.Tools, knownBuiltin)
}

func validateEndpointURL(field, raw string, required bool) *Issue {
	if strings.TrimSpace(raw) == "" {
		if required {
			return invalid(fmt.Sprintf("oauth.%s is required", field))
		}
		return nil
	}
	if raw != strings.TrimSpace(raw) {
		return invalid(fmt.Sprintf("oauth.%s must not contain surrounding whitespace", field))
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
	if hostDenied(strings.ToLower(host)) {
		return invalid(fmt.Sprintf("oauth.%s host is not allowed", field))
	}
	if ip := net.ParseIP(host); ip != nil && !tokenexchange.IsPublicAddress(ip) {
		return invalid(fmt.Sprintf("oauth.%s must not target private, loopback, or link-local addresses", field))
	}
	return validateEndpointQuery(field, parsed.RawQuery)
}

// validateEndpointQuery rejects malformed queries and credential-like query
// parameters on every endpoint, and flow-owned OAuth parameters on the
// authorization endpoint, so a preconfigured state, redirect URI, or secret
// can never ride along in public configuration.
func validateEndpointQuery(field, rawQuery string) *Issue {
	if rawQuery == "" {
		return nil
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
		if field == "authorizeURL" {
			if _, reserved := reservedAuthorizeParameters[normalized]; reserved {
				return invalid("oauth.authorizeURL query must not preset reserved OAuth fields")
			}
		}
		if credentialLikeParameter(normalized) {
			return invalid(fmt.Sprintf("oauth.%s query must not carry credentials; the spec is public configuration", field))
		}
	}
	return nil
}

// credentialLikeParameter reports whether a query or authorization parameter
// name looks like it carries a credential. Names are compared lowercased with
// underscores folded to hyphens.
func credentialLikeParameter(name string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	if slices.Contains([]string{"sig", "signature", "assertion", "key"}, normalized) {
		return true
	}
	for _, fragment := range []string{"secret", "password", "passwd", "credential", "authorization", "api-key", "apikey", "token", "assertion", "signature"} {
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
		if _, dup := seen[scope]; dup {
			return invalid(fmt.Sprintf("oauth.scopes.%s contains duplicate scope %q", group, scope))
		}
		seen[scope] = struct{}{}
	}
	return nil
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
	if raw, ok := schema["type"]; ok {
		var typeName string
		if err := json.Unmarshal(raw, &typeName); err != nil || typeName != "object" {
			return invalid(fmt.Sprintf("HTTP tool %q parameters must describe an object", name))
		}
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
	// would reject must not be accepted here.
	var full jsonschema.Schema
	if err := json.Unmarshal(parameters.Raw, &full); err != nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a valid JSON Schema: %s", name, err.Error()))
	}
	if _, err := full.Resolve(nil); err != nil {
		return invalid(fmt.Sprintf("HTTP tool %q parameters must be a resolvable JSON Schema: %s", name, err.Error()))
	}
	return nil
}

func validateHTTPTool(name string, spec corev1alpha1.ConnectorHTTPTool) *Issue {
	if issue := validateEndpointURL("tools."+name+".url", spec.URL, true); issue != nil {
		return &Issue{Reason: ReasonInvalidProvider, Message: strings.Replace(issue.Message, "oauth.", "", 1)}
	}
	switch spec.Method {
	case "", http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return invalid(fmt.Sprintf("HTTP tool %q method is not supported", name))
	}
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
		if !validHeaderValue(value) {
			return invalid(fmt.Sprintf("HTTP tool %q header values must not contain control bytes", name))
		}
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

// ProviderAuthorityDigest is a hex SHA-256 digest of the OAuth client identity
// a consent is granted against: the provider UID, client ID, client secret
// reference, authentication method, and endpoints. It carries no secret
// material and changes whenever a held token would belong to a different
// client or be sent to a different token endpoint.
func ProviderAuthorityDigest(provider *corev1alpha1.ConnectorProvider) string {
	if provider == nil {
		return ""
	}
	oauth := provider.Spec.OAuth
	sum := sha256.New()
	for _, part := range []string{
		string(provider.UID), oauth.ClientID, oauth.ClientSecretRef.Name, oauth.ClientSecretRef.Key,
		oauth.ClientAuthentication, oauth.AuthorizeURL, oauth.TokenURL, oauth.RevocationURL,
	} {
		_, _ = sum.Write([]byte(part))
		_, _ = sum.Write([]byte{0})
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
