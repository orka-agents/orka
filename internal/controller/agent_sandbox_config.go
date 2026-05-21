/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sandboxextv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
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
)

// AgentSandboxConfig holds disabled-by-default alpha configuration for agent sandbox workspace integration.
// The controller validates workspace requests and propagates resolved settings to agent worker Jobs;
// the worker wrapper performs the upstream sandbox claim, execution, and cleanup lifecycle.
type AgentSandboxConfig struct {
	// RouterURL is the optional base URL for an agent-sandbox router service.
	RouterURL string
	// DefaultTemplate is used when an enabled execution workspace omits templateRef.name.
	DefaultTemplate string
	// WarmPoolPolicy selects whether workspace claims may use warm pools.
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

	if err := r.validateExecutionWorkspaceTemplateExists(ctx, task, request); err != nil {
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
	reuseKey := ""
	claimName := deterministicSubstrateTaskActorID(string(task.UID), task.Status.Attempts+1)
	if reusePolicy == corev1alpha1.WorkspaceReusePolicySession && task.Spec.SessionRef != nil {
		reuseKey = task.Spec.SessionRef.Name
		claimName = deterministicSubstrateSessionActorID(task.Namespace, templateNamespace, templateName, reuseKey)
	}

	request := &ExecutionWorkspaceRequest{
		Provider:                       corev1alpha1.WorkspaceProviderSubstrate,
		TemplateName:                   templateName,
		TemplateNamespace:              templateNamespace,
		ClaimNamespace:                 templateNamespace,
		ClaimName:                      claimName,
		ReusePolicy:                    reusePolicy,
		ReuseKey:                       reuseKey,
		CleanupPolicy:                  cleanupPolicy,
		ClaimTimeout:                   cfg.ClaimTimeout,
		CommandTimeout:                 cfg.CommandTimeout,
		SubstrateAPIEndpoint:           cfg.APIEndpoint,
		SubstrateAPICAFile:             cfg.APICAFile,
		SubstrateAPIInsecureSkipVerify: cfg.APIInsecureSkipVerify,
		SubstrateRouterURL:             cfg.RouterURL,
		SubstrateActorDNSSuffix:        cfg.ActorDNSSuffix,
		SubstrateBootstrapSecretName:   cfg.BootstrapSecretName,
		SubstrateBootstrapSecretKey:    cfg.BootstrapSecretKey,
	}

	if err := r.validateSubstrateWorkspaceTemplate(ctx, task, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (r *TaskReconciler) validateExecutionWorkspaceTemplateExists(ctx context.Context, task *corev1alpha1.Task, request *AgentSandboxWorkspaceRequest) error {
	if r == nil || r.Client == nil || request == nil || request.TemplateName == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// The upstream agent-sandbox SDK accepts only template name plus claim namespace,
	// so validate the effective namespace where the SandboxClaim will be created.
	lookupNamespace := request.ClaimNamespace
	if strings.TrimSpace(lookupNamespace) == "" {
		lookupNamespace = request.TemplateNamespace
	}
	if strings.TrimSpace(lookupNamespace) == "" {
		lookupNamespace = task.Namespace
	}

	template := &sandboxextv1alpha1.SandboxTemplate{}
	err := r.Get(ctx, types.NamespacedName{Namespace: lookupNamespace, Name: request.TemplateName}, template)
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return fmt.Errorf(
			"execution workspace template %q not found in namespace %q",
			request.TemplateName,
			lookupNamespace,
		)
	}
	return fmt.Errorf(
		"failed to validate execution workspace template %q in namespace %q: %w",
		request.TemplateName,
		lookupNamespace,
		err,
	)
}

func (r *TaskReconciler) validateSubstrateWorkspaceTemplate(ctx context.Context, task *corev1alpha1.Task, request *ExecutionWorkspaceRequest) error {
	if r == nil || r.Client == nil || request == nil || request.TemplateName == "" {
		return nil
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
	err := r.Get(ctx, types.NamespacedName{Namespace: request.TemplateNamespace, Name: request.TemplateName}, template)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf(
				"substrate execution workspace ActorTemplate %q not found in namespace %q",
				request.TemplateName,
				request.TemplateNamespace,
			)
		}
		return fmt.Errorf(
			"failed to validate substrate execution workspace ActorTemplate %q in namespace %q: %w",
			request.TemplateName,
			request.TemplateNamespace,
			err,
		)
	}

	labels := template.GetLabels()
	annotations := template.GetAnnotations()
	if labels["orka.ai/execution-workspace"] != "true" {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q missing label orka.ai/execution-workspace=true", request.TemplateName, request.TemplateNamespace)
	}
	if labels["orka.ai/workspace-provider"] != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q missing label orka.ai/workspace-provider=substrate", request.TemplateName, request.TemplateNamespace)
	}
	if annotations["orka.ai/workspace-protocol"] != "http-json-v1" {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q missing annotation orka.ai/workspace-protocol=http-json-v1", request.TemplateName, request.TemplateNamespace)
	}
	if strings.TrimSpace(annotations["orka.ai/workspace-daemon-port"]) == "" {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q missing annotation orka.ai/workspace-daemon-port", request.TemplateName, request.TemplateNamespace)
	}
	stagingRoot := strings.TrimRight(strings.TrimSpace(annotations["orka.ai/workspace-staging-root"]), "/")
	if stagingRoot == "" {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q missing annotation orka.ai/workspace-staging-root", request.TemplateName, request.TemplateNamespace)
	}
	if stagingRoot != substrateWorkspaceStagingRoot {
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q must set annotation orka.ai/workspace-staging-root=%s", request.TemplateName, request.TemplateNamespace, substrateWorkspaceStagingRoot)
	}
	phase, _, _ := unstructured.NestedString(template.Object, "status", "phase")
	phase = strings.TrimSpace(phase)
	if phase != "Ready" {
		if phase == "" {
			phase = "<empty>"
		}
		return fmt.Errorf("substrate ActorTemplate %q in namespace %q is not Ready: phase=%s", request.TemplateName, request.TemplateNamespace, phase)
	}
	_ = task
	return nil
}
