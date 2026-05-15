/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"fmt"
	"time"

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
)

// AgentSandboxConfig holds disabled-by-default alpha configuration for agent sandbox workspace integration.
// The controller only validates workspace requests while the feature gate is enabled; no lifecycle
// execution is wired through this config yet.
type AgentSandboxConfig struct {
	// RouterURL is the optional base URL for an agent-sandbox router service.
	RouterURL string
	// DefaultTemplate is used when an enabled execution workspace omits templateRef.name.
	DefaultTemplate string
	// WarmPoolPolicy selects whether future workspace claims may use warm pools.
	WarmPoolPolicy string
	// NamespaceStrategy selects where future sandbox lifecycle resources will be managed.
	NamespaceStrategy string
	// ClaimTimeout bounds future workspace claim/attach operations.
	ClaimTimeout time.Duration
	// CommandTimeout bounds future sandbox command execution operations.
	CommandTimeout time.Duration
	// CleanupPolicy is the controller default when a Task workspace omits cleanupPolicy.
	CleanupPolicy corev1alpha1.WorkspaceCleanupPolicy
}

// DefaultAgentSandboxConfig returns safe alpha defaults. The empty RouterURL and DefaultTemplate keep
// sandbox lifecycle integration inert until explicitly configured and enabled.
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

// AgentSandboxWorkspaceRequest is the controller's resolved, validated view of a Task execution
// workspace request. It is scaffolding for future sandbox lifecycle execution and is intentionally
// not consumed by JobBuilder yet.
type AgentSandboxWorkspaceRequest struct {
	RouterURL         string
	TemplateName      string
	TemplateNamespace string
	ReusePolicy       corev1alpha1.WorkspaceReusePolicy
	ReuseKey          string
	CleanupPolicy     corev1alpha1.WorkspaceCleanupPolicy
	WarmPoolPolicy    string
	NamespaceStrategy string
	ClaimTimeout      time.Duration
	CommandTimeout    time.Duration
}

func executionWorkspaceTemplateName(ws *corev1alpha1.ExecutionWorkspaceSpec, cfg AgentSandboxConfig) string {
	if ws != nil && ws.TemplateRef != nil && ws.TemplateRef.Name != "" {
		return ws.TemplateRef.Name
	}
	return cfg.WithDefaults().DefaultTemplate
}

func executionWorkspaceTemplateNamespace(ws *corev1alpha1.ExecutionWorkspaceSpec, taskNamespace string) string {
	if ws != nil && ws.TemplateRef != nil && ws.TemplateRef.Namespace != "" {
		return ws.TemplateRef.Namespace
	}
	return taskNamespace
}

// resolveExecutionWorkspaceRequest applies controller defaults to a Task execution workspace request.
// It returns nil when the Task does not request an enabled execution workspace.
func (r *TaskReconciler) resolveExecutionWorkspaceRequest(task *corev1alpha1.Task) (*AgentSandboxWorkspaceRequest, error) {
	if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil || !task.Spec.Execution.Workspace.Enabled {
		return nil, nil
	}
	if err := r.validateExecutionWorkspace(task); err != nil {
		return nil, err
	}

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

	request := &AgentSandboxWorkspaceRequest{
		RouterURL:         cfg.RouterURL,
		TemplateName:      executionWorkspaceTemplateName(ws, cfg),
		TemplateNamespace: executionWorkspaceTemplateNamespace(ws, task.Namespace),
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

	return request, nil
}
