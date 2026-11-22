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

	"github.com/go-logr/logr"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	AdapterDirect        = "direct"
	AdapterGateway       = "gateway"
	AdapterConnection    = "connection"
	AdapterAgentIdentity = "agentIdentity"
	schemeHTTP           = "http"
	schemeHTTPS          = "https"

	defaultCredentialPrefix = "Bearer "

	DefaultCredentialReadScope = "orka:secrets:credentials:read"
)

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

	Requester         *corev1alpha1.RequestedBy
	FrozenConnections map[string]FrozenConnection
	CheckedPolicy     *PolicyIdentity
	Tool              ToolBinding
	Arguments         json.RawMessage
	MCPBacked         bool
	TaskUID           string
	AgentIdentity     *AgentIdentityBinding
}

type AgentIdentityBinding struct {
	Identity        corev1alpha1.AgentExecutionIdentityRef
	AgentNamespace  string
	AgentName       string
	AgentUID        string
	AgentGeneration int64
	TaskNamespace   string
	TaskName        string
	TaskUID         string
	TaskCreated     time.Time
	DelegationStart time.Time
	ManagedEgress   bool
}

type AgentIdentityTokenRequest struct {
	Identity      corev1alpha1.AgentExecutionIdentityRef
	Mode          string
	Resource      string
	Policy        PolicyIdentity
	UserAssertion string
}

const (
	AgentConditionIdentityVerified        = "IdentityVerified"
	AgentReasonIdentityVerificationFailed = "VerificationFailed"
)

func issuerRefusedIdentity(agent *corev1alpha1.Agent) error {
	condition := meta.FindStatusCondition(agent.Status.Conditions, AgentConditionIdentityVerified)
	if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == AgentReasonIdentityVerificationFailed {
		return errors.New("the issuer refused the Task's bound Agent identity when it was last verified")
	}
	return nil
}

type AgentIdentityTokenSource interface {
	AgentIdentityToken(context.Context, AgentIdentityTokenRequest) (string, error)
}

type ToolBinding struct {
	Name       string
	URL        string
	Method     string
	Class      corev1alpha1.AgentRuntimeBrokeredToolClass
	Forward    corev1alpha1.HTTPForwardProtocol
	Builtin    bool
	Headers    map[string]string
	Parameters *apiextensionsv1.JSON
	Timeout    time.Duration
	TimeoutSet bool
}

const connectorDefaultTimeout = 30 * time.Second

func NormalizedConnectorTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return connectorDefaultTimeout
	}
	return timeout
}

type PolicyIdentity struct {
	UID        string
	Generation int64
}

type FrozenConnection struct {
	Name             string
	UID              string
	Generation       int64
	GrantSequence    int64
	PolicyUID        string
	PolicyGeneration int64
	Provider         string
}

const builtinConnectionKeyPrefix = "builtin:"

func BuiltinConnectionKey(toolName string) string {
	return builtinConnectionKeyPrefix + strings.TrimSpace(toolName)
}

type ConnectionCredentialRequest struct {
	Namespace string
	Provider  string
	Issuer    string
	Subject   string
	Frozen    FrozenConnection
	Tool      ToolBinding
}

type ConnectionCredential struct {
	AccessToken   string
	TokenType     string
	ConnectionUID string
	Generation    int64
	Mode          string
}

type ConnectionCredentialSource interface {
	ResolveConnectionCredential(context.Context, ConnectionCredentialRequest) (ConnectionCredential, error)
}

type ConnectionLivenessChecker interface {
	RequireLiveConnection(context.Context, ConnectionCredentialRequest) error
}

type Resolution struct {
	Adapter string

	CredentialHeader string
	CredentialValue  string

	GatewayScheme string
	GatewayHost   string
	GatewayTLS    tokenexchange.TLSConfig

	SensitiveValues []string

	ConnectionUID string
	Parameters    *apiextensionsv1.JSON
}

type Resolver interface {
	Resolve(context.Context, ResolveRequest) (Resolution, error)
}

type KubernetesResolver struct {
	Reader                   client.Reader
	KubeClient               kubernetes.Interface
	Trust                    TrustConfig
	Exchanger                tokenexchange.Exchanger
	Connections              ConnectionCredentialSource
	AgentIdentities          AgentIdentityTokenSource
	MaxDelegationAge         time.Duration
	Now                      func() time.Time
	AuditLog                 logr.Logger
	AuditPseudonymKey        []byte
	DelegationLive           func(context.Context, AgentIdentityBinding) error
	ReservedServiceAccount   ReservedServiceAccount
	IdentityEnforcementStart func() (since time.Time, started bool)

	exchangeOnce     sync.Once
	defaultExchanger tokenexchange.Exchanger
}

func (r *KubernetesResolver) Resolve(ctx context.Context, req ResolveRequest) (resolution Resolution, err error) {
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
	defer r.auditAgentIdentityUse(policy, &req, &err)
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
	if err := validatePolicyCredentialAuthority(req, policy); err != nil {
		return Resolution{}, err
	}
	if issue, err := ResolveReferences(ctx, r.Reader, policy, r.Trust); err != nil {
		return Resolution{}, err
	} else if issue != nil {
		return Resolution{}, issue
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
	if injectsCredential(policy) {
		if !strings.EqualFold(strings.TrimSpace(req.TargetScheme), schemeHTTPS) {
			return Resolution{}, errors.New("credential-injecting outbound access requires an HTTPS Tool URL")
		}
		if req.HasAuthSecretRef {
			return Resolution{}, errors.New("credential-injecting outbound access cannot coexist with authSecretRef")
		}
	}
	if _, frozen := req.FrozenConnections[policy.Name]; frozen && !connectionBacked(policy) {
		return Resolution{}, fmt.Errorf("outbound access policy %q was in connection mode when the task was dispatched and no longer is", policy.Name)
	}
	switch {
	case policy.Spec.Direct != nil:
		return r.resolveDirect(ctx, policy, req)
	case policy.Spec.Connection != nil:
		return r.resolveConnection(ctx, policy, req)
	case policy.Spec.AgentIdentity != nil:
		resolution, err = r.resolveAgentIdentity(ctx, policy, req)
		return resolution, refuseAgentIdentity(agentIdentityRefusalBinding, err)
	}
	return r.resolveGateway(ctx, policy)
}

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
		declaredForward := corev1alpha1.HTTPForwardProtocol("")
		if candidate.HTTP.Forward != nil {
			declaredForward = candidate.HTTP.Forward.Protocol
		}
		if declaredForward != tool.Forward {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q does not match the forwarding declared by provider %q", name, provider.Name)
		}
		if strings.TrimSpace(tool.URL) != candidate.HTTP.URL || (tool.Forward == "" && toolMethod != method) {
			return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q does not match the endpoint declared by provider %q", name, provider.Name)
		}
		if tool.Forward != "" && tool.Class == corev1alpha1.AgentRuntimeBrokeredToolClassWrite {
			if candidate.Class != corev1alpha1.ConnectorToolClassRead || candidate.HTTP.Forward.Writes == nil {
				return corev1alpha1.ConnectorTool{}, fmt.Errorf("tool %q writes, which provider %q does not declare for it", name, provider.Name)
			}
			candidate.Class = corev1alpha1.ConnectorToolClassWrite
		} else if string(candidate.Class) != string(tool.Class) {
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
	timeout, _ := connectors.BuiltinConnectorToolTimeout(tool.Name)
	if !tool.TimeoutSet || tool.Timeout != timeout {
		return corev1alpha1.ConnectorTool{}, fmt.Errorf("built-in tool %q binding must carry the catalog timeout %s", tool.Name, timeout)
	}
	return candidate, nil
}

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

func sameStaticHeaders(declared, actual map[string]string) bool {
	canonicalize := func(headers map[string]string) (map[string]string, bool) {
		result := make(map[string]string, len(headers))
		for name, value := range headers {
			key := http.CanonicalHeaderKey(strings.TrimSpace(name))
			if _, dup := result[key]; dup {
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

func (r *KubernetesResolver) resolveConnection(ctx context.Context, policy *corev1alpha1.OutboundAccessPolicy, req ResolveRequest) (Resolution, error) {
	credential, declared, err := r.resolveRequesterConnection(ctx, policy, req)
	if err != nil {
		return Resolution{}, err
	}
	header, prefix := credentialOutput(policy.Spec.Connection.Output)
	return Resolution{
		Adapter:          AdapterConnection,
		CredentialHeader: header,
		CredentialValue:  prefix + credential.AccessToken,
		SensitiveValues:  compactSensitiveValues([]string{credential.AccessToken}),
		ConnectionUID:    credential.ConnectionUID,
		Parameters:       declared.Parameters,
	}, nil
}

func (r *KubernetesResolver) resolveRequesterConnection(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	req ResolveRequest,
) (ConnectionCredential, corev1alpha1.ConnectorTool, error) {
	providerName, ok := ConnectionProviderName(policy)
	if !ok {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("the policy does not draw on a ConnectorProvider")
	}
	if r.Connections == nil {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("connector-backed tools execute only in the controller")
	}
	requester := req.Requester
	if requester == nil || strings.TrimSpace(requester.Issuer) == "" || strings.TrimSpace(requester.Subject) == "" {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("connection outbound access requires a Task with a verified requester")
	}
	if req.MCPBacked {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("connection outbound access policies are not supported on MCP-backed tools")
	}
	if strings.Contains(req.Tool.URL, "{{") {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("connection outbound access requires a Tool URL without template placeholders")
	}
	frozen, ok := req.FrozenConnections[policy.Name]
	if !ok || strings.TrimSpace(frozen.UID) == "" {
		if refusal := r.unfrozenConnectionRefusal(ctx, policy.Namespace, providerName, requester); refusal != nil {
			r.describeConnectionRefusal(ctx, refusal, policy.Namespace, providerName, nil)
			return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, refusal
		}
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, fmt.Errorf("connection outbound access policy %q has no Connection frozen into the execution snapshot", policy.Name)
	}
	if frozen.PolicyUID != "" && (string(policy.UID) != frozen.PolicyUID || policy.Generation != frozen.PolicyGeneration) {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, fmt.Errorf("connection outbound access policy %q changed since the task was dispatched; re-dispatch to use it", policy.Name)
	}
	if frozen.Provider != "" && frozen.Provider != providerName {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, fmt.Errorf("connection outbound access policy %q selects a different provider than the one frozen for it", policy.Name)
	}
	if req.Tool.Builtin {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("a built-in tool has no outbound access policy")
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: providerName}, provider); err != nil {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, fmt.Errorf("resolve connector provider: %w", err)
	}
	declared, err := DeclaredConnectorTool(provider, req.Tool)
	if err != nil {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, err
	}
	if err := connectors.ValidateToolArguments(declared.Parameters, req.Arguments); err != nil {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, fmt.Errorf("connection credential request arguments rejected: %w", err)
	}
	credential, err := r.Connections.ResolveConnectionCredential(ctx, ConnectionCredentialRequest{
		Namespace: policy.Namespace,
		Provider:  providerName,
		Issuer:    requester.Issuer,
		Subject:   requester.Subject,
		Frozen:    frozen,
		Tool:      req.Tool,
	})
	if err != nil {
		if refusal, ok := errors.AsType[*ConnectionRequiredError](err); ok {
			described := *refusal
			r.describeConnectionRefusal(ctx, &described, policy.Namespace, providerName, provider)
			err = &described
		}
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, fmt.Errorf("resolve connection credential: %w", err)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("connection credential source returned an empty credential")
	}
	if declared.Class == corev1alpha1.ConnectorToolClassWrite && credential.Mode != corev1alpha1.ConnectionModeReadWrite {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New("connection is readOnly; write tools are not available")
	}
	return credential, declared, nil
}

func (r *KubernetesResolver) unfrozenConnectionRefusal(ctx context.Context, namespace, provider string, requester *corev1alpha1.RequestedBy) *ConnectionRequiredError {
	if r.Reader == nil {
		return nil
	}
	connections := &corev1alpha1.ConnectionList{}
	if err := r.Reader.List(ctx, connections, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var links []*corev1alpha1.Connection
	for i := range connections.Items {
		connection := &connections.Items[i]
		if connection.Spec.ProviderRef.Name == provider && connection.Spec.Subject.Issuer == requester.Issuer &&
			connection.Spec.Subject.Subject == requester.Subject {
			links = append(links, connection)
		}
	}
	switch {
	case len(links) == 0:
		return &ConnectionRequiredError{Kind: ConnectionNotLinked}
	case len(links) > 1:
		return &ConnectionRequiredError{Kind: ConnectionAmbiguous, Links: len(links)}
	}
	switch state := links[0].Status.State; state {
	case corev1alpha1.ConnectionStateExpired:
		return &ConnectionRequiredError{Kind: ConnectionExpired}
	case corev1alpha1.ConnectionStateReady:
		return &ConnectionRequiredError{Kind: ConnectionNotBound}
	default:
		return &ConnectionRequiredError{Kind: ConnectionNotReady, State: state}
	}
}

func (r *KubernetesResolver) describeConnectionRefusal(
	ctx context.Context, refusal *ConnectionRequiredError, namespace, providerName string, provider *corev1alpha1.ConnectorProvider,
) {
	refusal.Provider, refusal.Namespace = providerName, namespace
	if provider == nil && r.Reader != nil {
		candidate := &corev1alpha1.ConnectorProvider{}
		if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: providerName}, candidate); err == nil {
			provider = candidate
		}
	}
	if provider != nil {
		refusal.DisplayName = strings.TrimSpace(provider.Spec.DisplayName)
	}
}

func credentialOutput(output *corev1alpha1.OutboundCredentialOutput) (string, string) {
	header := defaultCredentialHeader
	prefix := defaultCredentialPrefix
	if output != nil {
		if strings.TrimSpace(output.Header) != "" {
			header = http.CanonicalHeaderKey(strings.TrimSpace(output.Header))
		}
		if output.Prefix != nil {
			prefix = *output.Prefix
		}
	}
	return header, prefix
}

func connectionBacked(policy *corev1alpha1.OutboundAccessPolicy) bool {
	_, ok := ConnectionProviderName(policy)
	return ok
}

func injectsCredential(policy *corev1alpha1.OutboundAccessPolicy) bool {
	return policy.Spec.Direct != nil || policy.Spec.Connection != nil || policy.Spec.AgentIdentity != nil
}

func (r *KubernetesResolver) resolveAgentIdentity(ctx context.Context, policy *corev1alpha1.OutboundAccessPolicy, req ResolveRequest) (Resolution, error) {
	if r.AgentIdentities == nil {
		return Resolution{}, errors.New("agent identity credentials are issued only by the controller")
	}
	binding := req.AgentIdentity
	if binding == nil || strings.TrimSpace(binding.Identity.ID) == "" {
		return Resolution{}, errors.New("agent identity outbound access requires a Task bound to an Agent with an identity")
	}
	if err := r.requireLiveIdentityAgent(ctx, *binding); err != nil {
		return Resolution{}, err
	}
	identity := policy.Spec.AgentIdentity
	if identity.Entra == nil || binding.Identity.Issuer != corev1alpha1.AgentIdentityIssuerEntra {
		return Resolution{}, fmt.Errorf("agent identity outbound access policy %q targets an issuer the Task's identity is not from", policy.Name)
	}
	if req.MCPBacked {
		return Resolution{}, errors.New("agent identity outbound access policies are not supported on MCP-backed tools")
	}
	if strings.Contains(req.Tool.URL, "{{") {
		return Resolution{}, errors.New("agent identity outbound access requires a Tool URL without template placeholders")
	}
	if req.Tool.Builtin {
		return Resolution{}, errors.New("a built-in tool has no outbound access policy")
	}
	tokenRequest := AgentIdentityTokenRequest{
		Identity: binding.Identity,
		Mode:     AgentIdentityMode(identity),
		Resource: identity.Entra.Resource,
		Policy:   PolicyIdentity{UID: string(policy.UID), Generation: policy.Generation},
	}
	var connectionUID string
	var parameters *apiextensionsv1.JSON
	if tokenRequest.Mode == corev1alpha1.OutboundAgentIdentityModeDelegated {
		credential, declared, err := r.resolveDelegation(ctx, policy, req)
		if err != nil {
			return Resolution{}, refuseAgentIdentity(agentIdentityRefusalDelegation, err)
		}
		tokenRequest.UserAssertion = credential.AccessToken
		connectionUID, parameters = credential.ConnectionUID, declared.Parameters
	}
	token, err := r.issueAgentIdentityToken(ctx, policy, req, tokenRequest)
	if err != nil {
		return Resolution{}, err
	}
	header, prefix := credentialOutput(identity.Output)
	return Resolution{
		Adapter:          AdapterAgentIdentity,
		CredentialHeader: header,
		CredentialValue:  prefix + token,
		SensitiveValues:  compactSensitiveValues([]string{token, tokenRequest.UserAssertion}),
		ConnectionUID:    connectionUID,
		Parameters:       parameters,
	}, nil
}

func (r *KubernetesResolver) requireLiveIdentityAgent(ctx context.Context, binding AgentIdentityBinding) error {
	live := &corev1alpha1.Agent{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: binding.AgentNamespace, Name: binding.AgentName}, live); err != nil {
		return fmt.Errorf("resolve the Task's bound Agent: %w", err)
	}
	if !live.DeletionTimestamp.IsZero() {
		return errors.New("the Task's bound Agent is being deleted")
	}
	if string(live.UID) != binding.AgentUID || live.Status.IdentityClaim == nil ||
		live.Status.IdentityClaim.Issuer != binding.Identity.Issuer || live.Status.IdentityClaim.ID != binding.Identity.ID {
		return errors.New("the Task's bound Agent no longer holds its identity")
	}
	issuer, id, named := live.IdentityRef()
	if live.Status.IdentityClaim.Releasing || !named || issuer != binding.Identity.Issuer || id != binding.Identity.ID {
		return errors.New("the Task's bound Agent no longer names its identity")
	}
	grantedAt := live.Status.IdentityClaim.GrantedAt
	if grantedAt == nil || !binding.TaskCreated.After(grantedAt.Time) {
		return errors.New("the Task was created before the Agent's current identity grant")
	}
	if r.IdentityEnforcementStart != nil {
		since, started := r.IdentityEnforcementStart()
		if !started {
			return errors.New("the Agent use enforcement period has not started yet")
		}
		if !since.IsZero() && !binding.TaskCreated.After(since) {
			return errors.New("the Task was created before the current period of Agent use enforcement")
		}
	}
	if err := issuerRefusedIdentity(live); err != nil {
		return err
	}
	return nil
}

func (r *KubernetesResolver) issueAgentIdentityToken(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	req ResolveRequest,
	tokenRequest AgentIdentityTokenRequest,
) (string, error) {
	token, err := r.AgentIdentities.AgentIdentityToken(ctx, tokenRequest)
	if err != nil {
		return "", refuseAgentIdentity(agentIdentityRefusalIssuer, fmt.Errorf("issue agent identity credential: %w", err))
	}
	if strings.TrimSpace(token) == "" {
		return "", refuseAgentIdentity(agentIdentityRefusalIssuer, errors.New("agent identity issuer returned an empty credential"))
	}
	binding := *req.AgentIdentity
	if err := r.requireLiveIdentityAgent(ctx, binding); err != nil {
		return "", fmt.Errorf("the Agent's identity changed while its credential was issued: %w", err)
	}
	if err := r.requireUnchangedPolicy(ctx, policy); err != nil {
		return "", refuseAgentIdentity(agentIdentityRefusalPolicy, err)
	}
	if tokenRequest.Mode != corev1alpha1.OutboundAgentIdentityModeDelegated {
		return token, nil
	}
	if r.DelegationLive == nil {
		return "", refuseAgentIdentity(agentIdentityRefusalDelegation, errors.New("delegated agent identity access requires the controller's Task and Session check"))
	}
	if err := r.requireLiveRequesterConnection(ctx, policy, req); err != nil {
		return "", refuseAgentIdentity(agentIdentityRefusalDelegation, fmt.Errorf("%w: %w", ErrRequesterLinkChanged, err))
	}
	if err := r.DelegationLive(ctx, binding); err != nil {
		return "", refuseAgentIdentity(agentIdentityRefusalDelegation, fmt.Errorf("the delegation ended while its credential was issued: %w", err))
	}
	if r.delegationExpired(policy, binding) {
		return "", refuseAgentIdentity(agentIdentityRefusalDelegation, errDelegationExpired)
	}
	return token, nil
}

func (r *KubernetesResolver) requireUnchangedPolicy(ctx context.Context, issued *corev1alpha1.OutboundAccessPolicy) error {
	live := &corev1alpha1.OutboundAccessPolicy{}
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(issued), live); err != nil {
		if apierrors.IsNotFound(err) {
			return errors.New("the outbound access policy was deleted while its credential was issued")
		}
		return fmt.Errorf("re-check the outbound access policy: %w", err)
	}
	if live.UID != issued.UID || live.Generation != issued.Generation || !live.DeletionTimestamp.IsZero() ||
		!policyConditionCurrentTrue(live, corev1alpha1.OutboundAccessPolicyConditionAccepted) ||
		!policyConditionCurrentTrue(live, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs) {
		return errors.New("the outbound access policy changed while its credential was issued; re-dispatch the task")
	}
	return nil
}

var ErrRequesterLinkChanged = errors.New("the requester's link changed while the credential was issued")

func (r *KubernetesResolver) requireLiveRequesterConnection(ctx context.Context, policy *corev1alpha1.OutboundAccessPolicy, req ResolveRequest) error {
	checker, ok := r.Connections.(ConnectionLivenessChecker)
	if !ok {
		return errors.New("the connection credential source cannot re-check the Connection")
	}
	provider, ok := ConnectionProviderName(policy)
	frozen, bound := req.FrozenConnections[policy.Name]
	if !ok || !bound || req.Requester == nil {
		return errors.New("the Task has no requester Connection frozen for the policy")
	}
	err := checker.RequireLiveConnection(ctx, ConnectionCredentialRequest{
		Namespace: policy.Namespace, Provider: provider,
		Issuer: req.Requester.Issuer, Subject: req.Requester.Subject,
		Frozen: frozen, Tool: req.Tool,
	})
	if refusal, ok := errors.AsType[*ConnectionRequiredError](err); ok {
		described := *refusal
		r.describeConnectionRefusal(ctx, &described, policy.Namespace, provider, nil)
		return &described
	}
	return err
}

var errDelegationExpired = errors.New("the delegation has expired; the requester must act again for the Agent to act for them")

func (r *KubernetesResolver) delegationExpired(policy *corev1alpha1.OutboundAccessPolicy, binding AgentIdentityBinding) bool {
	age, _ := EffectiveMaxDelegationAge(policy, r.MaxDelegationAge)
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	return binding.DelegationStart.IsZero() || now.After(binding.DelegationStart.Add(age))
}

const (
	DefaultMaxDelegationAge    = time.Hour
	DefaultMaxDelegationAgeCap = 24 * time.Hour
)

func EffectiveMaxDelegationAge(policy *corev1alpha1.OutboundAccessPolicy, limit time.Duration) (effective, requested time.Duration) {
	if limit <= 0 {
		limit = DefaultMaxDelegationAgeCap
	}
	requested = DefaultMaxDelegationAge
	if identity := policy.Spec.AgentIdentity; identity != nil && identity.Delegated != nil && identity.Delegated.MaxDelegationAge != nil {
		requested = identity.Delegated.MaxDelegationAge.Duration
	}
	return min(requested, limit), requested
}

func (r *KubernetesResolver) resolveDelegation(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	req ResolveRequest,
) (ConnectionCredential, corev1alpha1.ConnectorTool, error) {
	binding := req.AgentIdentity
	if !binding.ManagedEgress {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New(
			"delegated agent identity access runs only on built-in RuntimePools, whose network Orka controls")
	}
	if r.delegationExpired(policy, *binding) {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errDelegationExpired
	}
	if r.DelegationLive == nil {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, errors.New(
			"delegated agent identity access requires the controller's Task and Session check")
	}
	if err := r.DelegationLive(ctx, *binding); err != nil {
		return ConnectionCredential{}, corev1alpha1.ConnectorTool{}, fmt.Errorf("the delegation is no longer live: %w", err)
	}
	return r.resolveRequesterConnection(ctx, policy, req)
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
	prefix := defaultCredentialPrefix
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
		if r.ReservedServiceAccount.Reserves(namespace, source.ServiceAccountRef.Name) {
			return resolvedToken{}, errReservedServiceAccount
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
