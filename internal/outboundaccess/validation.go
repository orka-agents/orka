/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
	"github.com/orka-agents/orka/internal/transactiontoken"
)

const (
	ReasonAccepted          = "Accepted"
	ReasonInvalidPolicy     = "InvalidPolicy"
	ReasonResolvedRefs      = "ResolvedRefs"
	ReasonReferenceNotFound = "ReferenceNotFound"
	ReasonReferenceDenied   = "ReferenceDenied"
	ReasonReferenceInvalid  = "ReferenceInvalid"
	ReasonResolutionFailed  = "ResolutionFailed"
)

// Issue is a status-safe validation failure. Message must never contain
// credentials or a credential-bearing URL.
type Issue struct {
	Reason  string
	Message string
}

func (i *Issue) Error() string {
	if i == nil {
		return "outbound access policy validation failed"
	}
	return i.Message
}

// ValidateSpec validates structural and security invariants without reading
// referenced objects.
func ValidateSpec(policy *corev1alpha1.OutboundAccessPolicy) *Issue {
	if policy == nil {
		return invalid("policy is required")
	}
	direct, gateway := policy.Spec.Direct, policy.Spec.Gateway
	if (direct == nil) == (gateway == nil) {
		return invalid("exactly one of direct or gateway is required")
	}
	if direct != nil {
		return validateDirectSpec(direct)
	}
	return validateGatewaySpec(gateway)
}

func validateDirectSpec(direct *corev1alpha1.DirectOutboundAccess) *Issue {
	grant := direct.Grant
	if grant == "" {
		grant = corev1alpha1.OutboundGrantTokenExchange
	}
	switch grant {
	case corev1alpha1.OutboundGrantTokenExchange:
		if issue := validateTokenSource(direct.Subject, false); issue != nil {
			return issue
		}
		if direct.Actor != nil {
			if issue := validateTokenSource(*direct.Actor, false); issue != nil {
				return issue
			}
		}
	case corev1alpha1.OutboundGrantJWTBearer:
		if direct.Actor != nil {
			return invalid("JWTBearer does not support an actor token")
		}
		if issue := validateTokenSource(direct.Subject, true); issue != nil {
			return issue
		}
	default:
		return invalid("grant must be TokenExchange or JWTBearer")
	}
	if issue := validateTokenEndpoint(direct.TokenEndpoint); issue != nil {
		return issue
	}
	if grant == corev1alpha1.OutboundGrantTokenExchange && strings.TrimSpace(direct.ExpectedIssuedTokenType) == "" {
		return invalid("TokenExchange requires expectedIssuedTokenType")
	}
	for name := range direct.AdditionalParameters {
		if strings.TrimSpace(name) == "" {
			return invalid("additional parameter names must not be empty")
		}
		if tokenexchange.IsReservedParameter(name) {
			return invalid(fmt.Sprintf("additional parameter %q is reserved", name))
		}
	}
	if issue := validateClientAuthentication(direct.ClientAuthentication); issue != nil {
		return issue
	}
	header := http.CanonicalHeaderKey("Authorization")
	if direct.Output != nil && strings.TrimSpace(direct.Output.Header) != "" {
		header = http.CanonicalHeaderKey(strings.TrimSpace(direct.Output.Header))
	}
	if header == "" || !validHeaderName(header) {
		return invalid("output header is invalid")
	}
	if strings.EqualFold(header, transactiontoken.HeaderName) {
		return invalid("Txn-Token cannot be used as the resource credential output header")
	}
	if direct.Output != nil && direct.Output.Prefix != nil && strings.ContainsAny(*direct.Output.Prefix, "\r\n") {
		return invalid("output prefix must not contain carriage returns or newlines")
	}
	return nil
}

func validateTokenEndpoint(endpoint corev1alpha1.OutboundTokenEndpoint) *Issue {
	if (strings.TrimSpace(endpoint.URL) == "") == (endpoint.ServiceRef == nil) {
		return invalid("tokenEndpoint requires exactly one of url or serviceRef")
	}
	if endpoint.URL != "" {
		if endpoint.URL != strings.TrimSpace(endpoint.URL) {
			return invalid("token endpoint URL must not contain surrounding whitespace")
		}
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || parsed.Scheme != schemeHTTPS || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(endpoint.URL, "#") || parsed.String() != strings.TrimSpace(endpoint.URL) {
			return invalid("token endpoint URL must be an absolute HTTPS URL without userinfo or fragment")
		}
		if endpoint.Path != "" || endpoint.Scheme != "" {
			return invalid("token endpoint path and scheme are only valid with serviceRef")
		}
		host := parsed.Hostname()
		if strings.EqualFold(host, "metadata.google.internal") || strings.EqualFold(host, "kubernetes.default") || strings.EqualFold(host, "kubernetes.default.svc") {
			return invalid("token endpoint URL host is not allowed")
		}
		if ip := net.ParseIP(host); ip != nil && !tokenexchange.IsPublicAddress(ip) {
			return invalid("token endpoint URL must not target private, loopback, or link-local addresses")
		}
	} else {
		if issue := validateServiceReference(*endpoint.ServiceRef); issue != nil {
			return issue
		}
		scheme := endpoint.Scheme
		if scheme == "" {
			scheme = schemeHTTPS
		}
		if scheme != schemeHTTP && scheme != schemeHTTPS {
			return invalid("token endpoint Service scheme must be http or https")
		}
		path := endpoint.Path
		if path == "" {
			path = "/token"
		}
		parsed, err := url.ParseRequestURI(path)
		if err != nil || !strings.HasPrefix(path, "/") || parsed.IsAbs() || parsed.Host != "" {
			return invalid("token endpoint Service path must be an absolute request path")
		}
	}
	if issue := validateTLSConfig(endpoint.TLS, endpointScheme(endpoint)); issue != nil {
		return issue
	}
	return nil
}

func validateGatewaySpec(gateway *corev1alpha1.GatewayOutboundAccess) *Issue {
	if gateway == nil {
		return invalid("gateway adapter is required")
	}
	if issue := validateServiceReference(gateway.ServiceRef); issue != nil {
		return issue
	}
	scheme := gateway.Scheme
	if scheme == "" {
		scheme = schemeHTTP
	}
	if scheme != schemeHTTP && scheme != schemeHTTPS {
		return invalid("gateway scheme must be http or https")
	}
	return validateTLSConfig(gateway.TLS, scheme)
}

func validateTokenSource(source corev1alpha1.OutboundTokenSource, jwtBearer bool) *Issue {
	if jwtBearer && strings.TrimSpace(source.TokenType) != "" {
		return invalid("JWTBearer subject must not set tokenType")
	}
	switch source.Source {
	case corev1alpha1.OutboundTokenSourceTransactionToken:
		if source.SecretRef != nil || source.ServiceAccountRef != nil {
			return invalid("TransactionToken source must not configure Secret or ServiceAccount references")
		}
	case corev1alpha1.OutboundTokenSourceServiceAccount:
		if source.ServiceAccountRef == nil || source.SecretRef != nil {
			return invalid("ServiceAccount source requires only serviceAccountRef")
		}
		if strings.TrimSpace(source.ServiceAccountRef.Name) == "" {
			return invalid("serviceAccountRef.name is required")
		}
	case corev1alpha1.OutboundTokenSourceSecretRef:
		if source.SecretRef == nil || source.ServiceAccountRef != nil {
			return invalid("SecretRef source requires only secretRef")
		}
		if issue := validateSecretSelector(*source.SecretRef); issue != nil {
			return issue
		}
		if !jwtBearer && strings.TrimSpace(source.TokenType) == "" {
			return invalid("SecretRef token exchange sources require tokenType")
		}
	default:
		return invalid("token source must be TransactionToken, ServiceAccount, or SecretRef")
	}
	return nil
}

func validateClientAuthentication(auth *corev1alpha1.OutboundClientAuthentication) *Issue {
	if auth == nil {
		return nil
	}
	method := auth.Method
	if method == "" {
		method = corev1alpha1.OutboundClientAuthNone
	}
	switch method {
	case corev1alpha1.OutboundClientAuthNone:
		if auth.ClientSecretRef != nil || auth.PrivateKeyRef != nil || auth.KeyID != "" || auth.Audience != "" {
			return invalid("None client authentication must not configure credentials")
		}
	case corev1alpha1.OutboundClientAuthSecretBasic, corev1alpha1.OutboundClientAuthSecretPost:
		if strings.TrimSpace(auth.ClientID) == "" || auth.ClientSecretRef == nil || auth.PrivateKeyRef != nil {
			return invalid("client-secret authentication requires clientID and clientSecretRef only")
		}
		if issue := validateSecretSelector(*auth.ClientSecretRef); issue != nil {
			return issue
		}
	case corev1alpha1.OutboundClientAuthPrivateKeyJWT:
		if strings.TrimSpace(auth.ClientID) == "" || auth.PrivateKeyRef == nil || auth.ClientSecretRef != nil {
			return invalid("PrivateKeyJWT requires clientID and privateKeyRef only")
		}
		if issue := validateSecretSelector(*auth.PrivateKeyRef); issue != nil {
			return issue
		}
	default:
		return invalid("unsupported client authentication method")
	}
	return nil
}

func validateTLSConfig(config *corev1alpha1.OutboundTLSConfig, scheme string) *Issue {
	if config == nil {
		return nil
	}
	if scheme != schemeHTTPS {
		return invalid("TLS settings require https")
	}
	if config.CASecretRef != nil {
		return validateSecretSelector(*config.CASecretRef)
	}
	return nil
}

func validateServiceReference(ref corev1alpha1.OutboundServiceReference) *Issue {
	if strings.TrimSpace(ref.Name) == "" || ref.Port < 1 || ref.Port > 65535 {
		return invalid("Service reference requires a name and port from 1 to 65535")
	}
	if errs := validation.IsDNS1035Label(strings.TrimSpace(ref.Name)); len(errs) > 0 {
		return invalid("Service reference name is invalid")
	}
	if namespace := strings.TrimSpace(ref.Namespace); namespace != "" {
		if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
			return invalid("Service reference namespace is invalid")
		}
	}
	return nil
}

func validateSecretSelector(ref corev1alpha1.NamespacedSecretKeySelector) *Issue {
	if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Key) == "" {
		return invalid("Secret references require name and key")
	}
	if namespace := strings.TrimSpace(ref.Namespace); namespace != "" {
		if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
			return invalid("Secret reference namespace is invalid")
		}
	}
	return nil
}

// ResolveReferences verifies same-namespace Secrets/ServiceAccounts, Services,
// port existence, TLS CA keys, and exact cross-namespace trust.
func ResolveReferences(ctx context.Context, reader client.Reader, policy *corev1alpha1.OutboundAccessPolicy, trust TrustConfig) (*Issue, error) {
	if reader == nil {
		return nil, errors.New("outbound access reference reader is required")
	}
	if issue := ValidateSpec(policy); issue != nil {
		return issue, nil
	}
	if policy.Spec.Direct != nil {
		direct := policy.Spec.Direct
		if issue, err := resolveTokenEndpoint(ctx, reader, policy.Namespace, direct.TokenEndpoint, trust.TokenEndpoints); issue != nil || err != nil {
			return issue, err
		}
		if issue, err := resolveTokenSource(ctx, reader, policy.Namespace, direct.Subject); issue != nil || err != nil {
			return issue, err
		}
		if direct.Actor != nil {
			if issue, err := resolveTokenSource(ctx, reader, policy.Namespace, *direct.Actor); issue != nil || err != nil {
				return issue, err
			}
		}
		if auth := direct.ClientAuthentication; auth != nil {
			for _, ref := range []*corev1alpha1.NamespacedSecretKeySelector{auth.ClientSecretRef, auth.PrivateKeyRef} {
				if ref != nil {
					if issue, err := resolveSecret(ctx, reader, policy.Namespace, *ref); issue != nil || err != nil {
						return issue, err
					}
				}
			}
		}
		if issue, err := resolveTLS(ctx, reader, policy.Namespace, direct.TokenEndpoint.TLS); issue != nil || err != nil {
			return issue, err
		}
		return nil, nil
	}
	gateway := policy.Spec.Gateway
	if issue, err := resolveService(ctx, reader, policy.Namespace, gateway.ServiceRef, trust.Gateways); issue != nil || err != nil {
		return issue, err
	}
	return resolveTLS(ctx, reader, policy.Namespace, gateway.TLS)
}

func resolveTokenEndpoint(ctx context.Context, reader client.Reader, namespace string, endpoint corev1alpha1.OutboundTokenEndpoint, trusted TrustedServiceReferences) (*Issue, error) {
	if endpoint.ServiceRef == nil {
		return nil, nil
	}
	return resolveService(ctx, reader, namespace, *endpoint.ServiceRef, trusted)
}

func resolveTokenSource(ctx context.Context, reader client.Reader, namespace string, source corev1alpha1.OutboundTokenSource) (*Issue, error) {
	switch source.Source {
	case corev1alpha1.OutboundTokenSourceSecretRef:
		return resolveSecret(ctx, reader, namespace, *source.SecretRef)
	case corev1alpha1.OutboundTokenSourceServiceAccount:
		serviceAccount := &corev1.ServiceAccount{}
		key := client.ObjectKey{Namespace: namespace, Name: source.ServiceAccountRef.Name}
		if err := reader.Get(ctx, key, serviceAccount); err != nil {
			if apierrors.IsNotFound(err) {
				return unresolved(ReasonReferenceNotFound, "referenced ServiceAccount was not found"), nil
			}
			return nil, fmt.Errorf("resolve outbound ServiceAccount reference: %w", err)
		}
	}
	return nil, nil
}

func resolveService(ctx context.Context, reader client.Reader, policyNamespace string, ref corev1alpha1.OutboundServiceReference, trusted TrustedServiceReferences) (*Issue, error) {
	namespace := strings.TrimSpace(ref.Namespace)
	if namespace == "" {
		namespace = policyNamespace
	}
	if namespace != policyNamespace && !trusted.Allows(ref, policyNamespace) {
		return unresolved(ReasonReferenceDenied, "cross-namespace Service reference is not trusted"), nil
	}
	service := &corev1.Service{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, service); err != nil {
		if apierrors.IsNotFound(err) {
			return unresolved(ReasonReferenceNotFound, "referenced Service was not found"), nil
		}
		return nil, fmt.Errorf("resolve outbound Service reference: %w", err)
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName {
		return unresolved(ReasonReferenceDenied, "ExternalName Services are not allowed for outbound access"), nil
	}
	for _, port := range service.Spec.Ports {
		if port.Port == ref.Port {
			return nil, nil
		}
	}
	return unresolved(ReasonReferenceInvalid, "referenced Service does not expose the configured port"), nil
}

func resolveTLS(ctx context.Context, reader client.Reader, namespace string, config *corev1alpha1.OutboundTLSConfig) (*Issue, error) {
	if config == nil || config.CASecretRef == nil {
		return nil, nil
	}
	ref := *config.CASecretRef
	refNamespace := strings.TrimSpace(ref.Namespace)
	if refNamespace == "" {
		refNamespace = namespace
	}
	if refNamespace != namespace {
		return unresolved(ReasonReferenceDenied, "cross-namespace Secret references are not allowed"), nil
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: refNamespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return unresolved(ReasonReferenceNotFound, "referenced TLS CA Secret was not found"), nil
		}
		return nil, fmt.Errorf("resolve outbound TLS CA reference: %w", err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return unresolved(ReasonReferenceInvalid, "referenced TLS CA Secret key was not found or is empty"), nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(value) {
		return unresolved(ReasonReferenceInvalid, "referenced TLS CA Secret key is not valid PEM certificate data"), nil
	}
	return nil, nil
}

func resolveSecret(ctx context.Context, reader client.Reader, policyNamespace string, ref corev1alpha1.NamespacedSecretKeySelector) (*Issue, error) {
	namespace := strings.TrimSpace(ref.Namespace)
	if namespace == "" {
		namespace = policyNamespace
	}
	if namespace != policyNamespace {
		return unresolved(ReasonReferenceDenied, "cross-namespace Secret references are not allowed"), nil
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return unresolved(ReasonReferenceNotFound, "referenced Secret was not found"), nil
		}
		return nil, fmt.Errorf("resolve outbound Secret reference: %w", err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return unresolved(ReasonReferenceInvalid, "referenced Secret key was not found or is empty"), nil
	}
	return nil, nil
}

func endpointScheme(endpoint corev1alpha1.OutboundTokenEndpoint) string {
	if endpoint.URL != "" {
		return schemeHTTPS
	}
	if endpoint.Scheme != "" {
		return endpoint.Scheme
	}
	return schemeHTTPS
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func invalid(message string) *Issue {
	return &Issue{Reason: ReasonInvalidPolicy, Message: message}
}

func unresolved(reason, message string) *Issue {
	return &Issue{Reason: reason, Message: message}
}
