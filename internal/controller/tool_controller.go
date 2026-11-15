/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	goerrors "errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workspace"
)

const (
	toolConditionAvailable = "Available"
	// toolConditionEgressPolicyReady reports whether a Substrate MCP actor's
	// native egress policy was installed and verified before resume.
	toolConditionEgressPolicyReady = "EgressPolicyReady"

	// toolHealthCheckInterval is how often tools are re-checked.
	toolHealthCheckInterval = 5 * time.Minute

	// toolHealthCheckTimeout is the HTTP timeout for health checks.
	toolHealthCheckTimeout = 10 * time.Second

	mcpEndpointReadinessPollInterval = 250 * time.Millisecond

	substrateMCPToolActorFinalizer = "orka.ai/substrate-mcp-tool-actor-cleanup"
	substrateMCPToolActorIDAnno    = "orka.ai/substrate-mcp-tool-actor-id"
	substrateMCPToolBootedIDAnno   = "orka.ai/substrate-mcp-tool-booted-id"

	substrateMCPToolActorPoolNameAnno      = "orka.ai/substrate-mcp-tool-actor-pool-name"
	substrateMCPToolActorPoolNamespaceAnno = "orka.ai/substrate-mcp-tool-actor-pool-namespace"

	substrateMCPToolCleanupActorIDAnno        = "orka.ai/substrate-mcp-tool-cleanup-actor-id"
	substrateMCPToolCleanupPoolNameAnno       = "orka.ai/substrate-mcp-tool-cleanup-pool-name"
	substrateMCPToolCleanupPoolNamespaceAnno  = "orka.ai/substrate-mcp-tool-cleanup-pool-namespace"
	substrateMCPToolCleanupNonPooledValueAnno = "non-pooled"
)

// ToolReconciler reconciles a Tool object
type ToolReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// HTTPClient is the HTTP client used for health checks. If nil, a default is used.
	HTTPClient *http.Client

	// SkipSSRFValidation disables SSRF protection for testing. Do NOT set to true in production.
	SkipSSRFValidation bool

	// AllowPrivateConnectorEndpoints mirrors --connectors-allow-private-endpoints:
	// a Tool behind a connection-mode or agentIdentity outbound access policy
	// may target a private or cluster-local endpoint. Local fixtures only.
	AllowPrivateConnectorEndpoints bool

	// SubstrateMCPToolsEnabled enables durable MCP tool actors.
	SubstrateMCPToolsEnabled    bool
	SubstrateConfig             SubstrateConfig
	EnforceNamespaceIsolation   bool
	WorkspaceProviderAPIEnabled bool
	OutboundAccessTrust         outboundaccess.TrustConfig

	// SubstrateExecutorFactory is injectable for tests.
	SubstrateTemplateValidator func(context.Context, *ExecutionWorkspaceRequest) error
	SubstrateExecutorFactory   func(SubstrateConfig) (workspace.WorkspaceExecutor, error)
	// SubstrateEgressPolicyApplier installs and verifies an MCP actor's
	// native egress policy. Nil uses the Substrate control API. Injectable
	// for tests.
	SubstrateEgressPolicyApplier func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error
	// SubstrateEgressPolicyStateApplier is SubstrateEgressPolicyApplier for a
	// test that also sets when the stored policy was written. It takes
	// precedence.
	SubstrateEgressPolicyStateApplier func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) (workspace.SubstrateEgressPolicyState, error)
	// SubstrateRouterHTTPClient reaches MCP actors for readiness checks. Nil
	// builds the mTLS router client from SubstrateConfig. Injectable for tests.
	SubstrateRouterHTTPClient *http.Client
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=tools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=outboundaccesspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile validates the Tool configuration, performs a health check on the HTTP endpoint,
// and updates the Tool's status accordingly.
func (r *ToolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Tool
	tool := &corev1alpha1.Tool{}
	if err := r.Get(ctx, req.NamespacedName, tool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Tool", "tool", tool.Name, "endpoint", safeToolURLIdentity(tool))
	if !tool.DeletionTimestamp.IsZero() {
		return r.finalizeSubstrateMCPTool(ctx, tool)
	}
	if (tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil) &&
		controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
		return r.finalizeSubstrateMCPTool(ctx, tool)
	}

	// Accepted is remote-only, including while a new actor backend is still being provisioned.
	if !aitools.IsRemoteMCP(tool) && meta.RemoveStatusCondition(&tool.Status.Conditions, "Accepted") {
		if err := r.Status().Update(ctx, tool); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Validate the tool configuration
	if err := r.validateTool(ctx, tool); err != nil {
		logger.Error(err, "Tool validation failed")
		if aitools.IsRemoteMCP(tool) {
			return r.updateRemoteMCPStatus(ctx, tool, err)
		}
		if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
			return r.failSubstrateMCPToolValidation(ctx, tool, err)
		}
		return r.updateStatus(ctx, tool, false, err.Error())
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
		return r.reconcileSubstrateMCPTool(ctx, tool)
	}

	if aitools.IsRemoteMCP(tool) {
		return r.updateRemoteMCPStatus(ctx, tool, nil)
	}

	if tool.Spec.HTTP.GatewayTaskPass {
		// There is no controller-wide probe authority; issuance and readiness
		// are verified under the bound Task on every call.
		return r.updateStatus(ctx, tool, true, "")
	}

	// Perform health check on the HTTP endpoint
	if err := r.healthCheck(ctx, tool); err != nil {
		logger.Info("Tool health check failed", "tool", tool.Name, "error", err.Error())
		return r.updateStatus(ctx, tool, false, err.Error())
	}

	// Tool is valid and reachable
	return r.updateStatus(ctx, tool, true, "")
}

func (r *ToolReconciler) updateRemoteMCPStatus(ctx context.Context, tool *corev1alpha1.Tool, validationErr error) (ctrl.Result, error) {
	// A controller-wide HEAD request cannot establish Task-authorized MCP readiness.
	previous := tool.Status.DeepCopy()
	tool.Status.Available = false
	tool.Status.LastCheck = nil
	tool.Status.Error = ""
	tool.Status.Endpoint = ""
	tool.Status.Actor = nil
	tool.Status.Workspace = nil
	accepted := metav1.Condition{
		Type: "Accepted", Status: metav1.ConditionTrue, Reason: "RemoteConfigurationAccepted",
		Message: "Remote configuration accepted; discovery is verified under Task authority", ObservedGeneration: tool.Generation,
	}
	available := metav1.Condition{
		Type: toolConditionAvailable, Status: metav1.ConditionUnknown, Reason: "TaskVerificationRequired",
		Message: "Authenticated MCP readiness is verified by the native worker", ObservedGeneration: tool.Generation,
	}
	if validationErr != nil {
		tool.Status.Error = validationErr.Error()
		accepted.Status, accepted.Reason, accepted.Message = metav1.ConditionFalse, "InvalidConfiguration", validationErr.Error()
		available.Status, available.Reason, available.Message = metav1.ConditionFalse, "InvalidConfiguration", validationErr.Error()
	}
	meta.SetStatusCondition(&tool.Status.Conditions, accepted)
	meta.SetStatusCondition(&tool.Status.Conditions, available)
	if reflect.DeepEqual(*previous, tool.Status) {
		return ctrl.Result{RequeueAfter: toolHealthCheckInterval}, nil
	}
	return ctrl.Result{RequeueAfter: toolHealthCheckInterval}, r.Status().Update(ctx, tool)
}

// validateTool validates the Tool spec.
func (r *ToolReconciler) validateTool(ctx context.Context, tool *corev1alpha1.Tool) error {
	// Validate description
	if tool.Spec.Description == "" {
		return fmt.Errorf("description is required")
	}
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.GatewayTaskPass {
		return workerexecutor.ValidateGatewayTaskPassTool(tool)
	}
	if aitools.IsRemoteMCP(tool) {
		if err := aitools.ValidateRemoteMCPConfiguration(tool); err != nil {
			return err
		}
		if err := r.validateToolHTTPURL(tool.Spec.MCP.Remote.URL, false); err != nil {
			return err
		}
	}
	if err := r.validateToolHTTPAuth(ctx, tool); err != nil {
		return err
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
		return r.validateSubstrateMCPTool(ctx, tool)
	}
	if aitools.IsRemoteMCP(tool) {
		return nil
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.Workspace != nil {
		if !r.WorkspaceProviderAPIEnabled {
			return fmt.Errorf("mcp.workspace requires the workspace provider API")
		}
		return fmt.Errorf("mcp.workspace requires controller-first Tool workspace integration")
	}
	if tool.Spec.HTTP == nil {
		return fmt.Errorf("http is required unless mcp.substrateActor is set")
	}
	// Admission cannot see the schemaless parameters field, so a forwarding
	// Tool's schema is refused here, as prompt policy refuses it.
	if tool.Spec.HTTP.Forward != nil && tool.Spec.Parameters != nil && len(tool.Spec.Parameters.Raw) > 0 {
		return fmt.Errorf("a forwarding tool takes no parameter schema; its protocol defines the call")
	}
	// Only a Tool behind a connection-mode or agentIdentity outbound access
	// policy may point at a private endpoint under the fixture allowance.
	allowPrivate := false
	if r.AllowPrivateConnectorEndpoints {
		fixtureMode, err := r.toolPolicyAllowsPrivateFixture(ctx, tool)
		if err != nil {
			return err
		}
		allowPrivate = fixtureMode
	}
	if allowPrivate {
		// The allowance relaxes where such a call may go, never how:
		// execution refuses plain http for credential-injecting policies,
		// so a private endpoint is accepted only over HTTPS and the Tool's
		// status matches what can actually run.
		if parsed, err := url.Parse(strings.TrimSpace(tool.Spec.HTTP.URL)); err != nil || !strings.EqualFold(parsed.Scheme, "https") {
			return fmt.Errorf("a private connector endpoint requires an HTTPS Tool URL")
		}
	}
	return r.validateToolHTTPURL(tool.Spec.HTTP.URL, allowPrivate)
}

// toolPolicyAllowsPrivateFixture reports whether the Tool's outbound access
// policy injects a linked-account or Agent identity credential, the two modes
// the fixture-only private-endpoint allowance covers.
func (r *ToolReconciler) toolPolicyAllowsPrivateFixture(ctx context.Context, tool *corev1alpha1.Tool) (bool, error) {
	ref := tool.Spec.HTTP.OutboundAccessPolicyRef
	if ref == nil {
		return false, nil
	}
	policy := &corev1alpha1.OutboundAccessPolicy{}
	if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: tool.Namespace}, policy); err != nil {
		return false, fmt.Errorf("failed to get outbound access policy %q: %w", ref.Name, err)
	}
	_, connectorBacked := outboundaccess.ConnectionProviderName(policy)
	return connectorBacked || policy.Spec.AgentIdentity != nil, nil
}

// validateToolHTTPURL checks the Tool's endpoint. With allowPrivate the
// private, loopback, and link-local address rules are skipped, for local
// fixtures only; the fixed metadata and API server hosts stay blocked.
func (r *ToolReconciler) validateToolHTTPURL(rawURL string, allowPrivate bool) error {
	// Validate URL
	if rawURL == "" {
		return fmt.Errorf("http.url is required")
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != urlSchemeHTTP && parsedURL.Scheme != "https") {
		return fmt.Errorf("invalid http.url")
	}
	if parsedURL.User != nil {
		return fmt.Errorf("http.url must not contain embedded credentials")
	}
	query, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return fmt.Errorf("invalid http.url query")
	}
	for name := range query {
		if sensitiveURLParameter(name) {
			return fmt.Errorf("http.url must not contain credential query parameters")
		}
	}

	// Block private/internal network targets (unless in test mode)
	if !r.SkipSSRFValidation {
		// DNS names are case-insensitive and a trailing dot names the
		// same host, so the fixed block list compares canonical forms.
		host := strings.TrimSuffix(strings.ToLower(parsedURL.Hostname()), ".")
		// The same infrastructure block list the connector validator keeps
		// under the fixture allowance: metadata services and the Kubernetes
		// API service under any cluster domain, by name or address.
		if connectors.InfrastructureHostDenied(host) {
			return fmt.Errorf("tool URL host %q is not allowed", host)
		}
		if allowPrivate {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return fmt.Errorf("tool URL must not target private/loopback addresses")
			}
		}
		// Resolve DNS hostnames and block private/loopback IPs
		if net.ParseIP(host) == nil {
			ips, err := net.LookupHost(host)
			if err == nil {
				for _, ipStr := range ips {
					ip := net.ParseIP(ipStr)
					if ip == nil {
						continue
					}
					if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
						return fmt.Errorf("tool URL resolves to private/loopback IP %s", ipStr)
					}
				}
			}
		}
	}

	return nil
}

//nolint:gocyclo // Each HTTP auth mode has its own validation branch.
func (r *ToolReconciler) validateToolHTTPAuth(ctx context.Context, tool *corev1alpha1.Tool) error {
	if tool == nil || tool.Spec.HTTP == nil {
		return nil
	}
	if ref := tool.Spec.HTTP.OutboundAccessPolicyRef; ref != nil {
		if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
			// Workers reach actors only through the Orka MCP proxy, which never
			// forwards caller-resolved outbound credentials or gateway routes.
			return fmt.Errorf("mcp.substrateActor tools do not support outboundAccessPolicyRef; use http.authSecretRef for actor credentials and mcp.substrateActor.egress for actor egress")
		}
		if strings.TrimSpace(ref.Name) == "" {
			return fmt.Errorf("outboundAccessPolicyRef.name is required")
		}
		policy := &corev1alpha1.OutboundAccessPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: tool.Namespace}, policy); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced outbound access policy %q not found", ref.Name)
			}
			return fmt.Errorf("failed to get outbound access policy %q: %w", ref.Name, err)
		}
		if !policy.DeletionTimestamp.IsZero() {
			return fmt.Errorf("outbound access policy %q is terminating", ref.Name)
		}
		if policy.Status.ObservedGeneration != policy.Generation {
			return fmt.Errorf("outbound access policy %q has not observed its current generation", ref.Name)
		}
		for _, conditionType := range []string{corev1alpha1.OutboundAccessPolicyConditionAccepted, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs} {
			condition := meta.FindStatusCondition(policy.Status.Conditions, conditionType)
			if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != policy.Generation {
				return fmt.Errorf("outbound access policy %q is not accepted with resolved references", ref.Name)
			}
		}
		if aitools.IsRemoteMCP(tool) && (policy.Spec.Gateway == nil || policy.Spec.Direct != nil ||
			policy.Spec.Connection != nil || policy.Spec.AgentIdentity != nil) {
			return fmt.Errorf("remote MCP requires a gateway outbound access policy")
		}
		if err := forwardingPolicySupported(tool, policy); err != nil {
			return err
		}
		if policy.Spec.Direct != nil || policy.Spec.Connection != nil || policy.Spec.AgentIdentity != nil {
			if tool.Spec.HTTP.AuthSecretRef != nil {
				return fmt.Errorf("credential-injecting outbound access policy %q cannot coexist with authSecretRef", ref.Name)
			}
			targetURL := strings.TrimSpace(tool.Spec.HTTP.URL)
			parsed, err := url.Parse(targetURL)
			if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
				return fmt.Errorf("direct outbound access policy %q requires an HTTPS Tool URL", ref.Name)
			}
		}
	}
	if tool.Spec.HTTP.AuthSecretRef != nil {
		secret := &corev1.Secret{}
		key := client.ObjectKey{Name: tool.Spec.HTTP.AuthSecretRef.Name, Namespace: tool.Namespace}
		if err := r.Get(ctx, key, secret); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced auth secret %q not found", tool.Spec.HTTP.AuthSecretRef.Name)
			}
			return fmt.Errorf("failed to get auth secret %q: %w", tool.Spec.HTTP.AuthSecretRef.Name, err)
		}
		if aitools.IsRemoteMCP(tool) && strings.TrimSpace(string(secret.Data[tool.Spec.HTTP.AuthSecretRef.Key])) == "" {
			return fmt.Errorf("remote MCP auth Secret key is empty or missing")
		}
		if _, ok := secret.Data[tool.Spec.HTTP.AuthSecretRef.Key]; !ok {
			return fmt.Errorf("key %q not found in auth secret %q", tool.Spec.HTTP.AuthSecretRef.Key, tool.Spec.HTTP.AuthSecretRef.Name)
		}
	}

	// Validate authInject + authBodyKey combination
	if tool.Spec.HTTP.AuthInject == repositoryScanPullRequestBodyField && tool.Spec.HTTP.AuthBodyKey == "" {
		return fmt.Errorf("authBodyKey is required when authInject is 'body'")
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil && tool.Spec.HTTP.AuthInject == repositoryScanPullRequestBodyField {
		return fmt.Errorf("MCP tools do not support authInject=body")
	}

	return nil
}

func (r *ToolReconciler) validateSubstrateMCPTool(ctx context.Context, tool *corev1alpha1.Tool) error {
	if !r.SubstrateMCPToolsEnabled {
		return fmt.Errorf("MCP substrateActor requires Substrate MCP tools to be enabled with --substrate-mcp-tools-enabled")
	}
	actor := tool.Spec.MCP.SubstrateActor
	if strings.TrimSpace(actor.TemplateRef.Name) == "" {
		return fmt.Errorf("mcp.substrateActor.templateRef.name is required")
	}
	if actor.PoolRef != nil && strings.TrimSpace(actor.PoolRef.Name) == "" {
		return fmt.Errorf("mcp.substrateActor.poolRef.name is required")
	}
	if _, err := workerexecutor.SubstrateMCPActorPath(tool); err != nil {
		return err
	}
	if err := validateSubstrateMCPToolHeaders(tool); err != nil {
		return err
	}
	if err := workspace.ValidateSubstrateEgressRules(substrateMCPEgressRules(tool, r.SubstrateConfig.EgressCredentialProvider)); err != nil {
		return fmt.Errorf("invalid mcp.substrateActor.egress: %w", err)
	}
	if err := r.validateSubstrateMCPEgressCredentials(ctx, tool); err != nil {
		return err
	}
	templateRequest := r.substrateMCPTemplateRequest(tool)
	if r.EnforceNamespaceIsolation && templateRequest.TemplateNamespace != tool.Namespace {
		return fmt.Errorf(
			"cross-namespace MCP substrate actor templateRef not allowed when namespace isolation is enforced: template %q in namespace %q, tool in %q",
			templateRequest.TemplateName,
			templateRequest.TemplateNamespace,
			tool.Namespace,
		)
	}
	if err := r.validateSubstrateMCPTemplate(ctx, templateRequest); err != nil {
		return err
	}
	if _, _, _, err := r.resolveSubstrateMCPActorPool(ctx, tool, templateRequest); err != nil {
		return err
	}
	return nil
}

func (r *ToolReconciler) validateSubstrateMCPTemplate(ctx context.Context, request *ExecutionWorkspaceRequest) error {
	if r.SubstrateTemplateValidator != nil {
		return r.SubstrateTemplateValidator(ctx, request)
	}
	return validateNativeSubstrateRoutableTemplate(ctx, r.SubstrateConfig, request)
}

func (r *ToolReconciler) substrateMCPTemplateRequest(tool *corev1alpha1.Tool) *ExecutionWorkspaceRequest {
	actorSpec := tool.Spec.MCP.SubstrateActor
	templateNamespace := strings.TrimSpace(actorSpec.TemplateRef.Namespace)
	if templateNamespace == "" {
		templateNamespace = tool.Namespace
	}
	cfg := r.SubstrateConfig.WithDefaults()
	return &ExecutionWorkspaceRequest{
		TemplateName:                 strings.TrimSpace(actorSpec.TemplateRef.Name),
		TemplateNamespace:            templateNamespace,
		SubstrateBootstrapSecretName: cfg.BootstrapSecretName,
		SubstrateBootstrapSecretKey:  cfg.BootstrapSecretKey,
	}
}

//nolint:gocyclo // Tool admission and Actor lifecycle checks must complete before publishing readiness.
func (r *ToolReconciler) reconcileSubstrateMCPTool(ctx context.Context, tool *corev1alpha1.Tool) (ctrl.Result, error) {
	actorSpec := tool.Spec.MCP.SubstrateActor
	templateRequest := r.substrateMCPTemplateRequest(tool)
	if err := r.validateSubstrateMCPTemplate(ctx, templateRequest); err != nil {
		return r.failSubstrateMCPTool(ctx, tool, err.Error())
	}
	actorID := deterministicSubstrateToolActorID(tool.Namespace, tool.Name, templateRequest.TemplateNamespace, templateRequest.TemplateName)
	poolName, poolNamespace, pool, err := r.resolveSubstrateMCPActorPool(ctx, tool, templateRequest)
	if err != nil {
		return r.failSubstrateMCPTool(ctx, tool, err.Error())
	}
	if changed, err := r.migrateSubstrateMCPIdentities(ctx, tool, templateRequest, pool); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	var poolRef *corev1alpha1.SubstrateActorPoolReference
	if poolName != "" {
		prefix := deterministicSubstratePoolActorPrefix(poolNamespace, poolName)
		ordinal := deterministicSubstratePoolActorOrdinal(
			pool.Spec.TargetActors,
			prefix,
			tool.Namespace,
			tool.Name,
			templateRequest.TemplateNamespace,
			templateRequest.TemplateName,
		)
		actorID = workspace.SubstrateActorKey(templateRequest.TemplateNamespace, deterministicSubstratePoolActorID(prefix, ordinal))
		poolRef = &corev1alpha1.SubstrateActorPoolReference{Name: poolName, Namespace: poolNamespace}
		if assignedActorID := assignedSubstrateMCPPoolActorID(tool, poolName, poolNamespace, prefix, int(pool.Spec.TargetActors)); assignedActorID != "" {
			actorID = assignedActorID
		}
	}
	cleanupRef, err := r.substrateMCPReplacementCleanupRef(ctx, tool, actorID)
	if err != nil {
		return r.failSubstrateMCPTool(ctx, tool, err.Error())
	}
	ownershipChanged := ensureSubstrateMCPToolActorOwnership(tool, actorID, poolRef)
	pendingCleanupChanged := ensureSubstrateMCPToolCleanupState(tool, actorID, cleanupRef)
	if ownershipChanged || pendingCleanupChanged {
		if err := r.Update(ctx, tool); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if poolRef != nil {
		reservedActorID, reserved, err := r.reserveSubstrateMCPPoolActor(ctx, tool, poolNamespace, actorID, int(pool.Spec.TargetActors))
		if err != nil {
			return r.failSubstrateMCPTool(ctx, tool, err.Error())
		}
		if !reserved {
			return r.failSubstrateMCPTool(ctx, tool, fmt.Sprintf("all substrate pool actors in %q/%q are leased", poolNamespace, poolName))
		}
		if reservedActorID != actorID {
			actorID = reservedActorID
			cleanupRef, err = r.substrateMCPReplacementCleanupRef(ctx, tool, actorID)
			if err != nil {
				return r.failSubstrateMCPTool(ctx, tool, err.Error())
			}
			ownershipChanged = ensureSubstrateMCPToolActorOwnership(tool, actorID, poolRef)
			pendingCleanupChanged = ensureSubstrateMCPToolCleanupState(tool, actorID, cleanupRef)
			if ownershipChanged || pendingCleanupChanged {
				if err := r.Update(ctx, tool); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
	}
	if poolRef == nil {
		if err := r.ensureSubstrateMCPToolActorLease(ctx, tool, actorID); err != nil {
			return r.failSubstrateMCPTool(ctx, tool, err.Error())
		}
	}
	cfg := r.SubstrateConfig.WithDefaults()
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(cfg.WorkspaceClientConfig())
		}
	}
	executor, err := executorFactory(cfg)
	if err != nil {
		return r.failSubstrateMCPTool(ctx, tool, err.Error())
	}
	defer closeWorkspaceExecutor(ctx, executor)
	claim, err := executor.Claim(ctx, workspace.ClaimRequest{
		Namespace:       templateRequest.TemplateNamespace,
		ClaimName:       actorID,
		CreateIfMissing: true,
		Template: workspace.TemplateRef{
			Namespace: templateRequest.TemplateNamespace,
			Name:      templateRequest.TemplateName,
			UID:       templateRequest.TemplateUID,
		},
		Timeout: cfg.ClaimTimeout,
	})
	if err != nil {
		return r.failSubstrateMCPTool(ctx, tool, err.Error())
	}
	stop := substrateMCPActorStop{
		actorID: actorID, ref: claim.Ref, leaseNamespace: tool.Namespace,
		suspendUnsupported: templateRequest.TemplateSuspendUnsupported,
	}
	if poolRef != nil {
		stop.pooled, stop.leaseNamespace = true, poolNamespace
	}
	if result, done, err := r.waitForSubstrateMCPToolActor(ctx, tool, executor, claim, stop, actorSpec.Boot, cfg.ClaimTimeout); done || err != nil {
		return result, err
	}

	// Workers and other callers reach the actor only through the Orka API
	// proxy; the router URL and actor target never leave the manager.
	endpoint := workerexecutor.SubstrateMCPProxyPath(tool.Namespace, tool.Name)
	templateRef := &corev1alpha1.WorkspaceTemplateReference{Name: templateRequest.TemplateName, Namespace: templateRequest.TemplateNamespace}
	actorStatus := &corev1alpha1.ToolActorStatus{
		Provider:    corev1alpha1.WorkspaceProviderSubstrate,
		ActorID:     actorID,
		TemplateRef: templateRef,
		PoolRef:     poolRef,
	}
	if err := r.waitForMCPActorEndpoint(ctx, cfg, tool, actorID, cfg.ClaimTimeout); err != nil {
		if cleanupRef != nil && tool.Status.Actor != nil {
			previousActor := *tool.Status.Actor
			return r.updateStatusWithActor(ctx, tool, false, err.Error(), tool.Status.Endpoint, &previousActor)
		}
		return r.updateStatusWithActor(ctx, tool, false, err.Error(), endpoint, actorStatus)
	}
	if err := r.cleanupPendingSubstrateMCPActor(ctx, executor, tool, cleanupRef, cfg.ClaimTimeout); err != nil {
		return ctrl.Result{}, err
	}
	return r.updateStatusWithActor(ctx, tool, true, "", endpoint, actorStatus)
}

func (r *ToolReconciler) waitForSubstrateMCPToolActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	executor workspace.WorkspaceExecutor,
	claim *workspace.ClaimResult,
	stop substrateMCPActorStop,
	bootRequested bool,
	timeout time.Duration,
) (ctrl.Result, bool, error) {
	actorID := stop.actorID
	bootActor := shouldBootSubstrateMCPToolActor(tool, actorID, bootRequested, claim.Created)
	// Substrate v1 routes all actor egress through the gateway under the
	// actor's native policy. Install and verify it before every resume; any
	// failure leaves the actor suspended.
	policy, err := r.applySubstrateMCPActorEgress(ctx, tool, actorID)
	if err != nil {
		message := "actor egress policy was not applied; actor not resumed: " + err.Error()
		var suspendErr error
		// An actor this claim created has never been resumed, and one observed
		// suspended has nothing running; neither needs a call.
		if !claim.Created && claim.Phase != workspace.PhaseRetained {
			suspendErr = r.stopSubstrateMCPActor(ctx, tool, executor, stop, timeout)
		}
		// A Tool that moved to another Actor still has its previous one
		// running until the replacement is ready, which it now is not.
		if previous := tool.Status.Actor; suspendErr == nil && previous != nil &&
			previous.Provider == corev1alpha1.WorkspaceProviderSubstrate &&
			strings.TrimSpace(previous.ActorID) != "" && strings.TrimSpace(previous.ActorID) != actorID {
			suspendErr = r.stopSubstrateMCPStatusActor(ctx, tool)
		}
		if suspendErr != nil {
			message += "; the actor could not be suspended and may still run under its previous egress policy: " + suspendErr.Error()
		}
		meta.SetStatusCondition(&tool.Status.Conditions, metav1.Condition{
			Type: toolConditionEgressPolicyReady, Status: metav1.ConditionFalse, Reason: "EgressPolicyFailed",
			Message: message, ObservedGeneration: tool.Generation,
		})
		result, updateErr := r.updateStatus(ctx, tool, false, message)
		if updateErr == nil && suspendErr != nil {
			// Retry with backoff until the actor is confirmed suspended.
			return ctrl.Result{}, true, suspendErr
		}
		return result, true, updateErr
	}
	if wait := r.substrateMCPEgressPropagationWait(policy); wait > 0 {
		// The stored policy is right, but the gateway may still apply other
		// rules under this Actor's name. Until it cannot, the Actor must not
		// run and the Tool is not offered.
		message := fmt.Sprintf("actor egress policy is stored; the egress gateway may apply earlier rules for up to %s more",
			wait.Round(time.Second))
		requeue := wait
		var stopErr error
		if !claim.Created && claim.Phase != workspace.PhaseRetained {
			if stopErr = r.stopSubstrateMCPActor(ctx, tool, executor, stop, timeout); stopErr != nil {
				message += "; the actor could not be stopped and may still reach what those rules allow: " + stopErr.Error()
			} else if stop.suspendUnsupported {
				// The Actor was deleted. Create its replacement now, so that
				// its own wait starts at once instead of after this one.
				requeue = time.Second
			}
		}
		meta.SetStatusCondition(&tool.Status.Conditions, metav1.Condition{
			Type: toolConditionEgressPolicyReady, Status: metav1.ConditionFalse, Reason: "EgressPolicyPropagating",
			Message: message, ObservedGeneration: tool.Generation,
		})
		if _, updateErr := r.updateStatus(ctx, tool, false, message); updateErr != nil {
			return ctrl.Result{}, true, updateErr
		}
		if stopErr != nil {
			// Retry with backoff until the actor is confirmed stopped.
			return ctrl.Result{}, true, stopErr
		}
		return ctrl.Result{RequeueAfter: requeue}, true, nil
	}
	meta.SetStatusCondition(&tool.Status.Conditions, metav1.Condition{
		Type: toolConditionEgressPolicyReady, Status: metav1.ConditionTrue, Reason: "EgressPolicyApplied",
		Message: "Actor egress policy is installed and verified", ObservedGeneration: tool.Generation,
	})
	if _, err := executor.WaitReady(ctx, workspace.WaitReadyRequest{
		Ref:                   claim.Ref,
		Template:              claim.Template,
		Timeout:               timeout,
		Boot:                  bootActor,
		SkipDaemonHealthCheck: true,
	}); err != nil {
		result, updateErr := r.updateStatus(ctx, tool, false, err.Error())
		return result, true, updateErr
	}
	if bootRequested && !substrateMCPToolActorBootRecorded(tool, actorID) {
		canContinue, err := r.recordSubstrateMCPToolActorBooted(ctx, tool, actorID)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if !canContinue {
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
	}
	return ctrl.Result{}, false, nil
}

// substrateMCPActorStop names an Actor that must not keep running because its
// egress policy is not verified for the Tool's current specification.
type substrateMCPActorStop struct {
	actorID string
	ref     workspace.WorkspaceRef
	// leaseNamespace holds the Actor's ownership lease: the pool's namespace
	// for a pooled Actor, otherwise the Tool's.
	leaseNamespace string
	pooled         bool
	// suspendUnsupported selects deletion; see
	// ExecutionWorkspaceRequest.TemplateSuspendUnsupported.
	suspendUnsupported bool
}

const substrateMCPActorStopReason = "egress policy was not verified"

// stopSubstrateMCPActor stops an Actor the Tool still owns. It may be running
// under the policy an earlier reconcile installed, which can be broader than
// the Tool now allows. An Actor that keeps data is suspended, and the next
// successful reconcile resumes it. One that Substrate cannot suspend is deleted
// under its UID, as Tool deletion does, and the next claim creates it again.
func (r *ToolReconciler) stopSubstrateMCPActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	executor workspace.WorkspaceExecutor,
	stop substrateMCPActorStop,
	timeout time.Duration,
) error {
	held, err := r.substrateMCPActorHeld(ctx, tool, stop)
	if err != nil || !held {
		// Without the lease the Actor is not this Tool's to stop.
		return err
	}
	if stop.suspendUnsupported {
		if err := deleteSubstrateMCPActor(ctx, executor, stop.actorID, substrateMCPActorStopReason, timeout); err != nil {
			return err
		}
		return r.forgetSubstrateMCPToolActor(ctx, tool, stop.actorID)
	}
	// MCP actors run no workspace daemon, so there is nothing to scrub.
	_, err = executor.Release(ctx, workspace.ReleaseRequest{
		Ref:       stop.ref,
		Retain:    true,
		Reason:    substrateMCPActorStopReason,
		Timeout:   timeout,
		SkipScrub: true,
	})
	return err
}

// substrateMCPActorHeld reports whether the Tool holds the lease that makes
// the Actor its own.
func (r *ToolReconciler) substrateMCPActorHeld(ctx context.Context, tool *corev1alpha1.Tool, stop substrateMCPActorStop) (bool, error) {
	var held bool
	var err error
	if stop.pooled {
		_, held, err = r.substrateMCPPoolActorLeaseHeldByTool(ctx, tool, stop.leaseNamespace, stop.actorID)
	} else {
		_, held, err = r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, stop.leaseNamespace, stop.actorID)
	}
	return held, err
}

// forgetSubstrateMCPToolActor drops what the Tool recorded about an Actor it
// has deleted: that status names it, and that it was booted. The Actor created
// in its place under the same name has never run, and may be resumed by a later
// reconcile than the one that creates it, which must still ask for a boot from
// scratch. The caller writes the status.
func (r *ToolReconciler) forgetSubstrateMCPToolActor(ctx context.Context, tool *corev1alpha1.Tool, actorID string) error {
	if previous := tool.Status.Actor; previous != nil && strings.TrimSpace(previous.ActorID) == actorID {
		tool.Status.Actor = nil
	}
	if !substrateMCPToolActorBootRecorded(tool, actorID) {
		return nil
	}
	// A patch of this one annotation, on a copy: the Tool in hand carries
	// status changes that have not been written yet.
	patched := tool.DeepCopy()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, substrateMCPToolBootedIDAnno)
	if err := r.Patch(ctx, patched, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	delete(tool.Annotations, substrateMCPToolBootedIDAnno)
	tool.ResourceVersion = patched.ResourceVersion
	return nil
}

// substrateMCPEgressVerified reports whether the Actor's egress policy was
// installed and verified for the Tool's current specification.
func substrateMCPEgressVerified(tool *corev1alpha1.Tool) bool {
	condition := meta.FindStatusCondition(tool.Status.Conditions, toolConditionEgressPolicyReady)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == tool.Generation
}

// failSubstrateMCPTool reports a reconcile that failed before the Actor's
// egress policy could be applied. If the policy is not verified for the current
// specification, the Actor status names must not keep running under the
// previous one: it is stopped, and the reconcile retries until it is.
func (r *ToolReconciler) failSubstrateMCPTool(ctx context.Context, tool *corev1alpha1.Tool, message string) (ctrl.Result, error) {
	actor := tool.Status.Actor
	hasActor := actor != nil && actor.Provider == corev1alpha1.WorkspaceProviderSubstrate && strings.TrimSpace(actor.ActorID) != ""
	recorded := meta.FindStatusCondition(tool.Status.Conditions, toolConditionEgressPolicyReady) != nil
	if substrateMCPEgressVerified(tool) || (!hasActor && !recorded) {
		return r.updateStatus(ctx, tool, false, message)
	}
	var stopErr error
	if hasActor {
		if stopErr = r.stopSubstrateMCPStatusActor(ctx, tool); stopErr != nil {
			message += "; the actor could not be stopped and may still run under its previous egress policy: " + stopErr.Error()
		}
	}
	meta.SetStatusCondition(&tool.Status.Conditions, metav1.Condition{
		Type: toolConditionEgressPolicyReady, Status: metav1.ConditionFalse, Reason: "EgressPolicyUnverified",
		Message:            "actor egress policy is not verified for this specification: " + message,
		ObservedGeneration: tool.Generation,
	})
	result, updateErr := r.updateStatus(ctx, tool, false, message)
	if updateErr == nil && stopErr != nil {
		// Retry with backoff until the actor is confirmed stopped.
		return ctrl.Result{}, stopErr
	}
	return result, updateErr
}

// stopSubstrateMCPStatusActor stops the Actor that Tool status names.
func (r *ToolReconciler) stopSubstrateMCPStatusActor(ctx context.Context, tool *corev1alpha1.Tool) error {
	actor := tool.Status.Actor
	actorID := strings.TrimSpace(actor.ActorID)
	held := substrateMCPHeldActor{actorID: actorID, leaseNamespace: tool.Namespace}
	if actor.PoolRef != nil {
		held.pooled = true
		if namespace := strings.TrimSpace(actor.PoolRef.Namespace); namespace != "" {
			held.leaseNamespace = namespace
		}
	}
	if actor.TemplateRef == nil || strings.TrimSpace(actor.TemplateRef.Name) == "" {
		return r.denySubstrateMCPActorEgressWithoutTemplate(ctx, tool, held.stop(), fmt.Errorf("tool status does not name the actor's template"))
	}
	return r.stopHeldSubstrateMCPActor(ctx, tool, held,
		strings.TrimSpace(actor.TemplateRef.Name), cmp.Or(strings.TrimSpace(actor.TemplateRef.Namespace), tool.Namespace))
}

// stopUnnamedSubstrateMCPActor stops a held Actor that Tool status does not
// name. Its template is the one Substrate reports for it: the Tool's
// specification may have moved on since it was claimed, and the template
// decides between suspending and deleting it.
func (r *ToolReconciler) stopUnnamedSubstrateMCPActor(ctx context.Context, tool *corev1alpha1.Tool, held substrateMCPHeldActor) error {
	cfg := r.SubstrateConfig.WithDefaults()
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(cfg.WorkspaceClientConfig())
		}
	}
	executor, err := executorFactory(cfg)
	if err != nil {
		return err
	}
	description, err := executor.Describe(ctx, workspace.DescribeRequest{Ref: workspace.WorkspaceRef{ClaimName: held.actorID, ID: held.actorID}})
	closeWorkspaceExecutor(ctx, executor)
	if err != nil {
		if workspace.IsKind(err, workspace.ErrorKindNotFound) {
			// Substrate no longer has it: nothing runs.
			return nil
		}
		return fmt.Errorf("read the actor's template: %w", err)
	}
	if description.Phase == workspace.PhaseDeleted {
		// Describe reports an absent Actor as deleted, with no template.
		return nil
	}
	templateName := strings.TrimSpace(description.Template.Name)
	if templateName == "" {
		return r.denySubstrateMCPActorEgressWithoutTemplate(ctx, tool, held.stop(), fmt.Errorf("substrate does not report the actor's template"))
	}
	return r.stopHeldSubstrateMCPActor(ctx, tool, held, templateName, cmp.Or(strings.TrimSpace(description.Template.Namespace), tool.Namespace))
}

// stopHeldSubstrateMCPActor stops an Actor the Tool holds a lease on, running
// the named template.
func (r *ToolReconciler) stopHeldSubstrateMCPActor(
	ctx context.Context, tool *corev1alpha1.Tool, held substrateMCPHeldActor, templateName, templateNamespace string,
) error {
	stop := held.stop()
	// Whether to suspend or delete depends on the template the Actor runs.
	cfg := r.SubstrateConfig.WithDefaults()
	request := &ExecutionWorkspaceRequest{
		TemplateName:                 templateName,
		TemplateNamespace:            templateNamespace,
		SubstrateBootstrapSecretName: cfg.BootstrapSecretName,
		SubstrateBootstrapSecretKey:  cfg.BootstrapSecretKey,
	}
	var templateErr error
	if err := r.validateSubstrateMCPTemplate(ctx, request); err != nil {
		templateErr = fmt.Errorf("read the actor's template: %w", err)
	}
	if templateErr != nil {
		return r.denySubstrateMCPActorEgressWithoutTemplate(ctx, tool, stop, templateErr)
	}
	stop.suspendUnsupported = request.TemplateSuspendUnsupported
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(cfg.WorkspaceClientConfig())
		}
	}
	executor, err := executorFactory(cfg)
	if err != nil {
		return err
	}
	defer closeWorkspaceExecutor(ctx, executor)
	return r.stopSubstrateMCPActor(ctx, tool, executor, stop, cfg.ClaimTimeout)
}

// denySubstrateMCPActorEgressWithoutTemplate keeps an Actor whose template
// cannot be read from reaching anything. Substrate refuses to suspend an Actor
// without its template, before it touches the workload, so the Actor cannot be
// stopped that way. Replacing its egress policy with one that denies all needs
// no template, but the gateway applies it only once its cached copy of the
// previous rules expires. While it may not, an Actor that is not suspended is
// deleted, as one whose template cannot be suspended is: that discards any data
// on its durable volumes, which only the template declares, but leaves nothing
// running under rules the Tool no longer allows. Once the deny-all is enforced
// the Actor is left as it is, and the error is still returned, so the reconcile
// retries the stop.
func (r *ToolReconciler) denySubstrateMCPActorEgressWithoutTemplate(
	ctx context.Context, tool *corev1alpha1.Tool, stop substrateMCPActorStop, templateErr error,
) error {
	held, err := r.substrateMCPActorHeld(ctx, tool, stop)
	if err != nil {
		return goerrors.Join(templateErr, err)
	}
	if !held {
		return templateErr
	}
	policy, err := r.applySubstrateMCPActorEgressRules(ctx, stop.actorID, nil)
	if err != nil {
		return goerrors.Join(templateErr, fmt.Errorf("deny the actor's egress: %w", err))
	}
	denied := fmt.Errorf("%w; the actor's egress is denied instead", templateErr)
	if r.substrateMCPEgressPropagationWait(policy) <= 0 {
		return denied
	}
	cfg := r.SubstrateConfig.WithDefaults()
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(cfg.WorkspaceClientConfig())
		}
	}
	executor, err := executorFactory(cfg)
	if err != nil {
		return goerrors.Join(denied, err)
	}
	defer closeWorkspaceExecutor(ctx, executor)
	description, err := executor.Describe(ctx, workspace.DescribeRequest{Ref: stop.ref})
	if err != nil {
		return goerrors.Join(denied, fmt.Errorf("read the actor's state: %w", err))
	}
	if description.Phase == workspace.PhaseRetained || description.Phase == workspace.PhaseDeleted {
		// Nothing runs that could use the previous rules.
		return denied
	}
	if err := deleteSubstrateMCPActor(ctx, executor, stop.actorID, substrateMCPActorStopReason, cfg.ClaimTimeout); err != nil {
		return goerrors.Join(denied, fmt.Errorf("delete the actor while the gateway may still apply its previous egress rules: %w", err))
	}
	return r.forgetSubstrateMCPToolActor(ctx, tool, stop.actorID)
}

func shouldBootSubstrateMCPToolActor(tool *corev1alpha1.Tool, actorID string, requested bool, created bool) bool {
	actorID = strings.TrimSpace(actorID)
	if !requested || actorID == "" {
		return false
	}
	if created {
		return true
	}
	if tool == nil {
		return true
	}
	if strings.TrimSpace(tool.Annotations[substrateMCPToolBootedIDAnno]) == actorID {
		return false
	}
	return tool.Status.Actor == nil ||
		tool.Status.Actor.Provider != corev1alpha1.WorkspaceProviderSubstrate ||
		strings.TrimSpace(tool.Status.Actor.ActorID) != actorID
}

func substrateMCPToolActorBootRecorded(tool *corev1alpha1.Tool, actorID string) bool {
	if tool == nil {
		return false
	}
	return strings.TrimSpace(tool.Annotations[substrateMCPToolBootedIDAnno]) == strings.TrimSpace(actorID)
}

func markSubstrateMCPToolActorBooted(tool *corev1alpha1.Tool, actorID string) bool {
	actorID = strings.TrimSpace(actorID)
	if tool == nil || actorID == "" {
		return false
	}
	if tool.Annotations == nil {
		tool.Annotations = map[string]string{}
	}
	if strings.TrimSpace(tool.Annotations[substrateMCPToolBootedIDAnno]) == actorID {
		return false
	}
	tool.Annotations[substrateMCPToolBootedIDAnno] = actorID
	return true
}

func (r *ToolReconciler) recordSubstrateMCPToolActorBooted(ctx context.Context, tool *corev1alpha1.Tool, actorID string) (bool, error) {
	actorID = strings.TrimSpace(actorID)
	if tool == nil || actorID == "" {
		return true, nil
	}
	key := types.NamespacedName{Namespace: tool.Namespace, Name: tool.Name}
	var latestForStatus *corev1alpha1.Tool
	specChanged := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Tool{}
		if getErr := r.Get(ctx, key, latest); getErr != nil {
			if errors.IsNotFound(getErr) {
				return nil
			}
			return getErr
		}
		if latest.Generation != tool.Generation || !reflect.DeepEqual(latest.Spec, tool.Spec) {
			specChanged = true
			return nil
		}
		if !markSubstrateMCPToolActorBooted(latest, actorID) {
			latestForStatus = latest.DeepCopy()
			return nil
		}
		if updateErr := r.Update(ctx, latest); updateErr != nil {
			return updateErr
		}
		latestForStatus = latest.DeepCopy()
		return nil
	})
	if err != nil {
		return false, err
	}
	if specChanged || latestForStatus == nil {
		return false, nil
	}
	latestForStatus.DeepCopyInto(tool)
	return true, nil
}

func (r *ToolReconciler) resolveSubstrateMCPActorPool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	templateRequest *ExecutionWorkspaceRequest,
) (string, string, *corev1alpha1.SubstrateActorPool, error) {
	if tool == nil || tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil {
		return "", "", nil, nil
	}
	poolName, poolNamespace := substrateActorPoolReference(tool.Spec.MCP.SubstrateActor.PoolRef, tool.Namespace)
	if poolName == "" {
		return "", "", nil, nil
	}
	if r.EnforceNamespaceIsolation && poolNamespace != tool.Namespace {
		return "", "", nil, fmt.Errorf(
			"cross-namespace substrate actor poolRef not allowed when namespace isolation is enforced: pool %q in namespace %q, tool in %q",
			poolName,
			poolNamespace,
			tool.Namespace,
		)
	}
	pool, err := resolveSubstrateActorPoolReference(
		ctx,
		r.Client,
		poolNamespace,
		poolName,
		templateRequest.TemplateNamespace,
		templateRequest.TemplateName,
	)
	if err != nil {
		return "", "", nil, err
	}
	if pool.Status.TemplateUID == "" {
		return "", "", nil, fmt.Errorf("substrate actor poolRef %q in namespace %q has not pinned its native ActorTemplate UID", poolName, poolNamespace)
	}
	if pool.Status.TemplateUID != templateRequest.TemplateUID {
		return "", "", nil, fmt.Errorf("substrate actor poolRef %q in namespace %q has a different native ActorTemplate UID; create another pool to use its new identity", poolName, poolNamespace)
	}
	return poolName, poolNamespace, pool, nil
}

func (r *ToolReconciler) finalizeSubstrateMCPTool(ctx context.Context, tool *corev1alpha1.Tool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
		return ctrl.Result{}, nil
	}
	request, pool, err := r.substrateMCPFinalizerMigrationContext(ctx, tool)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed, err := r.migrateSubstrateMCPIdentities(ctx, tool, request, pool); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	cfg := r.SubstrateConfig.WithDefaults()
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(cfg.WorkspaceClientConfig())
		}
	}
	poolRefs := substrateMCPPoolActorLeaseRefs(tool)
	heldPoolRefs, err := r.heldSubstrateMCPPoolActorLeaseRefs(ctx, tool, poolRefs)
	if err != nil {
		return ctrl.Result{}, err
	}
	deleteRefs := r.substrateMCPFinalizerActorDeleteRefs(tool)
	leasedDeleteRefs, err := r.substrateMCPToolActorLeaseDeleteRefs(ctx, tool, "MCP tool deleted")
	if err != nil {
		return ctrl.Result{}, err
	}
	deleteRefs = appendSubstrateMCPActorDeleteRefs(deleteRefs, leasedDeleteRefs...)
	if len(deleteRefs) > 0 || len(heldPoolRefs) > 0 {
		executor, err := executorFactory(cfg)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer closeWorkspaceExecutor(ctx, executor)
		for _, ref := range deleteRefs {
			if err := deleteSubstrateMCPActor(ctx, executor, ref.actorID, ref.reason, cfg.ClaimTimeout); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.deleteSubstrateMCPToolActorLease(ctx, tool, tool.Namespace, ref.actorID); err != nil {
				return ctrl.Result{}, err
			}
		}
		for _, ref := range heldPoolRefs {
			if err := r.cleanupSubstrateMCPPoolActor(ctx, executor, tool, ref.poolNamespace, ref.actorID, "MCP pooled tool actor deleted", cfg.ClaimTimeout); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	controllerutil.RemoveFinalizer(tool, substrateMCPToolActorFinalizer)
	removeSubstrateMCPToolActorOwnership(tool)
	if err := r.Update(ctx, tool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ToolReconciler) cleanupPendingSubstrateMCPActor(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	tool *corev1alpha1.Tool,
	trustedRef *substrateMCPActorCleanupRef,
	timeout time.Duration,
) error {
	ref := trustedRef
	if ref == nil {
		ref = pendingSubstrateMCPActorCleanupRef(tool)
		if ref == nil {
			return nil
		}
		if ref.poolName == substrateMCPToolCleanupNonPooledValueAnno {
			_, held, err := r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, tool.Namespace, ref.actorID)
			if err != nil {
				return err
			}
			if !held && clearSubstrateMCPToolPendingCleanup(tool) {
				if err := r.Update(ctx, tool); err != nil && !errors.IsNotFound(err) {
					return err
				}
				return nil
			}
		}
	}
	if ref.poolName == substrateMCPToolCleanupNonPooledValueAnno {
		if executor == nil {
			return fmt.Errorf("workspace executor is required to delete pending MCP actor %q", ref.actorID)
		}
		if err := deleteSubstrateMCPActor(ctx, executor, ref.actorID, "MCP tool actor replaced", timeout); err != nil {
			return err
		}
		if err := r.deleteSubstrateMCPToolActorLease(ctx, tool, tool.Namespace, ref.actorID); err != nil {
			return err
		}
	} else {
		if err := r.cleanupSubstrateMCPPoolActor(ctx, executor, tool, ref.poolNamespace, ref.actorID, "MCP pooled tool actor replaced", timeout); err != nil {
			return err
		}
	}
	if clearSubstrateMCPToolPendingCleanup(tool) {
		if err := r.Update(ctx, tool); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (r *ToolReconciler) cleanupSubstrateMCPPoolActor(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
	reason string,
	timeout time.Duration,
) error {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return nil
	}
	if executor == nil {
		return fmt.Errorf("workspace executor is required to delete pooled MCP actor %q", actorID)
	}
	lease, held, err := r.substrateMCPPoolActorLeaseHeldByTool(ctx, tool, leaseNamespace, actorID)
	if err != nil {
		return err
	}
	if lease == nil {
		return nil
	}
	if !held {
		return nil
	}
	if err := deleteSubstrateMCPActor(ctx, executor, actorID, reason, timeout); err != nil {
		return err
	}
	if err := r.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !errors.IsNotFound(err) {
		if errors.IsConflict(err) {
			stillHeld, verifyErr := substrateLeaseStillMatchesAfterDeleteConflict(ctx, r.Client, lease, func(latest *coordinationv1.Lease) bool {
				return substratePoolActorLeaseHeldByTool(latest, tool)
			})
			if verifyErr != nil {
				return verifyErr
			}
			if !stillHeld {
				return nil
			}
		}
		return err
	}
	return nil
}

func (r *ToolReconciler) substrateMCPPoolActorLeaseHeldByTool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) (*coordinationv1.Lease, bool, error) {
	actorID = strings.TrimSpace(actorID)
	leaseNamespace = strings.TrimSpace(leaseNamespace)
	if actorID == "" || leaseNamespace == "" {
		return nil, false, nil
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: leaseNamespace, Name: substratePoolActorLeaseName(actorID)}
	if err := r.Get(ctx, key, lease); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !substratePoolActorLeaseHeldByTool(lease, tool) {
		return lease, false, nil
	}
	return lease, true, nil
}

func (r *ToolReconciler) ensureSubstrateMCPToolActorLease(ctx context.Context, tool *corev1alpha1.Tool, actorID string) error {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return nil
	}
	lease, held, err := r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, tool.Namespace, actorID)
	if err != nil {
		return err
	}
	if lease == nil {
		lease = newSubstrateMCPToolActorLease(tool, tool.Namespace, actorID)
		if err := r.Create(ctx, lease); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}
			return err
		}
		return nil
	}
	if !held {
		return fmt.Errorf("substrate MCP tool actor lease %q in namespace %q is held by another owner", lease.Name, lease.Namespace)
	}
	patch := client.MergeFromWithOptions(lease.DeepCopy(), client.MergeFromWithOptimisticLock{})
	setSubstrateMCPToolActorLeaseHolder(lease, tool, actorID)
	if err := r.Patch(ctx, lease, patch); err != nil {
		if errors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

func (r *ToolReconciler) substrateMCPToolActorLeaseReplacementCleanupRef(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	desiredActorID string,
) (*substrateMCPActorCleanupRef, error) {
	if tool == nil || tool.Annotations == nil {
		return nil, nil
	}
	oldActorID := strings.TrimSpace(tool.Annotations[substrateMCPToolActorIDAnno])
	desiredActorID = strings.TrimSpace(desiredActorID)
	if oldActorID == "" || oldActorID == desiredActorID {
		return nil, nil
	}
	_, held, err := r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, tool.Namespace, oldActorID)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	return &substrateMCPActorCleanupRef{
		actorID:  oldActorID,
		poolName: substrateMCPToolCleanupNonPooledValueAnno,
	}, nil
}

func (r *ToolReconciler) substrateMCPToolActorLeaseDeleteRefs(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	reason string,
) ([]substrateMCPActorDeleteRef, error) {
	if tool == nil {
		return nil, nil
	}
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.InNamespace(tool.Namespace), client.MatchingLabels{
		labels.LabelPurpose: substrateMCPToolActorLeasePurpose,
	}); err != nil {
		return nil, err
	}
	refs := make([]substrateMCPActorDeleteRef, 0, len(leases.Items))
	for i := range leases.Items {
		lease := &leases.Items[i]
		if !substratePoolActorLeaseHeldByTool(lease, tool) {
			continue
		}
		actorID := substratePoolActorLeaseActorID(lease)
		if actorID == "" {
			continue
		}
		refs = append(refs, substrateMCPActorDeleteRef{actorID: actorID, reason: reason})
	}
	return refs, nil
}

func (r *ToolReconciler) substrateMCPToolActorLeaseHeldByTool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) (*coordinationv1.Lease, bool, error) {
	actorID = strings.TrimSpace(actorID)
	leaseNamespace = strings.TrimSpace(leaseNamespace)
	if actorID == "" || leaseNamespace == "" {
		return nil, false, nil
	}
	var selected *coordinationv1.Lease
	names := []string{substratePoolActorLeaseName(actorID)}
	if names[0] != actorID {
		// Early native controllers wrote the qualified ID as the lease name.
		// Read both forms without creating a second ownership claim.
		names = append(names, actorID)
	}
	for _, name := range names {
		lease := &coordinationv1.Lease{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: leaseNamespace, Name: name}, lease); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, false, err
		}
		if lease.Labels[labels.LabelPurpose] != substrateMCPToolActorLeasePurpose || !substratePoolActorLeaseHeldByTool(lease, tool) {
			return lease, false, nil
		}
		if selected == nil {
			selected = lease
		}
	}
	return selected, selected != nil, nil
}

func (r *ToolReconciler) deleteSubstrateMCPToolActorLease(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) error {
	// A partially upgraded controller may have left both lease-name forms.
	// Remove each only while both remain attributable to this exact Tool.
	for range 2 {
		lease, held, err := r.substrateMCPToolActorLeaseHeldByTool(ctx, tool, leaseNamespace, actorID)
		if err != nil {
			return err
		}
		if lease == nil || !held {
			return nil
		}
		if err := r.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !errors.IsNotFound(err) {
			if errors.IsConflict(err) {
				stillHeld, verifyErr := substrateLeaseStillMatchesAfterDeleteConflict(ctx, r.Client, lease, func(latest *coordinationv1.Lease) bool {
					return substratePoolActorLeaseHeldByTool(latest, tool)
				})
				if verifyErr != nil {
					return verifyErr
				}
				if !stillHeld {
					return nil
				}
			}
			return err
		}
	}
	return nil
}

type substrateMCPActorCleanupRef struct {
	actorID       string
	poolName      string
	poolNamespace string
}

func (r *ToolReconciler) substrateMCPReplacementCleanupRef(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	desiredActorID string,
) (*substrateMCPActorCleanupRef, error) {
	if ref := substrateMCPStatusReplacementCleanupRef(tool, desiredActorID); ref != nil {
		return ref, nil
	}
	if ref, err := r.substrateMCPToolActorLeaseReplacementCleanupRef(ctx, tool, desiredActorID); err != nil {
		return nil, err
	} else if ref != nil {
		return ref, nil
	}
	return r.substrateMCPLeasedAnnotationReplacementCleanupRef(ctx, tool, desiredActorID)
}

func substrateMCPStatusReplacementCleanupRef(tool *corev1alpha1.Tool, desiredActorID string) *substrateMCPActorCleanupRef {
	if tool == nil || tool.Status.Actor == nil {
		return nil
	}
	actor := tool.Status.Actor
	oldActorID := strings.TrimSpace(actor.ActorID)
	desiredActorID = strings.TrimSpace(desiredActorID)
	if actor.Provider != corev1alpha1.WorkspaceProviderSubstrate ||
		oldActorID == "" ||
		oldActorID == desiredActorID {
		return nil
	}
	if actor.PoolRef == nil {
		return &substrateMCPActorCleanupRef{
			actorID:  oldActorID,
			poolName: substrateMCPToolCleanupNonPooledValueAnno,
		}
	}
	poolName := strings.TrimSpace(actor.PoolRef.Name)
	poolNamespace := strings.TrimSpace(actor.PoolRef.Namespace)
	if poolNamespace == "" {
		poolNamespace = tool.Namespace
	}
	return &substrateMCPActorCleanupRef{
		actorID:       oldActorID,
		poolName:      poolName,
		poolNamespace: poolNamespace,
	}
}

func (r *ToolReconciler) substrateMCPLeasedAnnotationReplacementCleanupRef(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	desiredActorID string,
) (*substrateMCPActorCleanupRef, error) {
	if tool == nil || tool.Annotations == nil {
		return nil, nil
	}
	oldActorID := strings.TrimSpace(tool.Annotations[substrateMCPToolActorIDAnno])
	desiredActorID = strings.TrimSpace(desiredActorID)
	if oldActorID == "" || oldActorID == desiredActorID {
		return nil, nil
	}
	poolName := strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNameAnno])
	if poolName == "" {
		return nil, nil
	}
	poolNamespace := strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNamespaceAnno])
	if poolNamespace == "" {
		poolNamespace = tool.Namespace
	}
	_, held, err := r.substrateMCPPoolActorLeaseHeldByTool(ctx, tool, poolNamespace, oldActorID)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	return &substrateMCPActorCleanupRef{
		actorID:       oldActorID,
		poolName:      poolName,
		poolNamespace: poolNamespace,
	}, nil
}

func (r *ToolReconciler) tryReserveSubstrateMCPPoolActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	actorID string,
) (bool, error) {
	leaseName := substratePoolActorLeaseName(actorID)
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: leaseNamespace, Name: leaseName}
	err := r.Get(ctx, key, lease)
	if errors.IsNotFound(err) {
		lease = newSubstrateMCPPoolActorLease(tool, leaseNamespace, leaseName, actorID)
		if err := r.Create(ctx, lease); err != nil {
			if errors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if substratePoolActorLeaseHeldByTool(lease, tool) {
		return true, nil
	}
	busy, err := substratePoolActorLeaseHasActiveHolder(ctx, r.Client, lease)
	if err != nil || busy {
		return false, err
	}
	patch := client.MergeFromWithOptions(lease.DeepCopy(), client.MergeFromWithOptimisticLock{})
	setSubstrateMCPPoolActorLeaseHolder(lease, tool, actorID)
	if err := r.Patch(ctx, lease, patch); err != nil {
		if errors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *ToolReconciler) reserveSubstrateMCPPoolActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	startActorID string,
	target int,
) (string, bool, error) {
	if target <= 0 {
		return "", false, fmt.Errorf("substrate actor pool in namespace %q has no target actors", leaseNamespace)
	}
	prefix, startOrdinal, ok := substratePoolActorPrefixAndOrdinal(startActorID)
	if !ok {
		return "", false, fmt.Errorf("substrate pool actor id %q is not deterministic", startActorID)
	}
	if actorID, found, err := r.substrateMCPPoolActorLeaseForTool(ctx, tool, leaseNamespace, prefix, target); err != nil {
		return "", false, err
	} else if found {
		return actorID, true, nil
	}
	for offset := range target {
		ordinal := (startOrdinal + offset) % target
		_, atespace, _ := strings.Cut(startActorID, ".")
		actorID := workspace.SubstrateActorKey(atespace, deterministicSubstratePoolActorID(prefix, ordinal))
		reserved, err := r.tryReserveSubstrateMCPPoolActor(ctx, tool, leaseNamespace, actorID)
		if err != nil {
			return "", false, err
		}
		if reserved {
			return actorID, true, nil
		}
	}
	return "", false, nil
}

func (r *ToolReconciler) substrateMCPPoolActorLeaseForTool(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	leaseNamespace string,
	prefix string,
	target int,
) (string, bool, error) {
	if tool == nil {
		return "", false, nil
	}
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.InNamespace(leaseNamespace), client.MatchingLabels{
		labels.LabelPurpose: substratePoolActorLeasePurpose,
	}); err != nil {
		return "", false, err
	}
	for i := range leases.Items {
		lease := &leases.Items[i]
		if !substratePoolActorLeaseHeldByTool(lease, tool) {
			continue
		}
		actorID := substratePoolActorLeaseActorID(lease)
		if ordinal, ok := substratePoolActorOrdinalFromID(actorID, prefix); ok && ordinal < target {
			return actorID, true, nil
		}
	}
	return "", false, nil
}

// heldSubstrateMCPPoolActorLeaseRefs returns the refs whose pool lease the
// Tool holds. A lease that cannot be read does not stop the others from being
// checked: it returns every ref proven held together with the joined lookup
// errors.
func (r *ToolReconciler) heldSubstrateMCPPoolActorLeaseRefs(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	refs []substrateMCPPoolActorLeaseRef,
) ([]substrateMCPPoolActorLeaseRef, error) {
	heldRefs := make([]substrateMCPPoolActorLeaseRef, 0, len(refs))
	var errs []error
	for _, ref := range refs {
		_, held, err := r.substrateMCPPoolActorLeaseHeldByTool(ctx, tool, ref.poolNamespace, ref.actorID)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the pool lease of actor %q: %w", ref.actorID, err))
			continue
		}
		if held {
			heldRefs = append(heldRefs, ref)
		}
	}
	return heldRefs, goerrors.Join(errs...)
}

func substrateMCPPoolActorLeaseRefs(tool *corev1alpha1.Tool) []substrateMCPPoolActorLeaseRef {
	if tool == nil {
		return nil
	}
	refs := make([]substrateMCPPoolActorLeaseRef, 0, 2)
	seen := map[substrateMCPPoolActorLeaseRef]struct{}{}
	add := func(namespace, actorID string) {
		ref := substrateMCPPoolActorLeaseRef{
			poolNamespace: strings.TrimSpace(namespace),
			actorID:       strings.TrimSpace(actorID),
		}
		if ref.poolNamespace == "" || ref.actorID == "" {
			return
		}
		if _, ok := seen[ref]; ok {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	if tool.Status.Actor != nil && tool.Status.Actor.PoolRef != nil {
		namespace := strings.TrimSpace(tool.Status.Actor.PoolRef.Namespace)
		if namespace == "" {
			namespace = tool.Namespace
		}
		add(namespace, tool.Status.Actor.ActorID)
	}
	if tool.Annotations != nil {
		add(tool.Annotations[substrateMCPToolActorPoolNamespaceAnno], tool.Annotations[substrateMCPToolActorIDAnno])
		if tool.Annotations[substrateMCPToolCleanupPoolNameAnno] != substrateMCPToolCleanupNonPooledValueAnno {
			add(tool.Annotations[substrateMCPToolCleanupPoolNamespaceAnno], tool.Annotations[substrateMCPToolCleanupActorIDAnno])
		}
	}
	if tool.Spec.MCP != nil && tool.Spec.MCP.SubstrateActor != nil {
		poolName, poolNamespace := substrateActorPoolReference(tool.Spec.MCP.SubstrateActor.PoolRef, tool.Namespace)
		if poolName != "" {
			add(poolNamespace, tool.Annotations[substrateMCPToolActorIDAnno])
		}
	}
	return refs
}

func assignedSubstrateMCPPoolActorID(tool *corev1alpha1.Tool, poolName string, poolNamespace string, prefix string, target int) string {
	if tool == nil {
		return ""
	}
	if tool.Annotations != nil &&
		strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNameAnno]) == strings.TrimSpace(poolName) &&
		strings.TrimSpace(tool.Annotations[substrateMCPToolActorPoolNamespaceAnno]) == strings.TrimSpace(poolNamespace) {
		actorID := strings.TrimSpace(tool.Annotations[substrateMCPToolActorIDAnno])
		if ordinal, ok := substratePoolActorOrdinalFromID(actorID, prefix); ok && ordinal < target {
			return actorID
		}
	}
	if tool.Status.Actor != nil &&
		tool.Status.Actor.Provider == corev1alpha1.WorkspaceProviderSubstrate &&
		tool.Status.Actor.PoolRef != nil {
		name, namespace := substrateActorPoolReference(tool.Status.Actor.PoolRef, tool.Namespace)
		if name == strings.TrimSpace(poolName) && namespace == strings.TrimSpace(poolNamespace) {
			actorID := strings.TrimSpace(tool.Status.Actor.ActorID)
			if ordinal, ok := substratePoolActorOrdinalFromID(actorID, prefix); ok && ordinal < target {
				return actorID
			}
		}
	}
	return ""
}

type substrateMCPPoolActorLeaseRef struct {
	poolNamespace string
	actorID       string
}

func pendingSubstrateMCPActorCleanupRef(tool *corev1alpha1.Tool) *substrateMCPActorCleanupRef {
	if tool == nil || tool.Annotations == nil {
		return nil
	}
	actorID := strings.TrimSpace(tool.Annotations[substrateMCPToolCleanupActorIDAnno])
	if actorID == "" {
		return nil
	}
	poolName := strings.TrimSpace(tool.Annotations[substrateMCPToolCleanupPoolNameAnno])
	if poolName == "" {
		poolName = substrateMCPToolCleanupNonPooledValueAnno
	}
	poolNamespace := strings.TrimSpace(tool.Annotations[substrateMCPToolCleanupPoolNamespaceAnno])
	if poolName != substrateMCPToolCleanupNonPooledValueAnno && poolNamespace == "" {
		poolNamespace = tool.Namespace
	}
	return &substrateMCPActorCleanupRef{
		actorID:       actorID,
		poolName:      poolName,
		poolNamespace: poolNamespace,
	}
}

func ensureSubstrateMCPToolPendingCleanup(tool *corev1alpha1.Tool, ref *substrateMCPActorCleanupRef) bool {
	if tool == nil || ref == nil || strings.TrimSpace(ref.actorID) == "" {
		return false
	}
	if tool.Annotations == nil {
		tool.Annotations = map[string]string{}
	}
	changed := false
	set := func(key, value string) {
		value = strings.TrimSpace(value)
		if tool.Annotations[key] != value {
			tool.Annotations[key] = value
			changed = true
		}
	}
	poolName := strings.TrimSpace(ref.poolName)
	if poolName == "" {
		poolName = substrateMCPToolCleanupNonPooledValueAnno
	}
	set(substrateMCPToolCleanupActorIDAnno, ref.actorID)
	set(substrateMCPToolCleanupPoolNameAnno, poolName)
	if poolName == substrateMCPToolCleanupNonPooledValueAnno {
		if _, ok := tool.Annotations[substrateMCPToolCleanupPoolNamespaceAnno]; ok {
			delete(tool.Annotations, substrateMCPToolCleanupPoolNamespaceAnno)
			changed = true
		}
		return changed
	}
	set(substrateMCPToolCleanupPoolNamespaceAnno, ref.poolNamespace)
	return changed
}

func ensureSubstrateMCPToolCleanupState(
	tool *corev1alpha1.Tool,
	desiredActorID string,
	ref *substrateMCPActorCleanupRef,
) bool {
	desiredActorID = strings.TrimSpace(desiredActorID)
	if ref != nil {
		if strings.TrimSpace(ref.actorID) == "" || strings.TrimSpace(ref.actorID) == desiredActorID {
			return clearSubstrateMCPToolPendingCleanup(tool)
		}
		return ensureSubstrateMCPToolPendingCleanup(tool, ref)
	}
	pending := pendingSubstrateMCPActorCleanupRef(tool)
	if pending != nil && strings.TrimSpace(pending.actorID) == desiredActorID {
		return clearSubstrateMCPToolPendingCleanup(tool)
	}
	return false
}

func clearSubstrateMCPToolPendingCleanup(tool *corev1alpha1.Tool) bool {
	if tool == nil || tool.Annotations == nil {
		return false
	}
	changed := false
	for _, key := range []string{
		substrateMCPToolCleanupActorIDAnno,
		substrateMCPToolCleanupPoolNameAnno,
		substrateMCPToolCleanupPoolNamespaceAnno,
	} {
		if _, ok := tool.Annotations[key]; ok {
			delete(tool.Annotations, key)
			changed = true
		}
	}
	if len(tool.Annotations) == 0 {
		tool.Annotations = nil
	}
	return changed
}

type substrateMCPActorDeleteRef struct {
	actorID string
	reason  string
}

func appendSubstrateMCPActorDeleteRefs(
	refs []substrateMCPActorDeleteRef,
	extraRefs ...substrateMCPActorDeleteRef,
) []substrateMCPActorDeleteRef {
	if len(extraRefs) == 0 {
		return refs
	}
	seen := make(map[string]struct{}, len(refs)+len(extraRefs))
	for _, ref := range refs {
		actorID := strings.TrimSpace(ref.actorID)
		if actorID == "" {
			continue
		}
		seen[actorID] = struct{}{}
	}
	for _, ref := range extraRefs {
		ref.actorID = strings.TrimSpace(ref.actorID)
		if ref.actorID == "" {
			continue
		}
		if _, ok := seen[ref.actorID]; ok {
			continue
		}
		seen[ref.actorID] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func (r *ToolReconciler) substrateMCPFinalizerActorDeleteRefs(tool *corev1alpha1.Tool) []substrateMCPActorDeleteRef {
	if tool == nil {
		return nil
	}
	refs := make([]substrateMCPActorDeleteRef, 0, 3)
	seen := map[string]struct{}{}
	add := func(actorID, reason string) {
		actorID = strings.TrimSpace(actorID)
		if actorID == "" {
			return
		}
		if _, ok := seen[actorID]; ok {
			return
		}
		seen[actorID] = struct{}{}
		refs = append(refs, substrateMCPActorDeleteRef{actorID: actorID, reason: reason})
	}
	if tool.Status.Actor != nil &&
		tool.Status.Actor.Provider == corev1alpha1.WorkspaceProviderSubstrate &&
		tool.Status.Actor.PoolRef == nil {
		add(tool.Status.Actor.ActorID, "MCP tool deleted")
	}
	if tool.Spec.MCP != nil &&
		tool.Spec.MCP.SubstrateActor != nil &&
		tool.Spec.MCP.SubstrateActor.PoolRef == nil {
		templateRequest := r.substrateMCPTemplateRequest(tool)
		if strings.TrimSpace(templateRequest.TemplateName) != "" {
			add(
				deterministicSubstrateToolActorID(tool.Namespace, tool.Name, templateRequest.TemplateNamespace, templateRequest.TemplateName),
				"MCP tool deleted",
			)
		}
	}
	return refs
}

func ensureSubstrateMCPToolActorOwnership(tool *corev1alpha1.Tool, actorID string, poolRef *corev1alpha1.SubstrateActorPoolReference) bool {
	changed := false
	if !controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
		controllerutil.AddFinalizer(tool, substrateMCPToolActorFinalizer)
		changed = true
	}
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return changed
	}
	if tool.Annotations == nil {
		tool.Annotations = map[string]string{}
	}
	if tool.Annotations[substrateMCPToolActorIDAnno] != actorID {
		tool.Annotations[substrateMCPToolActorIDAnno] = actorID
		changed = true
	}
	if poolRef == nil {
		if _, ok := tool.Annotations[substrateMCPToolActorPoolNameAnno]; ok {
			delete(tool.Annotations, substrateMCPToolActorPoolNameAnno)
			changed = true
		}
		if _, ok := tool.Annotations[substrateMCPToolActorPoolNamespaceAnno]; ok {
			delete(tool.Annotations, substrateMCPToolActorPoolNamespaceAnno)
			changed = true
		}
		return changed
	}
	if tool.Annotations[substrateMCPToolActorPoolNameAnno] != strings.TrimSpace(poolRef.Name) {
		tool.Annotations[substrateMCPToolActorPoolNameAnno] = strings.TrimSpace(poolRef.Name)
		changed = true
	}
	if tool.Annotations[substrateMCPToolActorPoolNamespaceAnno] != strings.TrimSpace(poolRef.Namespace) {
		tool.Annotations[substrateMCPToolActorPoolNamespaceAnno] = strings.TrimSpace(poolRef.Namespace)
		changed = true
	}
	return changed
}

func removeSubstrateMCPToolActorOwnership(tool *corev1alpha1.Tool) {
	if tool == nil || tool.Annotations == nil {
		return
	}
	delete(tool.Annotations, substrateMCPToolActorIDAnno)
	delete(tool.Annotations, substrateMCPToolBootedIDAnno)
	delete(tool.Annotations, substrateMCPToolActorPoolNameAnno)
	delete(tool.Annotations, substrateMCPToolActorPoolNamespaceAnno)
	delete(tool.Annotations, substrateMCPToolCleanupActorIDAnno)
	delete(tool.Annotations, substrateMCPToolCleanupPoolNameAnno)
	delete(tool.Annotations, substrateMCPToolCleanupPoolNamespaceAnno)
	if len(tool.Annotations) == 0 {
		tool.Annotations = nil
	}
}

func deleteSubstrateMCPActor(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	actorID string,
	reason string,
	timeout time.Duration,
) error {
	actorID = strings.TrimSpace(actorID)
	_, err := executor.Delete(ctx, workspace.DeleteRequest{
		Ref:       workspace.WorkspaceRef{ClaimName: actorID, ID: actorID},
		Reason:    reason,
		Timeout:   timeout,
		SkipScrub: true,
	})
	return err
}

func closeWorkspaceExecutor(ctx context.Context, executor workspace.WorkspaceExecutor) {
	closer, ok := executor.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil {
		log.FromContext(ctx).Error(err, "failed to close workspace executor")
	}
}

// healthCheck performs an HTTP health check against the tool endpoint.
func (r *ToolReconciler) healthCheck(ctx context.Context, tool *corev1alpha1.Tool) error {
	httpClient := r.getHTTPClient()
	if tool == nil || tool.Spec.HTTP == nil {
		return fmt.Errorf("http is required unless mcp.substrateActor is set")
	}
	if ref := tool.Spec.HTTP.OutboundAccessPolicyRef; ref != nil {
		policy := &corev1alpha1.OutboundAccessPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: tool.Namespace, Name: ref.Name}, policy); err != nil {
			return fmt.Errorf("failed to resolve outbound access policy for health check: %w", err)
		}
		if policy.Spec.Gateway != nil {
			return nil
		}
		// Under the fixture allowance a connection-mode or agentIdentity
		// Tool skipped the DNS-based private-address rule, so its probe
		// dials through the hardened private-endpoint dialer (infrastructure
		// addresses refused, no proxy, no redirect hops) rather than the
		// plain one.
		if r.AllowPrivateConnectorEndpoints && (policy.Spec.Connection != nil || policy.Spec.AgentIdentity != nil) {
			httpClient = privateEndpointHealthClient(httpClient.Timeout)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, tool.Spec.HTTP.URL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("endpoint unreachable")
	}
	defer resp.Body.Close() //nolint:errcheck

	// Any response (even 4xx/5xx) means the endpoint is reachable.
	// We only mark unavailable if the connection itself fails.
	return nil
}

func safeToolURLIdentity(tool *corev1alpha1.Tool) string {
	if tool == nil || tool.Spec.HTTP == nil {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(tool.Spec.HTTP.URL))
	if err != nil || parsed.Host == "" {
		return "invalid"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func sensitiveURLParameter(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	if slices.Contains([]string{
		"awsaccesskeyid", "googleaccessid", "key-pair-id", "sig", "signature",
		"x-amz-credential", "x-amz-security-token", "x-amz-signature",
		"x-goog-credential", "x-goog-signature", "x-ms-signature",
	}, normalized) {
		return true
	}
	if strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "credential") ||
		strings.Contains(normalized, "authorization") ||
		strings.Contains(normalized, "api-key") ||
		strings.Contains(normalized, "apikey") {
		return true
	}
	if normalized == defaultACPWorkspaceCredentialKey {
		return true
	}
	if !strings.HasSuffix(normalized, "-token") {
		return false
	}
	prefix := strings.TrimSuffix(normalized, "-token")
	return slices.Contains([]string{
		"access", "api", "auth", "bearer", "github", "gitlab", "id", "oauth",
		"personal-access", "private", "refresh", "security", "session", "slack",
		"transaction", "txn",
	}, prefix)
}

func (r *ToolReconciler) healthCheckMCPActorEndpoint(ctx context.Context, router *workerexecutor.SubstrateMCPRouter, tool *corev1alpha1.Tool) error {
	req, err := router.NewRequest(ctx, tool, http.MethodHead, nil)
	if err != nil {
		return fmt.Errorf("failed to create MCP endpoint check request: %w", err)
	}
	httpClient := *router.HTTPClient()
	if httpClient.Timeout <= 0 {
		httpClient.Timeout = toolHealthCheckTimeout
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("MCP endpoint unreachable through the Substrate router")
	}
	defer resp.Body.Close() //nolint:errcheck

	// An Actor protected by spec.http.authSecretRef answers this unauthenticated
	// probe with 401/403 and is still reachable. The router may reject an
	// unlisted client either during TLS or with a 403, so a 403 alone cannot
	// distinguish the two; a router identity problem surfaces on Tool calls.
	// Redirects are never followed for Actor traffic, so an Actor that
	// answers its MCP path with one cannot serve MCP through the proxy.
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("MCP endpoint returned HTTP %d", resp.StatusCode)
	case resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest:
		return fmt.Errorf("MCP endpoint returned HTTP %d; redirects are not followed", resp.StatusCode)
	case resp.StatusCode >= http.StatusInternalServerError:
		return fmt.Errorf("MCP endpoint returned HTTP %d", resp.StatusCode)
	default:
		return nil
	}
}

// waitForMCPActorEndpoint polls the actor's MCP path through the router with
// the controller's mTLS identity and the ate-target-actor header.
func (r *ToolReconciler) waitForMCPActorEndpoint(
	ctx context.Context, cfg SubstrateConfig, tool *corev1alpha1.Tool, actorID string, timeout time.Duration,
) error {
	routerClient := r.SubstrateRouterHTTPClient
	if routerClient == nil {
		// A fresh client reloads the rotated pod certificate; release its
		// connections once readiness is decided.
		built, err := workspace.NewSubstrateRouterHTTPClient(cfg.WorkspaceClientConfig())
		if err != nil {
			return err
		}
		defer built.CloseIdleConnections()
		routerClient = built
	}
	router, err := workerexecutor.NewSubstrateMCPRouter(cfg.RouterURL, routerClient)
	if err != nil {
		return err
	}
	probe := tool.DeepCopy()
	probe.Status.Actor = &corev1alpha1.ToolActorStatus{Provider: corev1alpha1.WorkspaceProviderSubstrate, ActorID: actorID}
	lastErr := r.healthCheckMCPActorEndpoint(ctx, router, probe)
	if lastErr == nil || timeout <= 0 {
		return lastErr
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	interval := min(timeout, mcpEndpointReadinessPollInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("MCP endpoint readiness canceled: %w", ctx.Err())
		case <-timer.C:
			return lastErr
		case <-ticker.C:
			lastErr = r.healthCheckMCPActorEndpoint(ctx, router, probe)
			if lastErr == nil {
				return nil
			}
		}
	}
}

func (r *ToolReconciler) applySubstrateMCPActorEgress(ctx context.Context, tool *corev1alpha1.Tool, actorID string) (workspace.SubstrateEgressPolicyState, error) {
	rules := substrateMCPEgressRules(tool, r.SubstrateConfig.EgressCredentialProvider)
	if err := workspace.ValidateSubstrateEgressRules(rules); err != nil {
		return workspace.SubstrateEgressPolicyState{}, err
	}
	return r.applySubstrateMCPActorEgressRules(ctx, actorID, rules)
}

// applySubstrateMCPActorEgressRules installs and verifies an Actor's egress
// policy. No rules deny all egress.
func (r *ToolReconciler) applySubstrateMCPActorEgressRules(
	ctx context.Context, actorID string, rules []workspace.SubstrateEgressRule,
) (workspace.SubstrateEgressPolicyState, error) {
	cfg := r.SubstrateConfig.WithDefaults()
	switch {
	case r.SubstrateEgressPolicyStateApplier != nil:
		return r.SubstrateEgressPolicyStateApplier(ctx, cfg, actorID, rules)
	case r.SubstrateEgressPolicyApplier != nil:
		return workspace.SubstrateEgressPolicyState{}, r.SubstrateEgressPolicyApplier(ctx, cfg, actorID, rules)
	}
	return applyNativeSubstrateActorEgressPolicy(ctx, cfg, actorID, rules)
}

// failSubstrateMCPToolValidation reports an invalid Substrate MCP Tool and
// first denies all egress to every actor it still holds. Otherwise an actor
// that was running before the Tool became invalid (for example a cleared
// egress credential provider or a credential Secret that no longer resolves)
// would keep egressing, and keep using a gateway-injected credential, under
// its last installed policy. The actor is then stopped, because the gateway
// may apply the previous policy from its cache for a while. A failed
// revocation or stop is reported and retried. Reconcile reinstalls the Tool's
// policy before the actor serves again.
func (r *ToolReconciler) failSubstrateMCPToolValidation(ctx context.Context, tool *corev1alpha1.Tool, validationErr error) (ctrl.Result, error) {
	message := validationErr.Error()
	held, revokeErr := r.revokeSubstrateMCPToolActorEgress(ctx, tool)
	if revokeErr == nil && len(held) == 0 {
		// This Tool holds no actor lease to revoke. An actor its status still
		// names is handled as on any other reconcile that fails before the
		// policy is verified.
		return r.failSubstrateMCPTool(ctx, tool, message)
	}
	if revokeErr != nil {
		message += "; actor egress was not revoked: " + revokeErr.Error()
		meta.SetStatusCondition(&tool.Status.Conditions, metav1.Condition{
			Type: toolConditionEgressPolicyReady, Status: metav1.ConditionFalse, Reason: "EgressRevocationFailed",
			Message: "actor egress was not revoked: " + revokeErr.Error(), ObservedGeneration: tool.Generation,
		})
	} else {
		meta.SetStatusCondition(&tool.Status.Conditions, metav1.Condition{
			Type: toolConditionEgressPolicyReady, Status: metav1.ConditionFalse, Reason: "EgressRevoked",
			Message: "Tool is invalid; actor egress is denied", ObservedGeneration: tool.Generation,
		})
	}
	// Every actor is stopped as well. A deny-all policy takes effect only once
	// the egress gateway drops its cached copy of the previous one, and may not
	// have been installed at all, so until then an actor could keep using its
	// last rules and any credential the gateway injects for them. That includes
	// a replacement the Tool holds while its status still names the actor it
	// replaces.
	var stopErrs []error
	statusActorID := ""
	if actor := tool.Status.Actor; actor != nil && actor.Provider == corev1alpha1.WorkspaceProviderSubstrate && strings.TrimSpace(actor.ActorID) != "" {
		statusActorID = strings.TrimSpace(actor.ActorID)
		if err := r.stopSubstrateMCPStatusActor(ctx, tool); err != nil {
			stopErrs = append(stopErrs, fmt.Errorf("actor %q: %w", statusActorID, err))
		}
	}
	for _, actor := range held {
		if actor.actorID == statusActorID {
			continue
		}
		if err := r.stopUnnamedSubstrateMCPActor(ctx, tool, actor); err != nil {
			stopErrs = append(stopErrs, fmt.Errorf("actor %q: %w", actor.actorID, err))
		}
	}
	stopErr := goerrors.Join(stopErrs...)
	if stopErr != nil {
		message += "; the actor could not be stopped: " + stopErr.Error()
	}
	result, updateErr := r.updateStatus(ctx, tool, false, message)
	// Retry with backoff until egress is revoked and every actor is stopped.
	if err := goerrors.Join(revokeErr, stopErr); err != nil {
		return ctrl.Result{}, err
	}
	return result, updateErr
}

// revokeSubstrateMCPToolActorEgress installs a deny-all egress policy on every
// actor whose Tool or pool lease this Tool still holds; actors leased by
// another owner are never touched. It only ever revokes: compiled Tool rules
// are never applied here. It also runs with Substrate MCP Tools disabled, so
// turning the feature off cannot leave a running actor egressing with
// gateway-injected credentials; startup validates the recovery configuration
// whenever a Tool still carries the actor finalizer. An actor Substrate no
// longer has cannot egress, so it counts as revoked. It returns every actor
// proven held, and the joined errors of lease lookups that failed and of the
// actors whose egress was not denied.
func (r *ToolReconciler) revokeSubstrateMCPToolActorEgress(ctx context.Context, tool *corev1alpha1.Tool) ([]substrateMCPHeldActor, error) {
	if tool == nil || !controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
		return nil, nil
	}
	// A lease that cannot be read must not cost the Actors proven held their
	// revocation and stop; the lookup error is still returned so the reconcile
	// retries.
	var errs []error
	leased, err := r.substrateMCPToolActorLeaseDeleteRefs(ctx, tool, "")
	if err != nil {
		errs = append(errs, fmt.Errorf("list the Tool's actor leases: %w", err))
	}
	pooled, err := r.heldSubstrateMCPPoolActorLeaseRefs(ctx, tool, substrateMCPPoolActorLeaseRefs(tool))
	if err != nil {
		errs = append(errs, err)
	}
	held := make([]substrateMCPHeldActor, 0, len(leased)+len(pooled))
	seen := map[string]struct{}{}
	add := func(actor substrateMCPHeldActor) {
		if actor.actorID == "" {
			return
		}
		if _, ok := seen[actor.actorID]; ok {
			return
		}
		seen[actor.actorID] = struct{}{}
		held = append(held, actor)
	}
	for _, ref := range leased {
		add(substrateMCPHeldActor{actorID: strings.TrimSpace(ref.actorID), leaseNamespace: tool.Namespace})
	}
	for _, ref := range pooled {
		add(substrateMCPHeldActor{actorID: strings.TrimSpace(ref.actorID), leaseNamespace: ref.poolNamespace, pooled: true})
	}
	slices.SortFunc(held, func(a, b substrateMCPHeldActor) int { return strings.Compare(a.actorID, b.actorID) })
	cfg := r.SubstrateConfig.WithDefaults()
	apply := func(ctx context.Context, cfg SubstrateConfig, actorID string, rules []workspace.SubstrateEgressRule) error {
		switch {
		case r.SubstrateEgressPolicyStateApplier != nil:
			_, err := r.SubstrateEgressPolicyStateApplier(ctx, cfg, actorID, rules)
			return err
		case r.SubstrateEgressPolicyApplier != nil:
			return r.SubstrateEgressPolicyApplier(ctx, cfg, actorID, rules)
		}
		_, err := applyNativeSubstrateActorEgressPolicy(ctx, cfg, actorID, rules)
		return err
	}
	// Every actor is attempted: one that cannot be reached must not leave the
	// others with their previous rules.
	for _, actor := range held {
		if err := apply(ctx, cfg, actor.actorID, nil); err != nil && !workspace.IsKind(err, workspace.ErrorKindNotFound) {
			errs = append(errs, fmt.Errorf("deny egress for actor %q: %w", actor.actorID, err))
		}
	}
	return held, goerrors.Join(errs...)
}

// substrateMCPHeldActor names an Actor whose ownership lease the Tool holds.
type substrateMCPHeldActor struct {
	actorID string
	// leaseNamespace holds the lease: the pool's namespace for a pooled Actor,
	// otherwise the Tool's.
	leaseNamespace string
	pooled         bool
}

// stop is the stop request for this held Actor; whether its template allows
// suspension is filled in by the caller once the template is read.
func (h substrateMCPHeldActor) stop() substrateMCPActorStop {
	return substrateMCPActorStop{
		actorID: h.actorID, ref: workspace.WorkspaceRef{ClaimName: h.actorID, ID: h.actorID},
		leaseNamespace: h.leaseNamespace, pooled: h.pooled,
	}
}

func applyNativeSubstrateActorEgressPolicy(ctx context.Context, cfg SubstrateConfig, actorID string, rules []workspace.SubstrateEgressRule) (workspace.SubstrateEgressPolicyState, error) {
	cfg = cfg.WithDefaults()
	ctx, cancel := context.WithTimeout(ctx, cfg.ClaimTimeout)
	defer cancel()
	api, err := workspace.NewSubstrateNativeClient(cfg.WorkspaceClientConfig())
	if err != nil {
		return workspace.SubstrateEgressPolicyState{}, err
	}
	defer api.Close() //nolint:errcheck
	return api.ApplyActorEgressPolicy(ctx, actorID, rules)
}

// substrateMCPEgressPropagationWait returns how much longer the egress gateway
// may apply rules other than the Actor's stored policy. See
// workspace.SubstrateEgressPolicyState.EnforcedAfter.
func (r *ToolReconciler) substrateMCPEgressPropagationWait(policy workspace.SubstrateEgressPolicyState) time.Duration {
	enforced := policy.EnforcedAfter(r.SubstrateConfig.EgressPolicyPropagation)
	if enforced.IsZero() {
		return 0
	}
	return max(0, time.Until(enforced))
}

// substrateMCPEgressRules converts the Tool allowlist. A credential names a
// Secret key in the Tool's namespace; credentialProvider is the gateway's
// configured credential provider class, and an empty value makes any
// credential rule fail validation instead of compiling without it.
func substrateMCPEgressRules(tool *corev1alpha1.Tool, credentialProvider string) []workspace.SubstrateEgressRule {
	if tool == nil || tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil {
		return nil
	}
	rules := make([]workspace.SubstrateEgressRule, 0, len(tool.Spec.MCP.SubstrateActor.Egress))
	for _, rule := range tool.Spec.MCP.SubstrateActor.Egress {
		converted := workspace.SubstrateEgressRule{Scheme: string(rule.Scheme), Host: rule.Host, Ports: append([]int32(nil), rule.Ports...)}
		if credential := rule.Credential; credential != nil {
			header := credential.Header
			if header == "" {
				header = "Authorization"
			}
			converted.Credential = &workspace.SubstrateEgressCredential{
				Header: header, Prefix: credential.Prefix, Provider: strings.TrimSpace(credentialProvider),
				Namespace: tool.Namespace, Name: credential.SecretRef.Name, Key: credential.SecretRef.Key,
			}
		}
		rules = append(rules, converted)
	}
	return rules
}

// validateSubstrateMCPEgressCredentials requires every referenced credential
// Secret key to exist and be non-empty, so a misconfigured Tool reports a
// condition instead of an actor whose requests the gateway denies. The value
// itself is never copied into the actor or its egress policy.
func (r *ToolReconciler) validateSubstrateMCPEgressCredentials(ctx context.Context, tool *corev1alpha1.Tool) error {
	if tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil {
		return nil
	}
	for _, rule := range tool.Spec.MCP.SubstrateActor.Egress {
		if rule.Credential == nil {
			continue
		}
		ref := rule.Credential.SecretRef
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: tool.Namespace, Name: ref.Name}, secret); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("mcp.substrateActor.egress credential Secret %q not found", ref.Name)
			}
			return fmt.Errorf("failed to get mcp.substrateActor.egress credential Secret %q: %w", ref.Name, err)
		}
		if strings.TrimSpace(string(secret.Data[ref.Key])) == "" {
			return fmt.Errorf("mcp.substrateActor.egress credential Secret %q key %q is empty or missing", ref.Name, ref.Key)
		}
	}
	return nil
}

// validateSubstrateMCPToolHeaders rejects Tool headers that would override
// router targeting, MCP session, transaction or credential headers owned by
// the Orka MCP proxy.
func validateSubstrateMCPToolHeaders(tool *corev1alpha1.Tool) error {
	if tool.Spec.HTTP == nil {
		return nil
	}
	for name := range tool.Spec.HTTP.Headers {
		if workerexecutor.SubstrateMCPReservedHeader(name) ||
			(strings.EqualFold(strings.TrimSpace(name), "Authorization") && tool.Spec.HTTP.AuthSecretRef != nil) {
			return fmt.Errorf("mcp.substrateActor tools must not configure reserved header %q", name)
		}
	}
	return nil
}

// getHTTPClient returns the HTTP client for health checks.
// privateEndpointHealthClient probes a private connection-mode endpoint
// without ever following a hop the validator did not see.
func privateEndpointHealthClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = toolHealthCheckTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       connectors.PrivateEndpointDialContext,
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (r *ToolReconciler) getHTTPClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{
		Timeout: toolHealthCheckTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// updateStatus updates the Tool status and conditions.
func (r *ToolReconciler) updateStatus(ctx context.Context, tool *corev1alpha1.Tool, available bool, errMsg string) (ctrl.Result, error) {
	return r.updateStatusWithActor(ctx, tool, available, errMsg, "", nil)
}

func (r *ToolReconciler) updateStatusWithActor(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	available bool,
	errMsg string,
	endpoint string,
	actor *corev1alpha1.ToolActorStatus,
) (ctrl.Result, error) {
	now := metav1.Now()
	if actor == nil && !available && tool.Status.Actor != nil {
		preserved := *tool.Status.Actor
		actor = &preserved
	}

	tool.Status.Available = available
	tool.Status.LastCheck = &now
	tool.Status.Error = errMsg
	tool.Status.Endpoint = endpoint
	tool.Status.Actor = actor

	condition := metav1.Condition{
		Type:               toolConditionAvailable,
		LastTransitionTime: now,
		ObservedGeneration: tool.Generation,
	}

	if available {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "EndpointReachable"
		condition.Message = "Tool endpoint is reachable"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "EndpointUnreachable"
		condition.Message = errMsg
	}

	meta.SetStatusCondition(&tool.Status.Conditions, condition)

	if err := r.Status().Update(ctx, tool); err != nil {
		return ctrl.Result{}, err
	}

	// Re-check periodically
	return ctrl.Result{RequeueAfter: toolHealthCheckInterval}, nil
}

func deterministicSubstrateToolActorID(namespace, name, templateNamespace, templateName string) string {
	parts := []string{
		strings.TrimSpace(namespace),
		strings.TrimSpace(name),
		strings.TrimSpace(templateNamespace),
		strings.TrimSpace(templateName),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return workspace.SubstrateActorKey(templateNamespace, "orka-tool-"+hex.EncodeToString(sum[:])[:32])
}

func (r *ToolReconciler) requestsForOutboundAccessPolicy(ctx context.Context, object client.Object) []reconcile.Request {
	policy, ok := object.(*corev1alpha1.OutboundAccessPolicy)
	if !ok || policy == nil {
		return nil
	}
	tools := &corev1alpha1.ToolList{}
	if err := r.List(ctx, tools, client.InNamespace(policy.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range tools.Items {
		tool := &tools.Items[i]
		if tool.Spec.HTTP != nil && tool.Spec.HTTP.OutboundAccessPolicyRef != nil && tool.Spec.HTTP.OutboundAccessPolicyRef.Name == policy.Name {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: tool.Namespace, Name: tool.Name}})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ToolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Tool{}).
		Watches(&corev1alpha1.OutboundAccessPolicy{}, handler.EnqueueRequestsFromMapFunc(r.requestsForOutboundAccessPolicy)).
		Named("tool").
		Complete(r)
}
