/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	// AgentSandboxWarmPoolPolicyDisabled disables pre-created sandbox workspaces.
	AgentSandboxWarmPoolPolicyDisabled = "disabled"
	// AgentSandboxWarmPoolPolicyTemplate allows warm pools to be keyed by workspace template.
	AgentSandboxWarmPoolPolicyTemplate = "template"

	// AgentSandboxNamespaceStrategyTask places sandbox resources in the Task namespace.
	AgentSandboxNamespaceStrategyTask = "task"
	// AgentSandboxNamespaceStrategyController places sandbox resources in the controller namespace.
	AgentSandboxNamespaceStrategyController = "controller"

	// Environment variable names for agent sandbox controller configuration.
	EnvAgentSandboxRouterURL         = "ORKA_AGENT_SANDBOX_ROUTER_URL"
	EnvAgentSandboxDefaultTemplate   = "ORKA_AGENT_SANDBOX_DEFAULT_TEMPLATE"
	EnvAgentSandboxWarmPoolPolicy    = "ORKA_AGENT_SANDBOX_WARM_POOL_POLICY"
	EnvAgentSandboxNamespaceStrategy = "ORKA_AGENT_SANDBOX_NAMESPACE_STRATEGY"
	EnvAgentSandboxClaimTimeout      = "ORKA_AGENT_SANDBOX_CLAIM_TIMEOUT"
	EnvAgentSandboxCommandTimeout    = "ORKA_AGENT_SANDBOX_COMMAND_TIMEOUT"
	EnvAgentSandboxCleanupPolicy     = "ORKA_AGENT_SANDBOX_CLEANUP_POLICY"
)

const (
	defaultAgentSandboxClaimTimeout   = 2 * time.Minute
	defaultAgentSandboxCommandTimeout = 30 * time.Minute
	substrateWorkspaceStagingRoot     = "/app"
	substrateWorkspaceDaemonCommand   = "orka-workspace-agent"
	substrateWorkspaceDaemonListenEnv = "ORKA_WORKSPACE_AGENT_LISTEN_ADDR"
	substrateWorkspaceDaemonListen    = ":8080"
)

var errSubstrateUnsafeLiteralBootstrap = errors.New("unsafe literal substrate bootstrap token")

// AgentSandboxConfig holds disabled-by-default alpha configuration for agent sandbox workspace integration.
// The controller validates workspace requests and propagates resolved settings to agent worker Jobs;
// the worker wrapper performs the upstream sandbox claim, execution, and cleanup lifecycle.
type AgentSandboxConfig struct {
	// RouterURL is the optional base URL for an agent-sandbox router service.
	RouterURL string
	// DefaultTemplate is the default agent-sandbox SandboxWarmPool name used when a workspace omits templateRef.name.
	DefaultTemplate string
	// WarmPoolPolicy is retained for the legacy worker environment contract.
	WarmPoolPolicy string
	// NamespaceStrategy selects where sandbox lifecycle resources are managed.
	NamespaceStrategy string
	// ControllerNamespace is the namespace used for controller-scoped sandbox lifecycle resources.
	ControllerNamespace string
	// ClaimTimeout bounds workspace claim and readiness operations.
	ClaimTimeout time.Duration
	// CommandTimeout bounds sandbox command execution operations.
	CommandTimeout time.Duration
	// CleanupPolicy is the controller default when a Task workspace omits cleanupPolicy.
	CleanupPolicy corev1alpha1.WorkspaceCleanupPolicy
}

// DefaultAgentSandboxConfig returns safe alpha defaults. Empty RouterURL and DefaultTemplate values
// require callers to provide an upstream router/default template or set a template per Task before execution.
func DefaultAgentSandboxConfig() AgentSandboxConfig {
	return AgentSandboxConfig{
		WarmPoolPolicy:    AgentSandboxWarmPoolPolicyDisabled,
		NamespaceStrategy: AgentSandboxNamespaceStrategyTask,
		ClaimTimeout:      defaultAgentSandboxClaimTimeout,
		CommandTimeout:    defaultAgentSandboxCommandTimeout,
		CleanupPolicy:     corev1alpha1.WorkspaceCleanupPolicyDelete,
	}
}

// AgentSandboxConfigFromEnv loads agent sandbox config defaults from the process environment.
func AgentSandboxConfigFromEnv(getenv func(string) string) (AgentSandboxConfig, error) {
	cfg := DefaultAgentSandboxConfig()

	if value := getenv(EnvAgentSandboxRouterURL); value != "" {
		cfg.RouterURL = value
	}
	if value := getenv(EnvAgentSandboxDefaultTemplate); value != "" {
		cfg.DefaultTemplate = value
	}
	if value := getenv(EnvAgentSandboxWarmPoolPolicy); value != "" {
		cfg.WarmPoolPolicy = value
	}
	if value := getenv(EnvAgentSandboxNamespaceStrategy); value != "" {
		cfg.NamespaceStrategy = value
	}
	if value := getenv(EnvAgentSandboxClaimTimeout); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("parse %s: %w", EnvAgentSandboxClaimTimeout, err)
		}
		cfg.ClaimTimeout = duration
	}
	if value := getenv(EnvAgentSandboxCommandTimeout); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("parse %s: %w", EnvAgentSandboxCommandTimeout, err)
		}
		cfg.CommandTimeout = duration
	}
	if value := getenv(EnvAgentSandboxCleanupPolicy); value != "" {
		cfg.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicy(value)
	}

	return cfg.WithDefaults(), nil
}

// WithDefaults fills unset optional fields with safe defaults.
func (c AgentSandboxConfig) WithDefaults() AgentSandboxConfig {
	if c.WarmPoolPolicy == "" {
		c.WarmPoolPolicy = AgentSandboxWarmPoolPolicyDisabled
	}
	if c.NamespaceStrategy == "" {
		c.NamespaceStrategy = AgentSandboxNamespaceStrategyTask
	}
	if c.ClaimTimeout == 0 {
		c.ClaimTimeout = defaultAgentSandboxClaimTimeout
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = defaultAgentSandboxCommandTimeout
	}
	if c.CleanupPolicy == "" {
		c.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyDelete
	}
	return c
}

// Validate rejects unsupported sandbox config values. Callers should only enforce this while the
// experimental sandbox feature gate is enabled so unrelated startup behavior remains unchanged.
func (c AgentSandboxConfig) Validate() error {
	cfg := c.WithDefaults()

	switch cfg.WarmPoolPolicy {
	case AgentSandboxWarmPoolPolicyDisabled, AgentSandboxWarmPoolPolicyTemplate:
	default:
		return fmt.Errorf("unsupported agent sandbox warm pool policy %q", cfg.WarmPoolPolicy)
	}

	switch cfg.NamespaceStrategy {
	case AgentSandboxNamespaceStrategyTask, AgentSandboxNamespaceStrategyController:
	default:
		return fmt.Errorf("unsupported agent sandbox namespace strategy %q", cfg.NamespaceStrategy)
	}

	if cfg.ClaimTimeout <= 0 {
		return fmt.Errorf("agent sandbox claim timeout must be greater than zero")
	}
	if cfg.CommandTimeout <= 0 {
		return fmt.Errorf("agent sandbox command timeout must be greater than zero")
	}

	switch cfg.CleanupPolicy {
	case corev1alpha1.WorkspaceCleanupPolicyDelete, corev1alpha1.WorkspaceCleanupPolicyRetain:
	default:
		return fmt.Errorf("unsupported agent sandbox cleanup policy %q", cfg.CleanupPolicy)
	}

	return nil
}

func executionWorkspaceTemplateName(ws *corev1alpha1.ExecutionWorkspaceSpec, cfg AgentSandboxConfig) string {
	if ws != nil && ws.TemplateRef != nil && ws.TemplateRef.Name != "" {
		return ws.TemplateRef.Name
	}
	return cfg.WithDefaults().DefaultTemplate
}

func executionWorkspaceTemplateNamespace(ws *corev1alpha1.ExecutionWorkspaceSpec, taskNamespace string, cfg AgentSandboxConfig) string {
	if ws != nil && ws.TemplateRef != nil && strings.TrimSpace(ws.TemplateRef.Namespace) != "" {
		return strings.TrimSpace(ws.TemplateRef.Namespace)
	}
	cfg = cfg.WithDefaults()
	if cfg.NamespaceStrategy == AgentSandboxNamespaceStrategyController && strings.TrimSpace(cfg.ControllerNamespace) != "" {
		return strings.TrimSpace(cfg.ControllerNamespace)
	}
	return taskNamespace
}

func substrateTemplateName(ws *corev1alpha1.ExecutionWorkspaceSpec, cfg SubstrateConfig) string {
	if ws != nil && ws.TemplateRef != nil && strings.TrimSpace(ws.TemplateRef.Name) != "" {
		return strings.TrimSpace(ws.TemplateRef.Name)
	}
	return strings.TrimSpace(cfg.WithDefaults().DefaultTemplate)
}

func substrateTemplateNamespace(ws *corev1alpha1.ExecutionWorkspaceSpec, taskNamespace string, cfg SubstrateConfig) string {
	if ws != nil && ws.TemplateRef != nil && strings.TrimSpace(ws.TemplateRef.Namespace) != "" {
		return strings.TrimSpace(ws.TemplateRef.Namespace)
	}
	cfg = cfg.WithDefaults()
	if strings.TrimSpace(cfg.DefaultTemplateNS) != "" {
		return strings.TrimSpace(cfg.DefaultTemplateNS)
	}
	return taskNamespace
}

// resolveExecutionWorkspaceRequest applies controller defaults to a Task execution workspace request.
// It returns nil when the Task does not request an enabled execution workspace.
func (r *TaskReconciler) resolveExecutionWorkspaceRequest(ctx context.Context, task *corev1alpha1.Task) (*ExecutionWorkspaceRequest, error) {
	if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil || !task.Spec.Execution.Workspace.Enabled {
		return nil, nil
	}
	if err := r.validateExecutionWorkspace(task); err != nil {
		return nil, err
	}

	provider := resolveWorkspaceProvider(task.Spec.Execution.Workspace, r.ExecutionWorkspaceDefaultProvider)
	if provider == corev1alpha1.WorkspaceProviderSubstrate {
		return r.resolveSubstrateWorkspaceRequest(ctx, task)
	}
	return r.resolveAgentSandboxWorkspaceRequest(ctx, task)
}

func (r *TaskReconciler) resolveAgentSandboxWorkspaceRequest(ctx context.Context, task *corev1alpha1.Task) (*ExecutionWorkspaceRequest, error) {
	cfg := r.AgentSandboxConfig.WithDefaults()
	ws := task.Spec.Execution.Workspace

	reusePolicy := ws.ReusePolicy
	if reusePolicy == "" {
		reusePolicy = corev1alpha1.WorkspaceReusePolicyNone
	}
	cleanupPolicy := ws.CleanupPolicy
	if cleanupPolicy == "" {
		cleanupPolicy = cfg.CleanupPolicy
	}

	templateNamespace := executionWorkspaceTemplateNamespace(ws, task.Namespace, cfg)
	request := &ExecutionWorkspaceRequest{
		Provider:          corev1alpha1.WorkspaceProviderAgentSandbox,
		RouterURL:         cfg.RouterURL,
		TemplateName:      executionWorkspaceTemplateName(ws, cfg),
		TemplateNamespace: templateNamespace,
		ClaimNamespace:    templateNamespace,
		ReusePolicy:       reusePolicy,
		CleanupPolicy:     cleanupPolicy,
		WarmPoolPolicy:    cfg.WarmPoolPolicy,
		NamespaceStrategy: cfg.NamespaceStrategy,
		ClaimTimeout:      cfg.ClaimTimeout,
		CommandTimeout:    cfg.CommandTimeout,
	}
	if reusePolicy == corev1alpha1.WorkspaceReusePolicySession && task.Spec.SessionRef != nil {
		request.ReuseKey = task.Spec.SessionRef.Name
	}

	if err := r.validateExecutionWorkspaceWarmPoolExists(ctx, task, request); err != nil {
		return nil, err
	}

	return request, nil
}

func (r *TaskReconciler) resolveSubstrateWorkspaceRequest(ctx context.Context, task *corev1alpha1.Task) (*ExecutionWorkspaceRequest, error) {
	cfg := r.SubstrateConfig.WithDefaults()
	ws := task.Spec.Execution.Workspace

	reusePolicy := ws.ReusePolicy
	if reusePolicy == "" {
		reusePolicy = corev1alpha1.WorkspaceReusePolicyNone
	}
	cleanupPolicy := ws.CleanupPolicy
	if cleanupPolicy == "" {
		cleanupPolicy = cfg.CleanupPolicy
	}
	templateName := substrateTemplateName(ws, cfg)
	templateNamespace := substrateTemplateNamespace(ws, task.Namespace, cfg)
	snapshotRestoreURI, snapshotCheckpointURI, snapshotOnRelease := executionWorkspaceSnapshot(ws)
	processMode, residentKey := executionWorkspaceHibernation(ws)
	reuseKey := ""
	claimName := deterministicSubstrateTaskActorID(string(task.UID), task.Status.Attempts+1)
	poolTargetActors := int32(0)
	if reusePolicy == corev1alpha1.WorkspaceReusePolicySession && task.Spec.SessionRef != nil {
		reuseKey = task.Spec.SessionRef.Name
		claimName = deterministicSubstrateSessionActorID(task.Namespace, templateNamespace, templateName, reuseKey)
	}
	if residentKey == "" && processMode == corev1alpha1.ExecutionWorkspaceProcessModeResident {
		residentKey = deterministicSubstrateSessionActorID(task.Namespace, templateNamespace, templateName, firstNonEmpty(reuseKey, claimName))
	}
	poolName, poolNamespace := executionWorkspacePoolRef(ws, task.Namespace)
	if poolName != "" {
		if r.EnforceNamespaceIsolation && poolNamespace != task.Namespace {
			return nil, fmt.Errorf(
				"cross-namespace substrate actor poolRef not allowed when namespace isolation is enforced: pool %q in namespace %q, task in %q",
				poolName,
				poolNamespace,
				task.Namespace,
			)
		}
		pool, err := r.resolveExecutionWorkspacePool(ctx, poolNamespace, poolName, templateNamespace, templateName)
		if err != nil {
			return nil, err
		}
		prefix := deterministicSubstratePoolActorPrefix(poolNamespace, poolName)
		ordinal := deterministicSubstratePoolActorOrdinal(
			pool.Spec.TargetActors,
			prefix,
			task.Namespace,
			string(task.UID),
			fmt.Sprint(task.Status.Attempts+1),
			reuseKey,
		)
		if reusePolicy == corev1alpha1.WorkspaceReusePolicySession && reuseKey != "" {
			ordinal = deterministicSubstratePoolActorOrdinal(
				pool.Spec.TargetActors,
				prefix,
				task.Namespace,
				templateNamespace,
				templateName,
				reuseKey,
			)
		}
		claimName = deterministicSubstratePoolActorID(prefix, ordinal)
		cleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyDelete
		poolTargetActors = pool.Spec.TargetActors
	}

	request := &ExecutionWorkspaceRequest{
		Provider:                           corev1alpha1.WorkspaceProviderSubstrate,
		TemplateName:                       templateName,
		TemplateNamespace:                  templateNamespace,
		ClaimNamespace:                     templateNamespace,
		ClaimName:                          claimName,
		ReusePolicy:                        reusePolicy,
		ReuseKey:                           reuseKey,
		CleanupPolicy:                      cleanupPolicy,
		Boot:                               ws.Boot,
		PoolName:                           poolName,
		PoolNamespace:                      poolNamespace,
		PoolTargetActors:                   poolTargetActors,
		SnapshotRestoreURI:                 snapshotRestoreURI,
		SnapshotCheckpointURI:              snapshotCheckpointURI,
		SnapshotOnRelease:                  snapshotOnRelease,
		ProcessMode:                        processMode,
		ResidentKey:                        residentKey,
		ClaimTimeout:                       cfg.ClaimTimeout,
		CommandTimeout:                     cfg.CommandTimeout,
		SubstrateAPIEndpoint:               cfg.APIEndpoint,
		SubstrateAPICAFile:                 cfg.APICAFile,
		SubstrateAPIInsecureSkipVerify:     cfg.APIInsecureSkipVerify,
		SubstrateRouterURL:                 cfg.RouterURL,
		SubstrateActorDNSSuffix:            cfg.ActorDNSSuffix,
		SubstrateBootstrapSecretName:       cfg.BootstrapSecretName,
		SubstrateBootstrapSecretKey:        cfg.BootstrapSecretKey,
		SubstrateSessionIdentitySecretName: cfg.SessionIdentitySecretName,
		SubstrateSessionIdentitySecretKey:  cfg.SessionIdentitySecretKey,
		SubstrateSessionIdentityRequired:   cfg.SessionIdentityRequired,
		SubstrateSessionIdentityMintCert:   cfg.SessionIdentityMintCert,
		SubstrateSessionIdentityAudience:   cfg.SessionIdentityAudience,
		SubstrateSessionIdentityAppID:      cfg.SessionIdentityAppID,
		SubstrateSessionIdentityUserID:     cfg.SessionIdentityUserID,
	}

	if err := r.validateSubstrateWorkspaceTemplate(ctx, task, request); err != nil {
		return nil, err
	}
	return request, nil
}

func executionWorkspacePoolRef(ws *corev1alpha1.ExecutionWorkspaceSpec, taskNamespace string) (string, string) {
	if ws == nil || ws.PoolRef == nil {
		return "", ""
	}
	return substrateActorPoolReference(ws.PoolRef, taskNamespace)
}

func substrateActorPoolReference(ref *corev1alpha1.SubstrateActorPoolReference, defaultNamespace string) (string, string) {
	if ref == nil {
		return "", ""
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", ""
	}
	namespace := strings.TrimSpace(ref.Namespace)
	if namespace == "" {
		namespace = defaultNamespace
	}
	return name, namespace
}

func (r *TaskReconciler) resolveExecutionWorkspacePool(
	ctx context.Context,
	poolNamespace string,
	poolName string,
	templateNamespace string,
	templateName string,
) (*corev1alpha1.SubstrateActorPool, error) {
	if r == nil || r.Client == nil {
		return nil, fmt.Errorf("substrate actor poolRef %q/%q cannot be resolved without a Kubernetes client", poolNamespace, poolName)
	}
	return resolveSubstrateActorPoolReference(ctx, r.Client, poolNamespace, poolName, templateNamespace, templateName)
}

func resolveSubstrateActorPoolReference(
	ctx context.Context,
	reader client.Reader,
	poolNamespace string,
	poolName string,
	templateNamespace string,
	templateName string,
) (*corev1alpha1.SubstrateActorPool, error) {
	if reader == nil {
		return nil, fmt.Errorf("substrate actor poolRef %q/%q cannot be resolved without a Kubernetes client", poolNamespace, poolName)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pool := &corev1alpha1.SubstrateActorPool{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: poolName}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("substrate actor poolRef %q not found in namespace %q", poolName, poolNamespace)
		}
		return nil, fmt.Errorf("failed to resolve substrate actor poolRef %q in namespace %q: %w", poolName, poolNamespace, err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("substrate actor poolRef %q in namespace %q is deleting", poolName, poolNamespace)
	}
	if err := validateSubstrateActorPoolTargetActors(poolNamespace, poolName, pool.Spec.TargetActors, false); err != nil {
		return nil, err
	}
	if !controllerutil.ContainsFinalizer(pool, substrateActorPoolFinalizer) {
		updater, ok := reader.(client.Client)
		if !ok {
			return nil, fmt.Errorf("substrate actor poolRef %q in namespace %q cannot persist cleanup finalizer", poolName, poolNamespace)
		}
		patch := client.MergeFrom(pool.DeepCopy())
		controllerutil.AddFinalizer(pool, substrateActorPoolFinalizer)
		if err := updater.Patch(ctx, pool, patch); err != nil {
			return nil, fmt.Errorf("failed to persist cleanup finalizer for substrate actor poolRef %q in namespace %q: %w", poolName, poolNamespace, err)
		}
	}
	poolTemplateNamespace := strings.TrimSpace(pool.Spec.TemplateRef.Namespace)
	if poolTemplateNamespace == "" {
		poolTemplateNamespace = pool.Namespace
	}
	poolTemplateName := strings.TrimSpace(pool.Spec.TemplateRef.Name)
	if poolTemplateNamespace != strings.TrimSpace(templateNamespace) || poolTemplateName != strings.TrimSpace(templateName) {
		return nil, fmt.Errorf(
			"substrate actor poolRef %q in namespace %q uses template %s/%s, want %s/%s",
			poolName,
			poolNamespace,
			poolTemplateNamespace,
			poolTemplateName,
			strings.TrimSpace(templateNamespace),
			strings.TrimSpace(templateName),
		)
	}
	return pool, nil
}

func (r *TaskReconciler) validateExecutionWorkspaceWarmPoolExists(ctx context.Context, task *corev1alpha1.Task, request *AgentSandboxWorkspaceRequest) error {
	if r == nil || r.Client == nil || request == nil || request.TemplateName == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// The upstream agent-sandbox SDK v0.5 claims a SandboxWarmPool by name,
	// so validate the effective namespace where the SandboxClaim will be created.
	lookupNamespace := request.ClaimNamespace
	if strings.TrimSpace(lookupNamespace) == "" {
		lookupNamespace = request.TemplateNamespace
	}
	if strings.TrimSpace(lookupNamespace) == "" {
		lookupNamespace = task.Namespace
	}

	warmPool := &sandboxextv1beta1.SandboxWarmPool{}
	err := r.Get(ctx, types.NamespacedName{Namespace: lookupNamespace, Name: request.TemplateName}, warmPool)
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return fmt.Errorf(
			"execution workspace warm pool %q not found in namespace %q",
			request.TemplateName,
			lookupNamespace,
		)
	}
	return fmt.Errorf(
		"failed to validate execution workspace warm pool %q in namespace %q: %w",
		request.TemplateName,
		lookupNamespace,
		err,
	)
}

func (r *TaskReconciler) validateSubstrateWorkspaceTemplate(ctx context.Context, task *corev1alpha1.Task, request *ExecutionWorkspaceRequest) error {
	_ = task
	if r == nil {
		return nil
	}
	return validateSubstrateActorTemplateResource(ctx, r.Client, request)
}

func validateSubstrateActorTemplateResource(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) error {
	template, err := validateSubstrateActorTemplateMetadata(ctx, reader, request)
	if err != nil {
		return err
	}
	if template == nil {
		return nil
	}
	annotations := template.GetAnnotations()
	if err := validateSubstrateWorkspaceTemplateStagingRoot(request, annotations); err != nil {
		return err
	}
	if err := validateSubstrateActorTemplateReady(request, template); err != nil {
		return err
	}
	if err := validateSubstrateWorkspaceTemplateDaemonPort(template, annotations["orka.ai/workspace-daemon-port"]); err != nil {
		return substrateActorTemplateRequestError(request, err)
	}
	if err := validateSubstrateWorkspaceTemplateBootstrapEnv(template, request); err != nil {
		return substrateActorTemplateRequestError(request, err)
	}
	if err := unsafeLiteralSubstrateBootstrapEnv(template, request); err != nil {
		return substrateActorTemplateRequestError(request, err)
	}
	return nil
}

func validateSubstrateMCPActorTemplateResource(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) error {
	return validateSubstrateRoutableActorTemplateResource(ctx, reader, request)
}

func validateSubstrateRoutableActorTemplateResource(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) error {
	template, err := validateSubstrateActorTemplateMetadata(ctx, reader, request)
	if err != nil {
		return err
	}
	if template == nil {
		return nil
	}
	unsafeLiteralBootstrapErr := unsafeLiteralDurableSubstrateBootstrapEnv(template)
	if err := validateSubstrateRoutableActorTemplate(request, template); err != nil {
		if unsafeLiteralBootstrapErr != nil {
			return substrateActorTemplateRequestError(request, unsafeLiteralBootstrapErr)
		}
		return err
	}
	if err := validateSubstrateRoutableActorTemplateBootstrapEnv(template); err != nil {
		return substrateActorTemplateRequestError(request, err)
	}
	return nil
}

func validateSubstratePrecreatedPoolActorTemplateResource(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) error {
	template, err := validateSubstrateActorTemplateMetadata(ctx, reader, request)
	if err != nil {
		return err
	}
	if template == nil {
		return nil
	}
	unsafeLiteralBootstrapErr := unsafeLiteralSubstrateBootstrapEnv(template, request)
	if err := validateSubstrateRoutableActorTemplate(request, template); err != nil {
		if unsafeLiteralBootstrapErr != nil {
			return substrateActorTemplateRequestError(request, unsafeLiteralBootstrapErr)
		}
		return err
	}
	if err := validateSubstratePoolPrecreateWorkspaceDaemonBootstrapEnv(template, request); err != nil {
		if unsafeLiteralBootstrapErr != nil {
			return substrateActorTemplateRequestError(request, unsafeLiteralBootstrapErr)
		}
		return substrateActorTemplateRequestError(request, err)
	}
	if unsafeLiteralBootstrapErr != nil {
		return substrateActorTemplateRequestError(request, unsafeLiteralBootstrapErr)
	}
	return nil
}

func substrateActorTemplateRequestError(request *ExecutionWorkspaceRequest, err error) error {
	return fmt.Errorf("substrate ActorTemplate %q in namespace %q %w", request.TemplateName, request.TemplateNamespace, err)
}

func validateSubstrateRoutableActorTemplateBootstrapEnv(template *unstructured.Unstructured) error {
	return unsafeLiteralDurableSubstrateBootstrapEnv(template)
}

func validateSubstrateRoutableActorTemplate(request *ExecutionWorkspaceRequest, template *unstructured.Unstructured) error {
	annotations := template.GetAnnotations()
	if err := validateSubstrateActorTemplateReady(request, template); err != nil {
		return err
	}
	if err := validateSubstrateMCPActorTemplateRoutePort(template, annotations["orka.ai/workspace-daemon-port"]); err != nil {
		return substrateActorTemplateRequestError(request, err)
	}
	return nil
}

func validateSubstrateActorTemplateMetadata(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) (*unstructured.Unstructured, error) {
	if reader == nil || request == nil || request.TemplateName == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "ate.dev",
		Version: "v1alpha1",
		Kind:    "ActorTemplate",
	})
	err := reader.Get(ctx, types.NamespacedName{Namespace: request.TemplateNamespace, Name: request.TemplateName}, template)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf(
				"substrate execution workspace ActorTemplate %q not found in namespace %q",
				request.TemplateName,
				request.TemplateNamespace,
			)
		}
		return nil, fmt.Errorf(
			"failed to validate substrate execution workspace ActorTemplate %q in namespace %q: %w",
			request.TemplateName,
			request.TemplateNamespace,
			err,
		)
	}

	labels := template.GetLabels()
	annotations := template.GetAnnotations()
	if labels["orka.ai/execution-workspace"] != scheduledRunLabelValue {
		return nil, fmt.Errorf("substrate ActorTemplate %q in namespace %q missing label orka.ai/execution-workspace=true", request.TemplateName, request.TemplateNamespace)
	}
	if labels["orka.ai/workspace-provider"] != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return nil, fmt.Errorf("substrate ActorTemplate %q in namespace %q missing label orka.ai/workspace-provider=substrate", request.TemplateName, request.TemplateNamespace)
	}
	if annotations["orka.ai/workspace-protocol"] != "http-json-v1" {
		return nil, fmt.Errorf("substrate ActorTemplate %q in namespace %q missing annotation orka.ai/workspace-protocol=http-json-v1", request.TemplateName, request.TemplateNamespace)
	}
	if strings.TrimSpace(annotations["orka.ai/workspace-daemon-port"]) == "" {
		return nil, fmt.Errorf("substrate ActorTemplate %q in namespace %q missing annotation orka.ai/workspace-daemon-port", request.TemplateName, request.TemplateNamespace)
	}
	return template, nil
}

func validateSubstrateActorTemplateReady(request *ExecutionWorkspaceRequest, template *unstructured.Unstructured) error {
	phase, _, _ := unstructured.NestedString(template.Object, "status", "phase")
	phase = strings.TrimSpace(phase)
	if phase != "Ready" {
		if phase == "" {
			phase = "<empty>"
		}
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q is not Ready: phase=%s", request.TemplateName, request.TemplateNamespace, phase)
	}
	return nil
}

func validateSubstrateWorkspaceTemplateStagingRoot(request *ExecutionWorkspaceRequest, annotations map[string]string) error {
	stagingRoot := strings.TrimRight(strings.TrimSpace(annotations["orka.ai/workspace-staging-root"]), "/")
	if stagingRoot == "" {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q missing annotation orka.ai/workspace-staging-root", request.TemplateName, request.TemplateNamespace)
	}
	if stagingRoot != substrateWorkspaceStagingRoot {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q must set annotation orka.ai/workspace-staging-root=%s", request.TemplateName, request.TemplateNamespace, substrateWorkspaceStagingRoot)
	}
	return nil
}

func validateSubstrateMCPActorTemplateRoutePort(template *unstructured.Unstructured, annotatedPort string) error {
	port, err := parseSubstrateWorkspaceDaemonPort(annotatedPort)
	if err != nil {
		return fmt.Errorf("has invalid annotation orka.ai/workspace-daemon-port: %w", err)
	}
	containers, found, err := unstructured.NestedSlice(template.Object, "spec", "containers")
	if err != nil {
		return fmt.Errorf("has invalid spec.containers: %w", err)
	}
	if !found || len(containers) == 0 {
		return fmt.Errorf("must define an MCP server container")
	}
	hasExplicitPorts := false
	hasMatchingPort := false
	hasExplicitListenEnv := false
	hasMatchingListenEnv := false
	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("has invalid spec.containers entry")
		}
		matches, hasPorts, err := substrateContainerHasPort(container, port)
		if err != nil {
			return err
		}
		hasExplicitPorts = hasExplicitPorts || hasPorts
		if matches {
			hasMatchingPort = true
		}
		matches, hasListenEnv, err := substrateContainerHasListenEnvPort(container, port)
		if err != nil {
			return err
		}
		hasExplicitListenEnv = hasExplicitListenEnv || hasListenEnv
		if matches {
			hasMatchingListenEnv = true
		}
	}
	if hasExplicitListenEnv {
		if hasMatchingListenEnv {
			return nil
		}
		return fmt.Errorf("no container listen env matches annotation orka.ai/workspace-daemon-port=%d", port)
	}
	if hasExplicitPorts {
		if hasMatchingPort {
			return nil
		}
		return fmt.Errorf("no container port matches annotation orka.ai/workspace-daemon-port=%d", port)
	}
	defaultPort, err := substrateWorkspaceDaemonListenPort(map[string]any{})
	if err != nil {
		return err
	}
	if port != defaultPort {
		return fmt.Errorf("annotation orka.ai/workspace-daemon-port=%d requires a matching containerPort when no container ports are declared", port)
	}
	return nil
}

func substrateContainerHasListenEnvPort(container map[string]any, want int) (bool, bool, error) {
	env, found, err := substrateContainerEnv(container)
	if err != nil {
		return false, false, err
	}
	if !found {
		return false, false, nil
	}
	value, found, err := substrateContainerLiteralEnv(env, substrateWorkspaceDaemonListenEnv)
	if err != nil {
		return false, true, err
	}
	if !found {
		return false, false, nil
	}
	_, portValue, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return false, true, fmt.Errorf("env %s must be a listen address with a port", substrateWorkspaceDaemonListenEnv)
	}
	port, err := parseSubstrateWorkspaceDaemonPort(portValue)
	if err != nil {
		return false, true, fmt.Errorf("env %s port %w", substrateWorkspaceDaemonListenEnv, err)
	}
	return port == want, true, nil
}

func substrateContainerHasPort(container map[string]any, want int) (bool, bool, error) {
	portsValue, found, err := unstructured.NestedFieldNoCopy(container, "ports")
	if err != nil {
		return false, false, fmt.Errorf("has invalid container ports: %w", err)
	}
	if !found || portsValue == nil {
		return false, false, nil
	}
	ports, ok := portsValue.([]any)
	if !ok {
		return false, false, fmt.Errorf("has invalid container ports")
	}
	for _, item := range ports {
		port, ok := item.(map[string]any)
		if !ok {
			return false, true, fmt.Errorf("has invalid container port entry")
		}
		containerPort, found, err := substrateContainerPortNumber(port)
		if err != nil {
			return false, true, err
		}
		if found && containerPort == want {
			return true, true, nil
		}
	}
	return false, true, nil
}

func substrateContainerPortNumber(port map[string]any) (int, bool, error) {
	value, found, err := unstructured.NestedFieldNoCopy(port, "containerPort")
	if err != nil {
		return 0, false, fmt.Errorf("has invalid containerPort: %w", err)
	}
	if !found || value == nil {
		return 0, false, nil
	}
	switch typed := value.(type) {
	case int:
		return validateSubstrateContainerPort(typed)
	case int32:
		return validateSubstrateContainerPort(int(typed))
	case int64:
		return validateSubstrateContainerPort(int(typed))
	case float64:
		asInt := int(typed)
		if typed != float64(asInt) {
			return 0, true, fmt.Errorf("containerPort must be an integer")
		}
		return validateSubstrateContainerPort(asInt)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, true, fmt.Errorf("containerPort must be an integer")
		}
		return validateSubstrateContainerPort(parsed)
	default:
		return 0, true, fmt.Errorf("containerPort must be an integer")
	}
}

func validateSubstrateContainerPort(port int) (int, bool, error) {
	if port < 1 || port > 65535 {
		return 0, true, fmt.Errorf("containerPort must be a TCP port number from 1 to 65535")
	}
	return port, true, nil
}

func validateSubstrateWorkspaceTemplateDaemonPort(template *unstructured.Unstructured, annotatedPort string) error {
	port, err := parseSubstrateWorkspaceDaemonPort(annotatedPort)
	if err != nil {
		return fmt.Errorf("has invalid annotation orka.ai/workspace-daemon-port: %w", err)
	}
	containers, found, err := unstructured.NestedSlice(template.Object, "spec", "containers")
	if err != nil {
		return fmt.Errorf("has invalid spec.containers: %w", err)
	}
	if !found || len(containers) == 0 {
		return fmt.Errorf("must define a workspace daemon container")
	}

	daemonContainers, err := substrateWorkspaceDaemonContainers(containers)
	if err != nil {
		return err
	}
	for _, container := range daemonContainers {
		listenPort, err := substrateWorkspaceDaemonListenPort(container)
		if err != nil {
			return substrateWorkspaceDaemonContainerError(container, err)
		}
		if listenPort != port {
			return substrateWorkspaceDaemonContainerError(
				container,
				fmt.Errorf(
					"listen port %d must match annotation orka.ai/workspace-daemon-port=%d",
					listenPort,
					port,
				),
			)
		}
	}
	return nil
}

func substrateWorkspaceDaemonContainerError(container map[string]any, err error) error {
	name, _, _ := unstructured.NestedString(container, "name")
	if strings.TrimSpace(name) != "" {
		return fmt.Errorf("workspace daemon container %q %w", name, err)
	}
	return fmt.Errorf("workspace daemon container %w", err)
}

func parseSubstrateWorkspaceDaemonPort(value string) (int, error) {
	value = strings.TrimSpace(value)
	port, err := strconv.ParseUint(value, 10, 16)
	if err != nil || port == 0 {
		return 0, fmt.Errorf("must be a TCP port number from 1 to 65535")
	}
	return int(port), nil
}

func substrateWorkspaceDaemonListenPort(container map[string]any) (int, error) {
	listenAddr := substrateWorkspaceDaemonListen
	env, found, err := substrateContainerEnv(container)
	if err != nil {
		return 0, err
	}
	if found {
		if value, ok, err := substrateContainerLiteralEnv(env, substrateWorkspaceDaemonListenEnv); err != nil {
			return 0, err
		} else if ok {
			listenAddr = value
		}
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return 0, fmt.Errorf("env %s must be a listen address with a port", substrateWorkspaceDaemonListenEnv)
	}
	return parseSubstrateWorkspaceDaemonPort(port)
}

func substrateContainerEnv(container map[string]any) ([]any, bool, error) {
	envValue, found, err := unstructured.NestedFieldNoCopy(container, "env")
	if err != nil {
		return nil, false, fmt.Errorf("has invalid container env: %w", err)
	}
	if !found || envValue == nil {
		return nil, false, nil
	}
	env, ok := envValue.([]any)
	if !ok {
		return nil, false, fmt.Errorf("has invalid container env")
	}
	return env, true, nil
}

func substrateContainerLiteralEnv(env []any, name string) (string, bool, error) {
	for _, envItem := range env {
		envVar, ok := envItem.(map[string]any)
		if !ok {
			return "", false, fmt.Errorf("has invalid container env entry")
		}
		envName, _, _ := unstructured.NestedString(envVar, "name")
		if envName != name {
			continue
		}
		value, found, err := unstructured.NestedString(envVar, "value")
		if err != nil {
			return "", false, fmt.Errorf("env %s has invalid value", name)
		}
		if !found {
			return "", false, fmt.Errorf("env %s must set a literal value", name)
		}
		return strings.TrimSpace(value), true, nil
	}
	return "", false, nil
}

func validateSubstrateWorkspaceTemplateBootstrapEnv(template *unstructured.Unstructured, request *ExecutionWorkspaceRequest) error {
	containers, found, err := unstructured.NestedSlice(template.Object, "spec", "containers")
	if err != nil {
		return fmt.Errorf("has invalid spec.containers: %w", err)
	}
	if !found || len(containers) == 0 {
		return fmt.Errorf("must define a container with %s", workerenv.WorkspaceBootstrapToken)
	}

	daemonContainers, err := substrateWorkspaceDaemonContainers(containers)
	if err != nil {
		return err
	}
	for _, container := range daemonContainers {
		if err := validateSubstrateWorkspaceDaemonBootstrapEnv(container, request); err != nil {
			return substrateWorkspaceDaemonContainerError(container, err)
		}
	}
	return nil
}

func substrateWorkspaceDaemonContainers(containers []any) ([]map[string]any, error) {
	parsed := make([]map[string]any, 0, len(containers))
	daemons := make([]map[string]any, 0, len(containers))

	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("has invalid spec.containers entry")
		}
		parsed = append(parsed, container)

		runsDaemon, err := substrateContainerRunsWorkspaceDaemon(container)
		if err != nil {
			return nil, err
		}
		if runsDaemon {
			daemons = append(daemons, container)
		}
	}

	if len(daemons) > 0 {
		return daemons, nil
	}
	if len(parsed) == 1 {
		return parsed, nil
	}
	return nil, fmt.Errorf(
		"must identify the workspace daemon container with command or args containing /%s",
		substrateWorkspaceDaemonCommand,
	)
}

func substrateContainerRunsWorkspaceDaemon(container map[string]any) (bool, error) {
	for _, field := range []string{"command", "args"} {
		values, err := substrateContainerStringList(container, field)
		if err != nil {
			return false, err
		}
		for _, value := range values {
			if strings.Contains(value, substrateWorkspaceDaemonCommand) {
				return true, nil
			}
		}
	}
	return false, nil
}

func substrateContainerStringList(container map[string]any, field string) ([]string, error) {
	value, found, err := unstructured.NestedFieldNoCopy(container, field)
	if err != nil {
		return nil, fmt.Errorf("has invalid container %s: %w", field, err)
	}
	if !found || value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("has invalid container %s", field)
	}

	result := make([]string, 0, len(values))
	for _, item := range values {
		stringValue, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("has invalid container %s entry", field)
		}
		result = append(result, strings.TrimSpace(stringValue))
	}
	return result, nil
}

func validateSubstrateWorkspaceDaemonBootstrapEnv(container map[string]any, request *ExecutionWorkspaceRequest) error {
	env, found, err := substrateContainerEnv(container)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("missing required env %s", workerenv.WorkspaceBootstrapToken)
	}
	for _, envItem := range env {
		envVar, ok := envItem.(map[string]any)
		if !ok {
			return fmt.Errorf("has invalid container env entry")
		}
		name, _, _ := unstructured.NestedString(envVar, "name")
		if name != workerenv.WorkspaceBootstrapToken {
			continue
		}
		value, valueFound, valueErr := unstructured.NestedString(envVar, "value")
		if valueErr != nil {
			return fmt.Errorf("%s has invalid value", workerenv.WorkspaceBootstrapToken)
		}
		if valueFound && strings.TrimSpace(value) != "" {
			return validateSubstrateLiteralBootstrapEnvAllowed(request)
		}
		if ok, err := substrateBootstrapEnvUsesConfiguredSecret(envVar, request); err != nil {
			return err
		} else if ok {
			return nil
		}
		return fmt.Errorf(
			"%s must set valueFrom.secretKeyRef",
			workerenv.WorkspaceBootstrapToken,
		)
	}

	return fmt.Errorf("missing required env %s", workerenv.WorkspaceBootstrapToken)
}

func substrateActorTemplateUnsafeLiteralDurableBootstrapEnv(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) error {
	template, ok := getSubstrateActorTemplateForUnsafeBootstrapCheck(ctx, reader, request)
	if !ok {
		return nil
	}
	return unsafeLiteralDurableSubstrateBootstrapEnv(template)
}

func getSubstrateActorTemplateForUnsafeBootstrapCheck(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) (*unstructured.Unstructured, bool) {
	if reader == nil || request == nil || request.TemplateName == "" {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "ate.dev",
		Version: "v1alpha1",
		Kind:    "ActorTemplate",
	})
	if err := reader.Get(ctx, types.NamespacedName{Namespace: request.TemplateNamespace, Name: request.TemplateName}, template); err != nil {
		return nil, false
	}
	return template, true
}

func unsafeLiteralDurableSubstrateBootstrapEnv(template *unstructured.Unstructured) error {
	if literalSubstrateBootstrapEnvPresent(template) {
		return fmt.Errorf("%w: %s literal value is not allowed for durable substrate actors", errSubstrateUnsafeLiteralBootstrap, workerenv.WorkspaceBootstrapToken)
	}
	return nil
}

func substrateActorTemplateUnsafeLiteralBootstrapEnv(ctx context.Context, reader client.Reader, request *ExecutionWorkspaceRequest) error {
	template, ok := getSubstrateActorTemplateForUnsafeBootstrapCheck(ctx, reader, request)
	if !ok {
		return nil
	}
	return unsafeLiteralSubstrateBootstrapEnv(template, request)
}

func literalSubstrateBootstrapEnvPresent(template *unstructured.Unstructured) bool {
	containers, found, err := unstructured.NestedSlice(template.Object, "spec", "containers")
	if err != nil || !found {
		return false
	}
	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			continue
		}
		env, found, err := substrateContainerEnv(container)
		if err != nil || !found {
			continue
		}
		for _, envItem := range env {
			envVar, ok := envItem.(map[string]any)
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(envVar, "name")
			if name != workerenv.WorkspaceBootstrapToken {
				continue
			}
			value, valueFound, valueErr := unstructured.NestedString(envVar, "value")
			if valueErr == nil && valueFound && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func unsafeLiteralSubstrateBootstrapEnv(template *unstructured.Unstructured, request *ExecutionWorkspaceRequest) error {
	if literalSubstrateBootstrapEnvPresent(template) {
		return validateSubstrateLiteralBootstrapEnvAllowed(request)
	}
	return nil
}

func validateSubstratePoolPrecreateWorkspaceDaemonBootstrapEnv(template *unstructured.Unstructured, request *ExecutionWorkspaceRequest) error {
	containers, found, err := unstructured.NestedSlice(template.Object, "spec", "containers")
	if err != nil {
		return fmt.Errorf("has invalid spec.containers: %w", err)
	}
	if !found || len(containers) == 0 {
		return nil
	}
	parsed := make([]map[string]any, 0, len(containers))
	foundExplicitDaemon := false
	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("has invalid spec.containers entry")
		}
		parsed = append(parsed, container)
		runsDaemon, err := substrateContainerRunsWorkspaceDaemon(container)
		if err != nil {
			return err
		}
		if !runsDaemon {
			continue
		}
		foundExplicitDaemon = true
		if err := validateSubstrateWorkspaceDaemonBootstrapEnv(container, request); err != nil {
			return substrateWorkspaceDaemonContainerError(container, err)
		}
	}
	if foundExplicitDaemon || len(parsed) != 1 {
		return nil
	}
	hasBootstrapEnv, err := substrateContainerHasEnvName(parsed[0], workerenv.WorkspaceBootstrapToken)
	if err != nil {
		return err
	}
	if !hasBootstrapEnv {
		return nil
	}
	if err := validateSubstrateWorkspaceDaemonBootstrapEnv(parsed[0], request); err != nil {
		return substrateWorkspaceDaemonContainerError(parsed[0], err)
	}
	return nil
}

func substrateContainerHasEnvName(container map[string]any, name string) (bool, error) {
	env, found, err := substrateContainerEnv(container)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	for _, envItem := range env {
		envVar, ok := envItem.(map[string]any)
		if !ok {
			return false, fmt.Errorf("has invalid container env entry")
		}
		envName, _, _ := unstructured.NestedString(envVar, "name")
		if envName == name {
			return true, nil
		}
	}
	return false, nil
}

func validateSubstrateLiteralBootstrapEnvAllowed(request *ExecutionWorkspaceRequest) error {
	if request == nil {
		return nil
	}
	if request.CleanupPolicy == corev1alpha1.WorkspaceCleanupPolicyRetain {
		return fmt.Errorf("%w: %s literal value is not allowed with substrate cleanupPolicy=retain", errSubstrateUnsafeLiteralBootstrap, workerenv.WorkspaceBootstrapToken)
	}
	if request.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession || strings.TrimSpace(request.ReuseKey) != "" {
		return fmt.Errorf("%w: %s literal value is not allowed with substrate session reuse", errSubstrateUnsafeLiteralBootstrap, workerenv.WorkspaceBootstrapToken)
	}
	if request.ProcessMode == corev1alpha1.ExecutionWorkspaceProcessModeResident || strings.TrimSpace(request.ResidentKey) != "" {
		return fmt.Errorf("%w: %s literal value is not allowed with substrate resident workspaces", errSubstrateUnsafeLiteralBootstrap, workerenv.WorkspaceBootstrapToken)
	}
	if strings.TrimSpace(request.PoolName) != "" {
		return fmt.Errorf("%w: %s literal value is not allowed with substrate actor pools", errSubstrateUnsafeLiteralBootstrap, workerenv.WorkspaceBootstrapToken)
	}
	return nil
}

func isSubstrateUnsafeLiteralBootstrapError(err error) bool {
	return errors.Is(err, errSubstrateUnsafeLiteralBootstrap)
}

func substrateBootstrapEnvUsesConfiguredSecret(envVar map[string]any, request *ExecutionWorkspaceRequest) (bool, error) {
	secretName, secretNameFound, secretNameErr := unstructured.NestedString(envVar, "valueFrom", "secretKeyRef", "name")
	secretKey, secretKeyFound, secretKeyErr := unstructured.NestedString(envVar, "valueFrom", "secretKeyRef", "key")
	if secretNameErr != nil || secretKeyErr != nil {
		return false, fmt.Errorf("%s has invalid valueFrom.secretKeyRef", workerenv.WorkspaceBootstrapToken)
	}
	if !secretNameFound && !secretKeyFound {
		return false, nil
	}
	secretName = strings.TrimSpace(secretName)
	secretKey = strings.TrimSpace(secretKey)
	if secretName == "" || secretKey == "" {
		return false, fmt.Errorf("%s valueFrom.secretKeyRef must set name and key", workerenv.WorkspaceBootstrapToken)
	}
	if request != nil && strings.TrimSpace(request.SubstrateBootstrapSecretName) != "" &&
		secretName != strings.TrimSpace(request.SubstrateBootstrapSecretName) {
		return false, fmt.Errorf(
			"%s valueFrom.secretKeyRef must use configured bootstrap Secret %q",
			workerenv.WorkspaceBootstrapToken,
			request.SubstrateBootstrapSecretName,
		)
	}
	if request != nil && strings.TrimSpace(request.SubstrateBootstrapSecretKey) != "" &&
		secretKey != strings.TrimSpace(request.SubstrateBootstrapSecretKey) {
		return false, fmt.Errorf(
			"%s valueFrom.secretKeyRef must use configured bootstrap Secret key %q",
			workerenv.WorkspaceBootstrapToken,
			request.SubstrateBootstrapSecretKey,
		)
	}
	return true, nil
}
