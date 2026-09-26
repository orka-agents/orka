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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// an OAuth endpoint.
	deniedHosts = []string{"metadata.google.internal", "kubernetes.default", "kubernetes.default.svc", "localhost"}
)

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
	for _, denied := range deniedHosts {
		if strings.EqualFold(host, denied) {
			return invalid(fmt.Sprintf("oauth.%s host is not allowed", field))
		}
	}
	if ip := net.ParseIP(host); ip != nil && !tokenexchange.IsPublicAddress(ip) {
		return invalid(fmt.Sprintf("oauth.%s must not target private, loopback, or link-local addresses", field))
	}
	return nil
}

func validateScopes(group string, scopes []string) *Issue {
	if len(scopes) > maxScopes {
		return invalid(fmt.Sprintf("oauth.scopes.%s has too many entries", group))
	}
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		if scope == "" || scope != strings.TrimSpace(scope) || strings.ContainsAny(scope, " \t\r\n\"\\") {
			return invalid(fmt.Sprintf("oauth.scopes.%s entries must be non-empty scope tokens without whitespace", group))
		}
		if _, dup := seen[scope]; dup {
			return invalid(fmt.Sprintf("oauth.scopes.%s contains duplicate scope %q", group, scope))
		}
		seen[scope] = struct{}{}
	}
	return nil
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
			if issue := validateHTTPTool(tool.Name, *tool.HTTP); issue != nil {
				return issue
			}
		default:
			return invalid(fmt.Sprintf("tool %q source must be Builtin or HTTP", tool.Name))
		}
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
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if canonical == "" || canonical != key {
			return invalid(fmt.Sprintf("HTTP tool %q header names must be canonical without whitespace", name))
		}
		switch canonical {
		case "Authorization", "Cookie", "Host", "Txn-Token", "Proxy-Authorization":
			return invalid(fmt.Sprintf("HTTP tool %q may not set the %s header", name, canonical))
		}
		if strings.ContainsAny(value, "\r\n") {
			return invalid(fmt.Sprintf("HTTP tool %q header values must not contain line breaks", name))
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
