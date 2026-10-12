/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/tokenexchange"
	"github.com/orka-agents/orka/internal/transactiontoken"
)

const (
	AdapterDirect  = "direct"
	AdapterGateway = "gateway"
	// AdapterConnection injects a person's linked-account credential; the
	// executor applies it exactly like a direct credential.
	AdapterConnection = "connection"
	schemeHTTP        = "http"
	schemeHTTPS       = "https"

	// DefaultCredentialReadScope authorizes use of cluster-managed credential material.
	DefaultCredentialReadScope = "orka:secrets:credentials:read"
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

	// Requester is the Task's verified human identity. Connection-mode
	// policies resolve the credential of this person and nobody else.
	Requester *corev1alpha1.RequestedBy
	// FrozenConnections maps policy name to the Connection identity frozen
	// into the Task's execution snapshot at dispatch. Connection-mode
	// resolution requires an entry and fails closed when the live Connection
	// differs from it.
	FrozenConnections map[string]FrozenConnection
	// CheckedPolicy, when set, is the policy the caller validated against
	// the dispatched configuration; resolution refuses a policy object that
	// changed after that check rather than injecting under a different one.
	CheckedPolicy *PolicyIdentity
	// Tool identifies the executing Tool. Connection-mode resolution releases
	// a credential only to a Tool the ConnectorProvider declares, with the
	// same URL, method, and class, so a policy cannot be attached to an
	// arbitrary endpoint to exfiltrate a person's token.
	Tool ToolBinding
	// Arguments are the call's arguments. Connection-mode resolution judges
	// them against the provider's curated schema before any credential is
	// resolved, so a rejected call never refreshes or shreds custody.
	Arguments json.RawMessage
	// MCPBacked marks a Tool whose requests go to an MCP actor endpoint
	// rather than its declared URL; connection mode refuses it before any
	// credential work.
	MCPBacked bool
}

// ToolBinding is the executing Tool's identity for connector checks.
type ToolBinding struct {
	Name   string
	URL    string
	Method string
	Class  corev1alpha1.AgentRuntimeBrokeredToolClass
	// Builtin marks an Orka built-in tool the controller executes itself.
	// It matches only a Builtin declaration of the same name and class; a
	// built-in carries no destination, headers, schema, or timeout to compare.
	Builtin bool
	// Headers are the Tool's static headers; the provider's declared set
	// must match exactly, or a header the provider never declared could
	// change what the credential authorizes.
	Headers map[string]string
	// Parameters is the Tool's JSON Schema; it must equal the provider's
	// curated schema so no weaker Tool schema admits arguments the
	// provider excluded.
	Parameters *apiextensionsv1.JSON
	// Timeout is the Tool's request timeout; it must equal the provider's
	// curated bound so a Tool cannot keep a credential-bearing request open
	// longer than the provider allows. An omitted timeout (TimeoutSet false)
	// means the 30s default; an explicit nonpositive one is refused, because
	// the executor would run such a request without any deadline.
	Timeout    time.Duration
	TimeoutSet bool
}

// connectorDefaultTimeout is the request timeout both a Tool CR and a
// ConnectorProvider tool declaration mean when they declare none.
const connectorDefaultTimeout = 30 * time.Second

// NormalizedConnectorTimeout applies the shared default.
func NormalizedConnectorTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return connectorDefaultTimeout
	}
	return timeout
}

// PolicyIdentity pins the OutboundAccessPolicy a caller checked before
// asking for resolution.
type PolicyIdentity struct {
	UID        string
	Generation int64
}

// FrozenConnection is the dispatch-time identity of a person's Connection.
type FrozenConnection struct {
	// Name is the Connection object's name when known. A person's link
	// may live under a non-canonical name (created outside the API and
	// adopted), so the credential source loads by this name, not by the
	// canonical one, when it is set.
	Name       string
	UID        string
	Generation int64
	// GrantSequence is the consent count frozen with the identity. The live
	// Connection must carry exactly this grant: a re-link of the same
	// object is a new grant the snapshot never bound.
	GrantSequence int64
	// PolicyUID and PolicyGeneration, when set, pin the policy object the
	// Connection was frozen under; a policy edited since (for example its
	// credential output header or prefix) needs a re-dispatch.
	PolicyUID        string
	PolicyGeneration int64
	// Provider is the ConnectorProvider the Connection links. A binding
	// frozen for a built-in tool carries it because no policy names one.
	Provider string
}

// builtinConnectionKeyPrefix keys a built-in tool's frozen Connection in
// the same map as policy bindings. Policy names are DNS labels, which never
// contain a colon, so the two key spaces cannot collide.
const builtinConnectionKeyPrefix = "builtin:"

// BuiltinConnectionKey returns the FrozenConnections key under which the
// requester's Connection for the named built-in tool is frozen.
func BuiltinConnectionKey(toolName string) string {
	return builtinConnectionKeyPrefix + strings.TrimSpace(toolName)
}

// ConnectionCredentialRequest asks the credential source for one person's
// current access token for one provider.
type ConnectionCredentialRequest struct {
	Namespace string
	Provider  string
	Issuer    string
	Subject   string
	Frozen    FrozenConnection
	// Tool is the executing Tool; the source verifies it against the same
	// provider state it validates the returned credential with.
	Tool ToolBinding
}

// ConnectionCredential is a resolved, possibly just-refreshed access token
// plus the non-secret identity of the Connection that supplied it.
type ConnectionCredential struct {
	AccessToken   string
	TokenType     string
	ConnectionUID string
	Generation    int64
	Mode          string
}

// ConnectionCredentialSource resolves and refreshes per-person credentials.
// It exists only in the controller; worker Pods have no implementation and
// therefore fail closed on connection-mode policies.
type ConnectionCredentialSource interface {
	ResolveConnectionCredential(context.Context, ConnectionCredentialRequest) (ConnectionCredential, error)
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

	// ConnectionUID identifies the person's Connection for connection-mode
	// resolutions, for audit records. Never the token.
	ConnectionUID string
	// Parameters is the provider-declared JSON Schema for the tool's
	// arguments; the executor validates the call against it before the
	// credential leaves the process.
	Parameters *apiextensionsv1.JSON
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
	// Connections supplies per-person credentials for connection-mode
	// policies. Nil means this process may not execute connector-backed
	// tools.
	Connections ConnectionCredentialSource

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
	if req.CheckedPolicy != nil && (string(policy.UID) != req.CheckedPolicy.UID || policy.Generation != req.CheckedPolicy.Generation) {
		return Resolution{}, errors.New("outbound access policy changed since it was checked; re-dispatch the task")
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
	if policy.Spec.Direct != nil || policy.Spec.Connection != nil {
		if !strings.EqualFold(strings.TrimSpace(req.TargetScheme), schemeHTTPS) {
			return Resolution{}, errors.New("credential-injecting outbound access requires an HTTPS Tool URL")
		}
		if req.HasAuthSecretRef {
			return Resolution{}, errors.New("credential-injecting outbound access cannot coexist with authSecretRef")
		}
	}
	if _, frozen := req.FrozenConnections[policy.Name]; frozen && policy.Spec.Connection == nil {
		// The Task was dispatched under a person's Connection for this
		// policy; a policy moved to another adapter since must not hand it
		// a service credential or route instead.
		return Resolution{}, fmt.Errorf("outbound access policy %q was in connection mode when the task was dispatched and no longer is", policy.Name)
	}
	switch {
	case policy.Spec.Direct != nil:
		return r.resolveDirect(ctx, policy, req)
	case policy.Spec.Connection != nil:
		return r.resolveConnection(ctx, policy, req)
	}
	return r.resolveGateway(ctx, policy)
}

// DeclaredConnectorTool returns the provider's curated HTTP definition that
// exactly matches the executing Tool, or an error. Built-in declarations are
// never matched by custom Tools. The credential source applies the same
// check against the provider it validates the returned credential with, so
// the tool and the credential are always judged against one provider state.
func DeclaredConnectorTool(provider *corev1alpha1.ConnectorProvider, tool ToolBinding) (corev1alpha1.ConnectorTool, error) {
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		return corev1alpha1.ConnectorTool{}, errors.New("connection outbound access requires the executing tool identity")
	}
	for _, candidate := range provider.Spec.Tools {
		if candidate.Name != name {
			continue
		}
		if tool.Builtin {
			return declaredBuiltinConnectorTool(provider, candidate, tool)
		}
		if candidate.Source != corev1alpha1.ConnectorToolSourceHTTP || candidate.HTTP == nil {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("connector tool %q is not a curated HTTP tool of provider %q", name, provider.Name)
		}
		method := strings.ToUpper(strings.TrimSpace(candidate.HTTP.Method))
		if method == "" {
			method = http.MethodPost
		}
		toolMethod := strings.ToUpper(strings.TrimSpace(tool.Method))
		if toolMethod == "" {
			toolMethod = http.MethodPost
		}
		if strings.TrimSpace(tool.URL) != candidate.HTTP.URL || toolMethod != method {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q does not match the endpoint declared by provider %q", name, provider.Name)
		}
		if string(candidate.Class) != string(tool.Class) {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q class does not match the class declared by provider %q", name, provider.Name)
		}
		if !sameStaticHeaders(candidate.HTTP.Headers, tool.Headers) {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q headers do not match the headers declared by provider %q", name, provider.Name)
		}
		if !sameParameterSchema(candidate.Parameters, tool.Parameters) {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q parameters do not match the schema declared by provider %q", name, provider.Name)
		}
		if tool.TimeoutSet && tool.Timeout <= 0 {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q declares a timeout of %s; connector-backed requests need a positive one", name, tool.Timeout)
		}
		declaredTimeout := time.Duration(0)
		if candidate.HTTP.Timeout != nil {
			declaredTimeout = candidate.HTTP.Timeout.Duration
		}
		if NormalizedConnectorTimeout(declaredTimeout) != NormalizedConnectorTimeout(tool.Timeout) {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q timeout does not match the timeout declared by provider %q", name, provider.Name)
		}
		return candidate, nil
	}
	return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q is not declared by provider %q", name, provider.Name)
}

// declaredBuiltinConnectorTool judges a built-in binding against the
// provider's declaration of the same name: it must be a Builtin declaration
// of the class the catalog fixes for the tool, and the binding must carry
// nothing a built-in cannot have.
func declaredBuiltinConnectorTool(provider *corev1alpha1.ConnectorProvider, candidate corev1alpha1.ConnectorTool, tool ToolBinding) (corev1alpha1.ConnectorTool, error) {
	if candidate.Source != corev1alpha1.ConnectorToolSourceBuiltin {
		return corev1alpha1.ConnectorTool{}, fmt.Errorf("built-in tool %q is not declared as a built-in by provider %q", tool.Name, provider.Name)
	}
	if !connectors.ProviderIssuesGitHubCredentials(provider) {
		return corev1alpha1.ConnectorTool{}, fmt.Errorf("built-in tool %q sends its credential to %s, which provider %q does not issue credentials for", tool.Name, connectors.BuiltinConnectorToolAudience, provider.Name)
	}
	if strings.TrimSpace(tool.URL) != "" || strings.TrimSpace(tool.Method) != "" || len(tool.Headers) > 0 || tool.Parameters != nil {
		return corev1alpha1.ConnectorTool{}, fmt.Errorf("built-in tool %q binding carries a destination, headers, or schema", tool.Name)
	}
	class, linked := connectors.BuiltinConnectorToolClass(tool.Name)
	if !linked {
		return corev1alpha1.ConnectorTool{}, fmt.Errorf("built-in tool %q cannot use a linked account", tool.Name)
	}
	if string(tool.Class) != string(class) || candidate.Class != class {
		return corev1alpha1.ConnectorTool{}, fmt.Errorf("built-in tool %q class does not match the class declared by provider %q", tool.Name, provider.Name)
	}
	// The binding carries the catalog's bound for the call, so the
	// credential is refreshed to cover all of it, never a caller's own.
	timeout, _ := connectors.BuiltinConnectorToolTimeout(tool.Name)
	if !tool.TimeoutSet || tool.Timeout != timeout {
		return corev1alpha1.ConnectorTool{}, fmt.Errorf("built-in tool %q binding must carry the catalog timeout %s", tool.Name, timeout)
	}
	return candidate, nil
}

// sameParameterSchema compares two JSON Schemas structurally; absent and
// empty are the same.
func sameParameterSchema(declared, actual *apiextensionsv1.JSON) bool {
	canonical := func(schema *apiextensionsv1.JSON) (string, bool) {
		if schema == nil || len(bytes.TrimSpace(schema.Raw)) == 0 {
			return "", true
		}
		var value any
		if err := json.Unmarshal(schema.Raw, &value); err != nil {
			return "", false
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	}
	a, okA := canonical(declared)
	b, okB := canonical(actual)
	return okA && okB && a == b
}

// sameStaticHeaders compares two header sets by canonical name and exact
// value; an empty and a nil set are the same.
func sameStaticHeaders(declared, actual map[string]string) bool {
	canonicalize := func(headers map[string]string) (map[string]string, bool) {
		result := make(map[string]string, len(headers))
		for name, value := range headers {
			key := http.CanonicalHeaderKey(strings.TrimSpace(name))
			if _, dup := result[key]; dup {
				// Two spellings of one header would collapse into one field
				// on the wire; the set is not the declared one.
				return nil, false
			}
			result[key] = value
		}
		return result, true
	}
	want, ok := canonicalize(declared)
	if !ok {
		return false
	}
	got, ok := canonicalize(actual)
	if !ok || len(want) != len(got) {
		return false
	}
	for key, value := range want {
		actualValue, present := got[key]
		if !present || actualValue != value {
			return false
		}
	}
	return true
}

// resolveConnection injects the requester's linked-account credential. Every
// precondition fails closed: no source in this process, no verified
// requester, no frozen binding, or a source error all mean no credential.
func (r *KubernetesResolver) resolveConnection(ctx context.Context, policy *corev1alpha1.OutboundAccessPolicy, req ResolveRequest) (Resolution, error) {
	if r.Connections == nil {
		return Resolution{}, errors.New("connector-backed tools execute only in the controller")
	}
	requester := req.Requester
	if requester == nil || strings.TrimSpace(requester.Issuer) == "" || strings.TrimSpace(requester.Subject) == "" {
		return Resolution{}, errors.New("connection outbound access requires a Task with a verified requester")
	}
	// The person's token is bound to the exact curated destination. A Tool
	// that sends to an MCP actor endpoint, or whose URL is a template the
	// call's arguments would rewrite, can never satisfy that, so it is
	// refused before any credential work.
	if req.MCPBacked {
		return Resolution{}, errors.New("connection outbound access policies are not supported on MCP-backed tools")
	}
	if strings.Contains(req.Tool.URL, "{{") {
		return Resolution{}, errors.New("connection outbound access requires a Tool URL without template placeholders")
	}
	frozen, ok := req.FrozenConnections[policy.Name]
	if !ok || strings.TrimSpace(frozen.UID) == "" {
		return Resolution{}, fmt.Errorf("connection outbound access policy %q has no Connection frozen into the execution snapshot", policy.Name)
	}
	if frozen.PolicyUID != "" && (string(policy.UID) != frozen.PolicyUID || policy.Generation != frozen.PolicyGeneration) {
		return Resolution{}, fmt.Errorf("connection outbound access policy %q changed since the task was dispatched; re-dispatch to use it", policy.Name)
	}
	if frozen.Provider != "" && frozen.Provider != policy.Spec.Connection.ProviderRef.Name {
		return Resolution{}, fmt.Errorf("connection outbound access policy %q selects a different provider than the one frozen for it", policy.Name)
	}
	if req.Tool.Builtin {
		return Resolution{}, errors.New("a built-in tool has no outbound access policy")
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: policy.Spec.Connection.ProviderRef.Name}, provider); err != nil {
		return Resolution{}, fmt.Errorf("resolve connector provider: %w", err)
	}
	declared, err := DeclaredConnectorTool(provider, req.Tool)
	if err != nil {
		return Resolution{}, err
	}
	if err := connectors.ValidateToolArguments(declared.Parameters, req.Arguments); err != nil {
		return Resolution{}, fmt.Errorf("connection credential request arguments rejected: %w", err)
	}
	credential, err := r.Connections.ResolveConnectionCredential(ctx, ConnectionCredentialRequest{
		Namespace: policy.Namespace,
		Provider:  policy.Spec.Connection.ProviderRef.Name,
		Issuer:    requester.Issuer,
		Subject:   requester.Subject,
		Frozen:    frozen,
		Tool:      req.Tool,
	})
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve connection credential: %w", err)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return Resolution{}, errors.New("connection credential source returned an empty credential")
	}
	if declared.Class == corev1alpha1.ConnectorToolClassWrite && credential.Mode != corev1alpha1.ConnectionModeReadWrite {
		return Resolution{}, errors.New("connection is readOnly; write tools are not available")
	}
	header := defaultCredentialHeader
	prefix := "Bearer "
	if output := policy.Spec.Connection.Output; output != nil {
		if strings.TrimSpace(output.Header) != "" {
			header = http.CanonicalHeaderKey(strings.TrimSpace(output.Header))
		}
		if output.Prefix != nil {
			prefix = *output.Prefix
		}
	}
	return Resolution{
		Adapter:          AdapterConnection,
		CredentialHeader: header,
		CredentialValue:  prefix + credential.AccessToken,
		SensitiveValues:  compactSensitiveValues([]string{credential.AccessToken}),
		ConnectionUID:    credential.ConnectionUID,
		Parameters:       declared.Parameters,
	}, nil
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
	sensitive := make([]string, 0, 3+len(clientSecrets))
	sensitive = append(sensitive, subject.value, actor.value, result.AccessToken)
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
		return resolvedToken{value: transactionToken, tokenType: tokenType, expiresAt: tokenexchange.UnverifiedJWTExpiry(transactionToken)}, nil
	case corev1alpha1.OutboundTokenSourceSecretRef:
		value, err := r.readSecret(ctx, namespace, *source.SecretRef)
		if err != nil {
			return resolvedToken{}, err
		}
		return resolvedToken{value: value, tokenType: strings.TrimSpace(source.TokenType), expiresAt: tokenexchange.UnverifiedJWTExpiry(value)}, nil
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
		value, err := r.readSecretBytes(ctx, namespace, *auth.ClientSecretRef)
		if err != nil {
			return tokenexchange.ClientAuthentication{}, nil, err
		}
		resolved.ClientSecret = string(value)
		secrets = append(secrets, string(value))
		if method == corev1alpha1.OutboundClientAuthSecretBasic {
			resolved.Method = tokenexchange.ClientAuthSecretBasic
		} else {
			resolved.Method = tokenexchange.ClientAuthSecretPost
		}
	case corev1alpha1.OutboundClientAuthPrivateKeyJWT:
		value, err := r.readSecretBytes(ctx, namespace, *auth.PrivateKeyRef)
		if err != nil {
			return tokenexchange.ClientAuthentication{}, nil, err
		}
		resolved.Method = tokenexchange.ClientAuthPrivateKeyJWT
		resolved.PrivateKeyPEM = value
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
		value, err := r.readSecretBytes(ctx, namespace, *config.CASecretRef)
		if err != nil {
			return tokenexchange.TLSConfig{}, err
		}
		resolved.CAPEM = value
	}
	return resolved, nil
}

func (r *KubernetesResolver) readSecret(ctx context.Context, policyNamespace string, ref corev1alpha1.NamespacedSecretKeySelector) (string, error) {
	value, err := r.readSecretBytes(ctx, policyNamespace, ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(value)), nil
}

func (r *KubernetesResolver) readSecretBytes(ctx context.Context, policyNamespace string, ref corev1alpha1.NamespacedSecretKeySelector) ([]byte, error) {
	namespace := strings.TrimSpace(ref.Namespace)
	if namespace == "" {
		namespace = policyNamespace
	}
	if namespace != policyNamespace {
		return nil, errors.New("cross-namespace Secret references are not allowed")
	}
	secret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return nil, fmt.Errorf("read outbound access Secret: %w", err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return nil, errors.New("outbound access Secret key is missing or empty")
	}
	return append([]byte(nil), value...), nil
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
	if policy == nil || !policy.DeletionTimestamp.IsZero() || policy.Status.ObservedGeneration != policy.Generation {
		return false
	}
	for _, condition := range policy.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == policy.Generation
		}
	}
	return false
}

// ValidateCredentialAuthority enforces immutable transaction authorization for
// Secret- and ServiceAccount-backed outbound credentials.
func ValidateCredentialAuthority(enforced, scopeAllowed bool, constraint string, secretNames []string, usesServiceAccount bool) error {
	if !enforced {
		return nil
	}
	names := make([]string, 0, len(secretNames))
	for _, name := range secretNames {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	if (len(names) > 0 || usesServiceAccount) && !scopeAllowed {
		return errors.New("outbound credentials are not authorized by task transaction authority")
	}
	if constraint = strings.TrimSpace(constraint); constraint != "" {
		for _, name := range names {
			if name != constraint {
				return errors.New("outbound credential Secret does not match task transaction authority")
			}
		}
	}
	return nil
}

func validatePolicyCredentialAuthority(req ResolveRequest, policy *corev1alpha1.OutboundAccessPolicy) error {
	if policy == nil {
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
	return ValidateCredentialAuthority(
		req.CredentialAuthorityEnforced,
		req.CredentialScopeAllowed,
		req.CredentialSecret,
		credentialSecrets,
		usesServiceAccount,
	)
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
		if value == "" {
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
