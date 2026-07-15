/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
	"github.com/orka-agents/orka/internal/transactiontoken"
)

const (
	AdapterDirect  = "direct"
	AdapterGateway = "gateway"
	schemeHTTP     = "http"
	schemeHTTPS    = "https"
)

// ResolveRequest contains trusted execution-time context that is not stored in
// the policy object.
type ResolveRequest struct {
	Namespace                   string
	PolicyName                  string
	TransactionToken            string
	TransactionTokenSource      func() (string, error)
	ParentTransactionScopes     []string
	HasAuthSecretRef            bool
	TargetScheme                string
	CredentialAuthorityEnforced bool
	CredentialScopeAllowed      bool
	CredentialSecret            string
}

// Resolution describes how ToolExecutor should modify a prepared request.
type Resolution struct {
	Adapter string

	CredentialHeader string
	CredentialValue  string

	GatewayScheme string
	GatewayHost   string
	GatewayTLS    tokenexchange.TLSConfig

	SensitiveValues []string
}

// Resolver resolves one same-namespace policy at execution time.
type Resolver interface {
	Resolve(context.Context, ResolveRequest) (Resolution, error)
}

// KubernetesResolver revalidates policy references and resolves credentials
// through Kubernetes immediately before Tool execution.
type KubernetesResolver struct {
	Reader     client.Reader
	KubeClient kubernetes.Interface
	Trust      TrustConfig
	Exchanger  tokenexchange.Exchanger

	exchangeOnce     sync.Once
	defaultExchanger tokenexchange.Exchanger
}

// Resolve returns a direct credential or trusted gateway dial target.
func (r *KubernetesResolver) Resolve(ctx context.Context, req ResolveRequest) (Resolution, error) {
	if r == nil || r.Reader == nil {
		return Resolution{}, errors.New("outbound access resolver is not configured")
	}
	namespace := strings.TrimSpace(req.Namespace)
	policyName := strings.TrimSpace(req.PolicyName)
	if namespace == "" || policyName == "" {
		return Resolution{}, errors.New("outbound access policy namespace and name are required")
	}
	policy := &corev1alpha1.OutboundAccessPolicy{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: policyName}, policy); err != nil {
		return Resolution{}, fmt.Errorf("resolve outbound access policy: %w", err)
	}
	if issue := ValidateSpec(policy); issue != nil {
		return Resolution{}, issue
	}
	if !policyConditionCurrentTrue(policy, corev1alpha1.OutboundAccessPolicyConditionAccepted) ||
		!policyConditionCurrentTrue(policy, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs) {
		return Resolution{}, errors.New("outbound access policy is not accepted with resolved references")
	}
	if issue, err := ResolveReferences(ctx, r.Reader, policy, r.Trust); err != nil {
		return Resolution{}, err
	} else if issue != nil {
		return Resolution{}, issue
	}
	if err := validatePolicyCredentialAuthority(req, policy); err != nil {
		return Resolution{}, err
	}
	needsTransactionToken := policy.Spec.Gateway != nil
	if direct := policy.Spec.Direct; direct != nil {
		needsTransactionToken = direct.Subject.Source == corev1alpha1.OutboundTokenSourceTransactionToken ||
			(direct.Actor != nil && direct.Actor.Source == corev1alpha1.OutboundTokenSourceTransactionToken)
	}
	if needsTransactionToken && strings.TrimSpace(req.TransactionToken) == "" && req.TransactionTokenSource != nil {
		token, err := req.TransactionTokenSource()
		if err != nil {
			return Resolution{}, err
		}
		req.TransactionToken = token
	}
	if policy.Spec.Direct != nil {
		if !strings.EqualFold(strings.TrimSpace(req.TargetScheme), schemeHTTPS) {
			return Resolution{}, errors.New("direct outbound access requires an HTTPS Tool URL")
		}
		if req.HasAuthSecretRef {
			return Resolution{}, errors.New("direct outbound access cannot coexist with authSecretRef")
		}
		return r.resolveDirect(ctx, policy, req)
	}
	return r.resolveGateway(ctx, policy)
}

func (r *KubernetesResolver) exchanger() tokenexchange.Exchanger {
	if r.Exchanger != nil {
		return r.Exchanger
	}
	r.exchangeOnce.Do(func() {
		r.defaultExchanger = tokenexchange.NewClient(tokenexchange.ClientOptions{})
	})
	return r.defaultExchanger
}

func (r *KubernetesResolver) resolveDirect(ctx context.Context, policy *corev1alpha1.OutboundAccessPolicy, req ResolveRequest) (Resolution, error) {
	direct := policy.Spec.Direct
	usesTransactionToken := direct.Subject.Source == corev1alpha1.OutboundTokenSourceTransactionToken ||
		(direct.Actor != nil && direct.Actor.Source == corev1alpha1.OutboundTokenSourceTransactionToken)
	if usesTransactionToken {
		if err := validateRequestedScopeSubset(direct.Scopes, req.ParentTransactionScopes); err != nil {
			return Resolution{}, err
		}
	}
	subject, err := r.resolveTokenSource(ctx, policy.Namespace, direct.Subject, req.TransactionToken)
	if err != nil {
		return Resolution{}, err
	}
	actor := resolvedToken{}
	if direct.Actor != nil {
		actor, err = r.resolveTokenSource(ctx, policy.Namespace, *direct.Actor, req.TransactionToken)
		if err != nil {
			return Resolution{}, err
		}
	}
	endpoint, endpointTLS, endpointIdentity, err := r.resolveTokenEndpoint(ctx, policy.Namespace, direct.TokenEndpoint)
	if err != nil {
		return Resolution{}, err
	}
	clientAuth, clientSecrets, err := r.resolveClientAuthentication(ctx, policy.Namespace, direct.ClientAuthentication)
	if err != nil {
		return Resolution{}, err
	}
	grantType := tokenexchange.GrantTypeTokenExchange
	if direct.Grant == corev1alpha1.OutboundGrantJWTBearer {
		grantType = tokenexchange.GrantTypeJWTBearer
	}
	result, err := r.exchanger().Exchange(ctx, tokenexchange.Request{
		Adapter:                 AdapterDirect,
		ActorExpiresAt:          actor.expiresAt,
		Endpoint:                endpoint,
		TLS:                     endpointTLS,
		RequirePublicEndpoint:   direct.TokenEndpoint.URL != "",
		DisableProxy:            direct.TokenEndpoint.ServiceRef != nil,
		GrantType:               grantType,
		SubjectToken:            subject.value,
		SubjectTokenType:        subject.tokenTypeForGrant(grantType),
		SubjectExpiresAt:        subject.expiresAt,
		ActorToken:              actor.value,
		ActorTokenType:          actor.tokenTypeForGrant(grantType),
		Audiences:               append([]string(nil), direct.Audiences...),
		Scopes:                  append([]string(nil), direct.Scopes...),
		Resources:               append([]string(nil), direct.Resources...),
		RequestedTokenType:      direct.RequestedTokenType,
		AdditionalParameters:    cloneStringMap(direct.AdditionalParameters),
		ClientAuthentication:    clientAuth,
		ExpectedIssuedTokenType: direct.ExpectedIssuedTokenType,
		RequiredTokenType:       "Bearer",
		CacheNamespace:          policyCacheNamespace(policy) + endpointIdentity,
	})
	if err != nil {
		return Resolution{}, fmt.Errorf("outbound resource token exchange failed: %w", err)
	}
	header := "Authorization"
	prefix := "Bearer "
	if direct.Output != nil {
		if strings.TrimSpace(direct.Output.Header) != "" {
			header = http.CanonicalHeaderKey(strings.TrimSpace(direct.Output.Header))
		}
		if direct.Output.Prefix != nil {
			prefix = *direct.Output.Prefix
		}
	}
	sensitive := []string{subject.value, actor.value, result.AccessToken}
	sensitive = append(sensitive, clientSecrets...)
	return Resolution{
		Adapter:          AdapterDirect,
		CredentialHeader: header,
		CredentialValue:  prefix + result.AccessToken,
		SensitiveValues:  compactSensitiveValues(sensitive),
	}, nil
}

func (r *KubernetesResolver) resolveGateway(ctx context.Context, policy *corev1alpha1.OutboundAccessPolicy) (Resolution, error) {
	gateway := policy.Spec.Gateway
	namespace := strings.TrimSpace(gateway.ServiceRef.Namespace)
	if namespace == "" {
		namespace = policy.Namespace
	}
	scheme := gateway.Scheme
	if scheme == "" {
		scheme = schemeHTTP
	}
	tlsConfig, err := r.resolveTLS(ctx, policy.Namespace, gateway.TLS)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{
		Adapter:       AdapterGateway,
		GatewayScheme: scheme,
		GatewayHost:   net.JoinHostPort(gateway.ServiceRef.Name+"."+namespace+".svc", fmt.Sprintf("%d", gateway.ServiceRef.Port)),
		GatewayTLS:    tlsConfig,
	}, nil
}

type resolvedToken struct {
	value     string
	tokenType string
	expiresAt time.Time
}

func (t resolvedToken) tokenTypeForGrant(grant string) string {
	if grant == tokenexchange.GrantTypeJWTBearer {
		return ""
	}
	return t.tokenType
}

func (r *KubernetesResolver) resolveTokenSource(ctx context.Context, namespace string, source corev1alpha1.OutboundTokenSource, transactionToken string) (resolvedToken, error) {
	switch source.Source {
	case corev1alpha1.OutboundTokenSourceTransactionToken:
		if strings.TrimSpace(transactionToken) == "" {
			return resolvedToken{}, errors.New("TransactionToken source requires a current transaction token")
		}
		tokenType := strings.TrimSpace(source.TokenType)
		if tokenType == "" {
			tokenType = transactiontoken.SubjectTokenTypeTransactionToken
		}
		return resolvedToken{value: transactionToken, tokenType: tokenType, expiresAt: unverifiedJWTExpiry(transactionToken)}, nil
	case corev1alpha1.OutboundTokenSourceSecretRef:
		value, err := r.readSecret(ctx, namespace, *source.SecretRef)
		if err != nil {
			return resolvedToken{}, err
		}
		return resolvedToken{value: value, tokenType: strings.TrimSpace(source.TokenType), expiresAt: unverifiedJWTExpiry(value)}, nil
	case corev1alpha1.OutboundTokenSourceServiceAccount:
		if r.KubeClient == nil {
			return resolvedToken{}, errors.New("ServiceAccount token source requires a Kubernetes client")
		}
		expiration := int64(600)
		if source.ServiceAccountRef.ExpirationSeconds != nil {
			expiration = *source.ServiceAccountRef.ExpirationSeconds
		}
		request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
			Audiences:         append([]string(nil), source.ServiceAccountRef.Audiences...),
			ExpirationSeconds: &expiration,
		}}
		response, err := r.KubeClient.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, source.ServiceAccountRef.Name, request, metav1.CreateOptions{})
		if err != nil {
			return resolvedToken{}, fmt.Errorf("request ServiceAccount subject token: %w", err)
		}
		if strings.TrimSpace(response.Status.Token) == "" {
			return resolvedToken{}, errors.New("ServiceAccount TokenRequest returned an empty token")
		}
		tokenType := strings.TrimSpace(source.TokenType)
		if tokenType == "" {
			tokenType = tokenexchange.TokenTypeAccessToken
		}
		return resolvedToken{value: response.Status.Token, tokenType: tokenType, expiresAt: response.Status.ExpirationTimestamp.Time}, nil
	default:
		return resolvedToken{}, errors.New("unsupported outbound token source")
	}
}

func (r *KubernetesResolver) resolveTokenEndpoint(ctx context.Context, namespace string, endpoint corev1alpha1.OutboundTokenEndpoint) (string, tokenexchange.TLSConfig, string, error) {
	tlsConfig, err := r.resolveTLS(ctx, namespace, endpoint.TLS)
	if err != nil {
		return "", tokenexchange.TLSConfig{}, "", err
	}
	if endpoint.URL != "" {
		return strings.TrimSpace(endpoint.URL), tlsConfig, "", nil
	}
	serviceNamespace := strings.TrimSpace(endpoint.ServiceRef.Namespace)
	if serviceNamespace == "" {
		serviceNamespace = namespace
	}
	scheme := endpoint.Scheme
	if scheme == "" {
		scheme = schemeHTTPS
	}
	path := endpoint.Path
	if path == "" {
		path = "/token"
	}
	parsedPath, err := url.ParseRequestURI(path)
	if err != nil {
		return "", tokenexchange.TLSConfig{}, "", errors.New("token endpoint Service path is invalid")
	}
	service := &corev1.Service{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: serviceNamespace, Name: endpoint.ServiceRef.Name}, service); err != nil {
		return "", tokenexchange.TLSConfig{}, "", fmt.Errorf("resolve token endpoint Service identity: %w", err)
	}
	identityData, _ := json.Marshal([]any{serviceNamespace, service.Name, string(service.UID), service.ResourceVersion, endpoint.ServiceRef.Port})
	identitySum := sha256.Sum256(identityData)
	endpointIdentity := hex.EncodeToString(identitySum[:])
	endpointURL := url.URL{
		Scheme:   scheme,
		Host:     net.JoinHostPort(endpoint.ServiceRef.Name+"."+serviceNamespace+".svc", fmt.Sprintf("%d", endpoint.ServiceRef.Port)),
		Path:     parsedPath.Path,
		RawPath:  parsedPath.RawPath,
		RawQuery: parsedPath.RawQuery,
	}
	return endpointURL.String(), tlsConfig, endpointIdentity, nil
}

func (r *KubernetesResolver) resolveClientAuthentication(ctx context.Context, namespace string, auth *corev1alpha1.OutboundClientAuthentication) (tokenexchange.ClientAuthentication, []string, error) {
	if auth == nil {
		return tokenexchange.ClientAuthentication{Method: tokenexchange.ClientAuthNone}, nil, nil
	}
	resolved := tokenexchange.ClientAuthentication{
		ClientID: auth.ClientID,
		KeyID:    auth.KeyID,
		Audience: auth.Audience,
	}
	var secrets []string
	method := auth.Method
	if method == "" {
		method = corev1alpha1.OutboundClientAuthNone
	}
	switch method {
	case corev1alpha1.OutboundClientAuthNone:
		resolved.Method = tokenexchange.ClientAuthNone
	case corev1alpha1.OutboundClientAuthSecretBasic, corev1alpha1.OutboundClientAuthSecretPost:
		value, err := r.readSecret(ctx, namespace, *auth.ClientSecretRef)
		if err != nil {
			return tokenexchange.ClientAuthentication{}, nil, err
		}
		resolved.ClientSecret = value
		secrets = append(secrets, value)
		if method == corev1alpha1.OutboundClientAuthSecretBasic {
			resolved.Method = tokenexchange.ClientAuthSecretBasic
		} else {
			resolved.Method = tokenexchange.ClientAuthSecretPost
		}
	case corev1alpha1.OutboundClientAuthPrivateKeyJWT:
		value, err := r.readSecret(ctx, namespace, *auth.PrivateKeyRef)
		if err != nil {
			return tokenexchange.ClientAuthentication{}, nil, err
		}
		resolved.Method = tokenexchange.ClientAuthPrivateKeyJWT
		resolved.PrivateKeyPEM = []byte(value)
	default:
		return tokenexchange.ClientAuthentication{}, nil, errors.New("unsupported outbound client authentication method")
	}
	return resolved, secrets, nil
}

func (r *KubernetesResolver) resolveTLS(ctx context.Context, namespace string, config *corev1alpha1.OutboundTLSConfig) (tokenexchange.TLSConfig, error) {
	if config == nil {
		return tokenexchange.TLSConfig{}, nil
	}
	resolved := tokenexchange.TLSConfig{ServerName: strings.TrimSpace(config.ServerName)}
	if config.CASecretRef != nil {
		value, err := r.readSecret(ctx, namespace, *config.CASecretRef)
		if err != nil {
			return tokenexchange.TLSConfig{}, err
		}
		resolved.CAPEM = []byte(value)
	}
	return resolved, nil
}

func (r *KubernetesResolver) readSecret(ctx context.Context, policyNamespace string, ref corev1alpha1.NamespacedSecretKeySelector) (string, error) {
	namespace := strings.TrimSpace(ref.Namespace)
	if namespace == "" {
		namespace = policyNamespace
	}
	if namespace != policyNamespace {
		return "", errors.New("cross-namespace Secret references are not allowed")
	}
	secret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return "", fmt.Errorf("read outbound access Secret: %w", err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return "", errors.New("outbound access Secret key is missing or empty")
	}
	return strings.TrimSpace(string(value)), nil
}

func validateRequestedScopeSubset(requested, parent []string) error {
	requested = normalizeScopes(requested)
	if len(requested) == 0 {
		return errors.New("TransactionToken direct exchange requires at least one requested scope")
	}
	parent = normalizeScopes(parent)
	if len(parent) == 0 {
		return errors.New("parent transaction scopes are required for TransactionToken direct exchange")
	}
	for _, scope := range requested {
		if !slices.Contains(parent, scope) {
			return fmt.Errorf("outbound transaction scope %q is not present in parent transaction scopes", scope)
		}
	}
	return nil
}

func normalizeScopes(scopes []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(scopes))
	for _, value := range scopes {
		for scope := range strings.FieldsSeq(value) {
			if _, ok := seen[scope]; ok {
				continue
			}
			seen[scope] = struct{}{}
			out = append(out, scope)
		}
	}
	return out
}

func policyConditionCurrentTrue(policy *corev1alpha1.OutboundAccessPolicy, conditionType string) bool {
	if policy == nil || policy.Status.ObservedGeneration != policy.Generation {
		return false
	}
	for _, condition := range policy.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == policy.Generation
		}
	}
	return false
}

func validatePolicyCredentialAuthority(req ResolveRequest, policy *corev1alpha1.OutboundAccessPolicy) error {
	if !req.CredentialAuthorityEnforced || policy == nil {
		return nil
	}
	credentialSecrets := []string{}
	usesServiceAccount := false
	if direct := policy.Spec.Direct; direct != nil {
		credentialSecrets = append(credentialSecrets, policyCredentialSecretNames(direct)...)
		usesServiceAccount = tokenSourceUsesServiceAccount(direct.Subject) ||
			(direct.Actor != nil && tokenSourceUsesServiceAccount(*direct.Actor))
	}
	if gateway := policy.Spec.Gateway; gateway != nil && gateway.TLS != nil && gateway.TLS.CASecretRef != nil {
		credentialSecrets = append(credentialSecrets, strings.TrimSpace(gateway.TLS.CASecretRef.Name))
	}
	if (len(credentialSecrets) > 0 || usesServiceAccount) && !req.CredentialScopeAllowed {
		return errors.New("outbound credentials are not authorized by task transaction authority")
	}
	if constraint := strings.TrimSpace(req.CredentialSecret); constraint != "" {
		for _, name := range credentialSecrets {
			if name != "" && name != constraint {
				return errors.New("outbound credential Secret does not match task transaction authority")
			}
		}
	}
	return nil
}

func tokenSourceUsesServiceAccount(source corev1alpha1.OutboundTokenSource) bool {
	return source.Source == corev1alpha1.OutboundTokenSourceServiceAccount
}

func policyCredentialSecretNames(direct *corev1alpha1.DirectOutboundAccess) []string {
	if direct == nil {
		return nil
	}
	names := []string{}
	appendRef := func(ref *corev1alpha1.NamespacedSecretKeySelector) {
		if ref != nil && strings.TrimSpace(ref.Name) != "" {
			names = append(names, strings.TrimSpace(ref.Name))
		}
	}
	appendRef(direct.Subject.SecretRef)
	if direct.Actor != nil {
		appendRef(direct.Actor.SecretRef)
	}
	if direct.ClientAuthentication != nil {
		appendRef(direct.ClientAuthentication.ClientSecretRef)
		appendRef(direct.ClientAuthentication.PrivateKeyRef)
	}
	if direct.TokenEndpoint.TLS != nil {
		appendRef(direct.TokenEndpoint.TLS.CASecretRef)
	}
	return names
}

func policyCacheNamespace(policy *corev1alpha1.OutboundAccessPolicy) string {
	if policy == nil {
		return ""
	}
	shape := struct {
		Namespace  string
		Name       string
		UID        string
		Generation int64
		Spec       corev1alpha1.OutboundAccessPolicySpec
	}{
		Namespace:  policy.Namespace,
		Name:       policy.Name,
		UID:        string(policy.UID),
		Generation: policy.Generation,
		Spec:       policy.Spec,
	}
	data, _ := json.Marshal(shape)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func unverifiedJWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Expiration json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if decoder.Decode(&claims) != nil || claims.Expiration == "" {
		return time.Time{}
	}
	seconds, err := claims.Expiration.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	maps.Copy(cloned, values)
	return cloned
}

func compactSensitiveValues(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
