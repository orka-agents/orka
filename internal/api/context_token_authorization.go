/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/workerenv"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// ContextTokenAuthorizationModeOff disables context-token authorization checks.
	ContextTokenAuthorizationModeOff = "off"
	// ContextTokenAuthorizationModeAudit logs context-token authorization failures but allows the request.
	ContextTokenAuthorizationModeAudit = "audit"
	// ContextTokenAuthorizationModeEnforce rejects requests that fail context-token authorization.
	ContextTokenAuthorizationModeEnforce = "enforce"

	// ContextTokenScopeTaskCreate authorizes context-token callers to create Orka Tasks.
	ContextTokenScopeTaskCreate = "orka:tasks:create"
	// ContextTokenScopeTaskGet authorizes context-token callers to read a Task and its related data.
	ContextTokenScopeTaskGet = "orka:tasks:get"
	// ContextTokenScopeTaskList authorizes context-token callers to list Tasks.
	ContextTokenScopeTaskList = "orka:tasks:list"
	// ContextTokenScopeTaskDelete authorizes context-token callers to delete Tasks.
	ContextTokenScopeTaskDelete = "orka:tasks:delete"
	// ContextTokenScopeToolsRead authorizes context-token callers to read Tool definitions.
	ContextTokenScopeToolsRead = "orka:tools:read"
	// ContextTokenScopeToolsUse authorizes context-token callers to execute Orka-managed tools.
	ContextTokenScopeToolsUse = "orka:tools:use"
	// ContextTokenScopeProvidersUse authorizes context-token callers to use configured model providers.
	ContextTokenScopeProvidersUse = "orka:providers:use"
	// ContextTokenScopeSecretsRead authorizes context-token callers to read Secret metadata.
	ContextTokenScopeSecretsRead = "orka:secrets:read"
	// ContextTokenScopeAgentsRead authorizes context-token callers to read Agent definitions.
	ContextTokenScopeAgentsRead = "orka:agents:read"
	// ContextTokenScopeAgentsWrite authorizes context-token callers to mutate Agent definitions.
	ContextTokenScopeAgentsWrite = "orka:agents:write"
	// ContextTokenScopeMemoryRead authorizes context-token callers to read memory resources.
	ContextTokenScopeMemoryRead = "orka:memory:read"
	// ContextTokenScopeMemoryWrite authorizes context-token callers to mutate memory resources.
	ContextTokenScopeMemoryWrite = "orka:memory:write"
	// ContextTokenScopeSessionsRead authorizes context-token callers to read sessions.
	ContextTokenScopeSessionsRead = "orka:sessions:read"
	// ContextTokenScopeSessionsWrite authorizes context-token callers to delete or mutate sessions.
	ContextTokenScopeSessionsWrite = "orka:sessions:write"
	// ContextTokenScopeSecurityRead authorizes context-token callers to read security scan resources.
	ContextTokenScopeSecurityRead = "orka:security:read"
	// ContextTokenScopeSecurityWrite authorizes context-token callers to mutate security scan resources.
	ContextTokenScopeSecurityWrite = "orka:security:write"
	// ContextTokenScopeSkillsRead authorizes context-token callers to read Skills.
	ContextTokenScopeSkillsRead = "orka:skills:read"
	// ContextTokenScopeSkillsWrite authorizes context-token callers to mutate Skills.
	ContextTokenScopeSkillsWrite = "orka:skills:write"
)

// ContextTokenAuthorizationConfig controls optional authorization checks derived
// from verified context-token scope and transaction context claims.
type ContextTokenAuthorizationConfig struct {
	Mode                string
	TaskCreateScopes    []string
	TaskReadScopes      []string
	TaskListScopes      []string
	TaskDeleteScopes    []string
	ToolReadScopes      []string
	ToolUseScopes       []string
	ProviderUseScopes   []string
	SecretReadScopeList []string
	AgentReadScopes     []string
	AgentWriteScopes    []string
	MemoryReadScopes    []string
	MemoryWriteScopes   []string
	SessionReadScopes   []string
	SessionWriteScopes  []string
	SecurityReadScopes  []string
	SecurityWriteScopes []string
	SkillReadScopes     []string
	SkillWriteScopes    []string
}

// ContextTokenAuthorizationConfigOptions names the inputs used to build
// context-token authorization config.
type ContextTokenAuthorizationConfigOptions struct {
	Mode                string
	TaskCreateScopes    string
	TaskReadScopes      string
	TaskListScopes      string
	TaskDeleteScopes    string
	ToolReadScopes      string
	ToolUseScopes       string
	ProviderUseScopes   string
	SecretReadScopes    string
	AgentReadScopes     string
	AgentWriteScopes    string
	MemoryReadScopes    string
	MemoryWriteScopes   string
	SessionReadScopes   string
	SessionWriteScopes  string
	SecurityReadScopes  string
	SecurityWriteScopes string
	SkillReadScopes     string
	SkillWriteScopes    string
}

// NewContextTokenAuthorizationConfig builds context-token authorization config.
func NewContextTokenAuthorizationConfig(opts ContextTokenAuthorizationConfigOptions) (ContextTokenAuthorizationConfig, error) {
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = ContextTokenAuthorizationModeOff
	}
	switch mode {
	case ContextTokenAuthorizationModeOff, ContextTokenAuthorizationModeAudit, ContextTokenAuthorizationModeEnforce:
	default:
		return ContextTokenAuthorizationConfig{}, fmt.Errorf("unsupported context-token authorization mode %q", mode)
	}

	createScopes := defaultScopes(opts.TaskCreateScopes, ContextTokenScopeTaskCreate)
	readScopes := defaultScopes(opts.TaskReadScopes, ContextTokenScopeTaskGet)
	listScopes := defaultScopes(opts.TaskListScopes, ContextTokenScopeTaskList)
	deleteScopes := defaultScopes(opts.TaskDeleteScopes, ContextTokenScopeTaskDelete)
	toolRead := defaultScopes(opts.ToolReadScopes, ContextTokenScopeToolsRead)
	toolUse := defaultScopes(opts.ToolUseScopes, ContextTokenScopeToolsUse)
	providerUse := defaultScopes(opts.ProviderUseScopes, ContextTokenScopeProvidersUse)
	secretRead := defaultScopes(opts.SecretReadScopes, ContextTokenScopeSecretsRead)
	agentRead := defaultScopes(opts.AgentReadScopes, ContextTokenScopeAgentsRead)
	agentWrite := defaultScopes(opts.AgentWriteScopes, ContextTokenScopeAgentsWrite)
	memoryRead := defaultScopes(opts.MemoryReadScopes, ContextTokenScopeMemoryRead)
	memoryWrite := defaultScopes(opts.MemoryWriteScopes, ContextTokenScopeMemoryWrite)
	sessionRead := defaultScopes(opts.SessionReadScopes, ContextTokenScopeSessionsRead)
	sessionWrite := defaultScopes(opts.SessionWriteScopes, ContextTokenScopeSessionsWrite)
	securityRead := defaultScopes(opts.SecurityReadScopes, ContextTokenScopeSecurityRead)
	securityWrite := defaultScopes(opts.SecurityWriteScopes, ContextTokenScopeSecurityWrite)
	skillRead := defaultScopes(opts.SkillReadScopes, ContextTokenScopeSkillsRead)
	skillWrite := defaultScopes(opts.SkillWriteScopes, ContextTokenScopeSkillsWrite)
	return ContextTokenAuthorizationConfig{
		Mode:                mode,
		TaskCreateScopes:    createScopes,
		TaskReadScopes:      readScopes,
		TaskListScopes:      listScopes,
		TaskDeleteScopes:    deleteScopes,
		ToolReadScopes:      toolRead,
		ToolUseScopes:       toolUse,
		ProviderUseScopes:   providerUse,
		SecretReadScopeList: secretRead,
		AgentReadScopes:     agentRead,
		AgentWriteScopes:    agentWrite,
		MemoryReadScopes:    memoryRead,
		MemoryWriteScopes:   memoryWrite,
		SessionReadScopes:   sessionRead,
		SessionWriteScopes:  sessionWrite,
		SecurityReadScopes:  securityRead,
		SecurityWriteScopes: securityWrite,
		SkillReadScopes:     skillRead,
		SkillWriteScopes:    skillWrite,
	}, nil
}

// Enabled reports whether context-token authorization is configured.
func (c ContextTokenAuthorizationConfig) Enabled() bool {
	return c.Mode == ContextTokenAuthorizationModeAudit || c.Mode == ContextTokenAuthorizationModeEnforce
}

func (c ContextTokenAuthorizationConfig) enforcing() bool {
	return c.Mode == ContextTokenAuthorizationModeEnforce
}

func (c ContextTokenAuthorizationConfig) SecretReadScopes() []string {
	return c.SecretReadScopeList
}

type contextTokenTaskCreateAuthorizationContext struct {
	Request             CreateTaskRequest
	Namespace           string
	Agent               *corev1alpha1.Agent
	AgentName           string
	AgentNamespace      string
	Provider            *corev1alpha1.Provider
	ProviderRef         ProviderResolutionInfo
	EffectiveProvider   ProviderResolutionInfo
	EffectiveModel      string
	EffectiveAITools    []string
	RuntimeAllowedTools []string
}

func defaultScopes(value, fallback string) []string {
	if scopes := workerenv.SplitCSV(value); len(scopes) > 0 {
		return scopes
	}
	return []string{fallback}
}

func (h *Handlers) authorizeContextTokenTaskCreate(c fiber.Ctx, req CreateTaskRequest, namespace string) error {
	if !h.contextTokenAuthorization.Enabled() {
		return nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil
	}

	authzCtx, err := h.resolveContextTokenTaskCreateAuthorizationContext(c.Context(), req, namespace)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}

	failures := contextTokenTaskCreateFailures(ui.ContextToken, h.contextTokenAuthorization, authzCtx)
	if len(failures) == 0 {
		metrics.RecordContextTokenAuthorization("createTask", "allowed", "ok")
		return nil
	}

	return h.handleContextTokenAuthorizationFailures(ui.ContextToken, "createTask", failures)
}

func (h *Handlers) authorizeContextTokenAction(c fiber.Ctx, action string, requiredScopes []string) error {
	return authorizeContextTokenActionWithConfig(c, h.contextTokenAuthorization, action, requiredScopes)
}

func (h *Handlers) handleContextTokenAuthorizationFailures(token *ContextToken, action string, failures []string) error {
	return handleContextTokenAuthorizationFailures(h.contextTokenAuthorization, token, action, failures)
}

func authorizeContextTokenActionWithConfig(c fiber.Ctx, cfg ContextTokenAuthorizationConfig, action string, requiredScopes []string) error {
	if !cfg.Enabled() {
		return nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil
	}

	failures := []string{}
	if !hasAnyScope(ui.ContextToken.Scopes, requiredScopes) {
		failures = append(failures, fmt.Sprintf("missing one of required scopes %q", strings.Join(requiredScopes, ",")))
	}
	if want, ok := contextString(ui.ContextToken.TransactionContext, "namespace"); ok {
		if got, ok := c.Locals(resolvedNamespaceLocalKey).(string); ok && strings.TrimSpace(got) != "" && got != want {
			failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", got, want))
		}
	}
	if len(failures) == 0 {
		metrics.RecordContextTokenAuthorization(action, "allowed", "ok")
		return nil
	}
	return handleContextTokenAuthorizationFailures(cfg, ui.ContextToken, action, failures)
}

func authorizeContextTokenProviderUse(c fiber.Ctx, cfg ContextTokenAuthorizationConfig, action, namespace string, provider ProviderResolutionInfo, model string) error {
	if !cfg.Enabled() {
		return nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil
	}

	failures := contextTokenProviderUseFailures(ui.ContextToken, cfg, namespace, provider, model)
	if len(failures) == 0 {
		metrics.RecordContextTokenAuthorization(action, "allowed", "ok")
		return nil
	}
	return handleContextTokenAuthorizationFailures(cfg, ui.ContextToken, action, failures)
}

func contextTokenAllowsListedProviderModel(c fiber.Ctx, cfg ContextTokenAuthorizationConfig, action, namespace string, provider ProviderResolutionInfo, model string) bool {
	if !cfg.Enabled() {
		return true
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return true
	}

	failures := contextTokenProviderUseFailures(ui.ContextToken, cfg, namespace, provider, model)
	if len(failures) == 0 {
		return true
	}
	if cfg.enforcing() {
		return false
	}
	_ = handleContextTokenAuthorizationFailures(cfg, ui.ContextToken, action, failures)
	return true
}

func authorizeContextTokenToolUse(c fiber.Ctx, cfg ContextTokenAuthorizationConfig, action string, toolNames []string) error {
	if !cfg.Enabled() {
		return nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil
	}

	failures := []string{}
	if !hasAnyScope(ui.ContextToken.Scopes, cfg.ToolUseScopes) {
		failures = append(failures, fmt.Sprintf("missing one of required scopes %q", strings.Join(cfg.ToolUseScopes, ",")))
	}
	if allowed, ok := contextStringList(ui.ContextToken.TransactionContext, "allowedTools"); ok && !toolNamesAllowed(toolNames, allowed) {
		failures = append(failures, "one or more tools are not allowed by token context")
	}
	if len(failures) == 0 {
		metrics.RecordContextTokenAuthorization(action, "allowed", "ok")
		return nil
	}
	return handleContextTokenAuthorizationFailures(cfg, ui.ContextToken, action, failures)
}

func handleContextTokenAuthorizationFailures(cfg ContextTokenAuthorizationConfig, token *ContextToken, action string, failures []string) error {
	result := "audit"
	if cfg.enforcing() {
		result = "denied"
	}
	metrics.RecordContextTokenAuthorization(action, result, contextTokenAuthorizationFailureReason(failures))

	log.Info("context-token authorization failed",
		"mode", cfg.Mode,
		"action", action,
		"transactionID", token.TransactionID,
		"subject", token.Subject,
		"issuer", token.Issuer,
		"failures", strings.Join(failures, "; "),
	)
	if cfg.enforcing() {
		return fiber.NewError(fiber.StatusForbidden, "context token is not authorized for "+action)
	}
	return nil
}

func contextTokenAuthorizationFailureReason(failures []string) string {
	if len(failures) == 0 {
		return "unknown"
	}
	joined := strings.ToLower(strings.Join(failures, "; "))
	switch {
	case strings.Contains(joined, "missing one of required scopes"):
		return "missing_scope"
	case strings.Contains(joined, "namespace"):
		return "namespace_mismatch"
	case strings.Contains(joined, "agent"):
		return "agent_mismatch"
	case strings.Contains(joined, "workspace repo") || strings.Contains(joined, "repository"):
		return "repo_mismatch"
	case strings.Contains(joined, "workspace branch"):
		return "branch_mismatch"
	case strings.Contains(joined, "workspace ref"):
		return "ref_mismatch"
	case strings.Contains(joined, "provider"):
		return "provider_mismatch"
	case strings.Contains(joined, "model"):
		return "model_mismatch"
	case strings.Contains(joined, "tool"):
		return "tool_not_allowed"
	default:
		return "context_violation"
	}
}

func contextTokenProviderUseFailures(token *ContextToken, cfg ContextTokenAuthorizationConfig, namespace string, provider ProviderResolutionInfo, model string) []string {
	failures := []string{}
	if !hasAnyScope(token.Scopes, cfg.ProviderUseScopes) {
		failures = append(failures, fmt.Sprintf("missing one of required scopes %q", strings.Join(cfg.ProviderUseScopes, ",")))
	}

	tokenNamespace, hasTokenNamespace := contextString(token.TransactionContext, "namespace")
	if hasTokenNamespace {
		if namespace != tokenNamespace {
			failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", namespace, tokenNamespace))
		}
		if !providerNamespaceMatchesContext(provider, tokenNamespace, hasTokenNamespace) {
			failures = append(failures, fmt.Sprintf("provider namespace %q does not match token context %q", provider.Namespace, tokenNamespace))
		}
	}
	if want, ok := contextString(token.TransactionContext, "provider"); ok && !providerMatches(provider, want, tokenNamespace, hasTokenNamespace) {
		failures = append(failures, fmt.Sprintf("provider %q is not allowed by token context", provider.Name))
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedProviders"); ok && !providerAllowed(provider, allowed, tokenNamespace, hasTokenNamespace) {
		failures = append(failures, fmt.Sprintf("provider %q is not allowed by token context", provider.Name))
	}
	if want, ok := contextString(token.TransactionContext, "model"); ok && model != want {
		failures = append(failures, fmt.Sprintf("model %q does not match token context %q", model, want))
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedModels"); ok && !modelAllowed(provider, model, allowed, tokenNamespace, hasTokenNamespace) {
		failures = append(failures, fmt.Sprintf("model %q is not allowed by token context", model))
	}

	return failures
}

func (h *Handlers) resolveContextTokenTaskCreateAuthorizationContext(ctx context.Context, req CreateTaskRequest, namespace string) (contextTokenTaskCreateAuthorizationContext, error) {
	authzCtx := contextTokenTaskCreateAuthorizationContext{
		Request:   req,
		Namespace: namespace,
	}

	if req.AgentRef != nil {
		authzCtx.AgentName = req.AgentRef.Name
		authzCtx.AgentNamespace = req.AgentRef.Namespace
		if authzCtx.AgentNamespace == "" {
			authzCtx.AgentNamespace = namespace
		}

		if authzCtx.AgentName != "" {
			agent := &corev1alpha1.Agent{}
			key := types.NamespacedName{Name: authzCtx.AgentName, Namespace: authzCtx.AgentNamespace}
			if err := h.client.Get(ctx, key, agent); err != nil {
				if !apierrors.IsNotFound(err) {
					return authzCtx, fmt.Errorf("resolve agent %q in namespace %q: %w", authzCtx.AgentName, authzCtx.AgentNamespace, err)
				}
			} else {
				authzCtx.Agent = agent
			}
		}
	}

	providerRef := contextTokenTaskCreateProviderRef(req, authzCtx.Agent)
	if providerRef != nil && strings.TrimSpace(providerRef.Name) != "" {
		providerNamespace := providerRef.Namespace
		if providerNamespace == "" {
			providerNamespace = namespace
		}
		authzCtx.ProviderRef = ProviderResolutionInfo{Name: providerRef.Name, Namespace: providerNamespace}
		provider := &corev1alpha1.Provider{}
		key := types.NamespacedName{Name: providerRef.Name, Namespace: providerNamespace}
		if err := h.client.Get(ctx, key, provider); err != nil {
			if !apierrors.IsNotFound(err) {
				return authzCtx, fmt.Errorf("resolve provider %q in namespace %q: %w", providerRef.Name, providerNamespace, err)
			}
		} else {
			authzCtx.Provider = provider
		}
	}

	authzCtx.EffectiveProvider, authzCtx.EffectiveModel = contextTokenTaskCreateEffectiveProviderModel(req, authzCtx.Agent, authzCtx.Provider)
	authzCtx.EffectiveAITools = contextTokenTaskCreateEffectiveAITools(req, authzCtx.Agent)
	authzCtx.RuntimeAllowedTools = contextTokenTaskCreateEffectiveRuntimeAllowedTools(req, authzCtx.Agent)

	return authzCtx, nil
}

func contextTokenTaskCreateProviderRef(req CreateTaskRequest, agent *corev1alpha1.Agent) *corev1alpha1.ProviderReference {
	if req.AI != nil && req.AI.ProviderRef != nil {
		return req.AI.ProviderRef
	}
	if agent != nil && agent.Spec.ProviderRef != nil {
		return agent.Spec.ProviderRef
	}
	return nil
}

func contextTokenTaskCreateEffectiveProviderModel(req CreateTaskRequest, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) (ProviderResolutionInfo, string) {
	providerInfo := ProviderResolutionInfo{}
	model := ""

	if provider != nil {
		providerInfo = providerResolutionInfo(provider)
		model = provider.Spec.DefaultModel
	}

	if agent != nil && agent.Spec.Model != nil {
		if strings.TrimSpace(agent.Spec.Model.Provider) != "" {
			providerInfo = ProviderResolutionInfo{Type: agent.Spec.Model.Provider}
		}
		if strings.TrimSpace(agent.Spec.Model.Name) != "" {
			model = agent.Spec.Model.Name
		}
	}

	if req.AI != nil {
		if strings.TrimSpace(req.AI.Provider) != "" {
			providerInfo = ProviderResolutionInfo{Type: req.AI.Provider}
		}
		if strings.TrimSpace(req.AI.Model) != "" {
			model = req.AI.Model
		}
	}

	// Provider CRD type is authoritative when a ProviderRef resolves; direct provider
	// strings on the task or agent must not override the loaded Provider type.
	if provider != nil {
		providerInfo = providerResolutionInfo(provider)
	}

	return providerInfo, model
}

func contextTokenTaskCreateEffectiveAITools(req CreateTaskRequest, agent *corev1alpha1.Agent) []string {
	tools := []string{}
	if agent != nil {
		for _, tool := range agent.Spec.Tools {
			if tool.Enabled != nil && !*tool.Enabled {
				continue
			}
			if strings.TrimSpace(tool.Name) != "" {
				tools = append(tools, tool.Name)
			}
		}
	}
	if req.AI != nil {
		for _, tool := range req.AI.Tools {
			if strings.TrimSpace(tool) != "" {
				tools = append(tools, tool)
			}
		}
	}
	return tools
}

func contextTokenTaskCreateEffectiveRuntimeAllowedTools(req CreateTaskRequest, agent *corev1alpha1.Agent) []string {
	if req.AgentRuntime != nil && len(req.AgentRuntime.AllowedTools) > 0 {
		return append([]string{}, req.AgentRuntime.AllowedTools...)
	}
	if agent != nil && agent.Spec.Runtime != nil && len(agent.Spec.Runtime.DefaultAllowedTools) > 0 {
		return append([]string{}, agent.Spec.Runtime.DefaultAllowedTools...)
	}
	return nil
}

func contextTokenTaskCreateFailures(token *ContextToken, cfg ContextTokenAuthorizationConfig, authzCtx contextTokenTaskCreateAuthorizationContext) []string {
	failures := []string{}
	req := authzCtx.Request

	if !hasAnyScope(token.Scopes, cfg.TaskCreateScopes) {
		failures = append(failures, fmt.Sprintf("missing one of required scopes %q", strings.Join(cfg.TaskCreateScopes, ",")))
	}

	tokenNamespace, hasTokenNamespace := contextString(token.TransactionContext, "namespace")
	if hasTokenNamespace {
		failures = append(failures, contextTokenTaskCreateNamespaceFailures(authzCtx, tokenNamespace)...)
	}
	if want, ok := contextString(token.TransactionContext, "taskType"); ok && string(req.Type) != want {
		failures = append(failures, fmt.Sprintf("task type %q does not match token context %q", req.Type, want))
	}
	if want, ok := contextString(token.TransactionContext, "agent"); ok {
		if !agentMatches(authzCtx.AgentName, authzCtx.AgentNamespace, want) {
			failures = append(failures, fmt.Sprintf("agent %q does not match token context %q", namespacedNameString(authzCtx.AgentNamespace, authzCtx.AgentName), want))
		}
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedAgents"); ok && authzCtx.AgentName != "" && !agentAllowed(authzCtx.AgentName, authzCtx.AgentNamespace, allowed) {
		failures = append(failures, fmt.Sprintf("agent %q is not allowed by token context", namespacedNameString(authzCtx.AgentNamespace, authzCtx.AgentName)))
	}
	if want, ok := contextString(token.TransactionContext, "provider"); ok && !providerMatches(authzCtx.EffectiveProvider, want, tokenNamespace, hasTokenNamespace) {
		failures = append(failures, fmt.Sprintf("provider %q is not allowed by token context", providerDisplayName(authzCtx.EffectiveProvider)))
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedProviders"); ok && !providerAllowed(authzCtx.EffectiveProvider, allowed, tokenNamespace, hasTokenNamespace) {
		failures = append(failures, fmt.Sprintf("provider %q is not allowed by token context", providerDisplayName(authzCtx.EffectiveProvider)))
	}
	if want, ok := contextString(token.TransactionContext, "model"); ok && authzCtx.EffectiveModel != want {
		failures = append(failures, fmt.Sprintf("model %q does not match token context %q", authzCtx.EffectiveModel, want))
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedModels"); ok && !modelAllowed(authzCtx.EffectiveProvider, authzCtx.EffectiveModel, allowed, tokenNamespace, hasTokenNamespace) {
		failures = append(failures, fmt.Sprintf("model %q is not allowed by token context", authzCtx.EffectiveModel))
	}

	workspace := taskRequestWorkspace(req)
	for _, constraint := range []struct {
		key string
		got string
	}{
		{key: "repo", got: workspaceGitRepo(workspace)},
		{key: "branch", got: workspaceBranch(workspace)},
		{key: "ref", got: workspaceRef(workspace)},
	} {
		if want, ok := contextString(token.TransactionContext, constraint.key); ok && constraint.got != want {
			failures = append(failures, fmt.Sprintf("workspace %s %q does not match token context %q", constraint.key, constraint.got, want))
		}
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedTools"); ok {
		for _, tool := range append(append([]string{}, authzCtx.EffectiveAITools...), authzCtx.RuntimeAllowedTools...) {
			if strings.TrimSpace(tool) == "" {
				continue
			}
			if !slices.Contains(allowed, tool) {
				failures = append(failures, fmt.Sprintf("tool %q is not allowed by token context", tool))
			}
		}
	}

	return failures
}

func contextTokenTaskCreateNamespaceFailures(authzCtx contextTokenTaskCreateAuthorizationContext, tokenNamespace string) []string {
	failures := []string{}
	if authzCtx.Namespace != tokenNamespace {
		failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", authzCtx.Namespace, tokenNamespace))
	}
	if authzCtx.AgentName != "" && authzCtx.AgentNamespace != "" && authzCtx.AgentNamespace != tokenNamespace {
		failures = append(failures, fmt.Sprintf("agent namespace %q does not match token context %q", authzCtx.AgentNamespace, tokenNamespace))
	}

	providerNamespaceInfo := authzCtx.EffectiveProvider
	if authzCtx.ProviderRef.Name != "" {
		providerNamespaceInfo = authzCtx.ProviderRef
	}
	if !providerNamespaceMatchesContext(providerNamespaceInfo, tokenNamespace, true) {
		failures = append(failures, fmt.Sprintf("provider namespace %q does not match token context %q", providerNamespaceInfo.Namespace, tokenNamespace))
	}

	return failures
}

func filterCompletionToolsForContextToken(c fiber.Ctx, cfg ContextTokenAuthorizationConfig, tools []llm.Tool) []llm.Tool {
	if !cfg.Enabled() {
		return tools
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return tools
	}

	allowed, ok := contextStringList(ui.ContextToken.TransactionContext, "allowedTools")
	if !ok {
		return tools
	}
	return filterCompletionToolsByName(tools, allowed)
}

func filterCompletionToolsByName(tools []llm.Tool, allowed []string) []llm.Tool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name != "" {
			allowedSet[name] = struct{}{}
		}
	}

	filtered := make([]llm.Tool, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		if _, ok := allowedSet[name]; ok {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func completionToolNames(tools []llm.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) != "" {
			names = append(names, tool.Name)
		}
	}
	return names
}

func completionToolNameSet(tools []llm.Tool) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

func openAIContextTokenAuthorizationError(c fiber.Ctx, err error) error {
	if err == nil {
		return nil
	}
	if ferr, ok := err.(*fiber.Error); ok && ferr.Code == fiber.StatusForbidden {
		return c.Status(fiber.StatusForbidden).JSON(OAIError{Error: OAIErrorDetail{
			Message: ferr.Message,
			Type:    "permission_error",
		}})
	}
	return err
}

func anthropicContextTokenAuthorizationError(c fiber.Ctx, err error) error {
	if err == nil {
		return nil
	}
	if ferr, ok := err.(*fiber.Error); ok && ferr.Code == fiber.StatusForbidden {
		return anthropicError(c, fiber.StatusForbidden, "permission_error", ferr.Message)
	}
	return err
}

func agentAllowed(name, namespace string, allowed []string) bool {
	for _, want := range allowed {
		if agentMatches(name, namespace, want) {
			return true
		}
	}
	return false
}

func agentMatches(name, namespace, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" || strings.TrimSpace(name) == "" {
		return false
	}
	return name == want || namespacedNameString(namespace, name) == want
}

func namespacedNameString(namespace, name string) string {
	if namespace == "" {
		return name
	}
	if name == "" {
		return ""
	}
	return namespace + "/" + name
}

func providerDisplayName(provider ProviderResolutionInfo) string {
	if provider.Name != "" {
		return namespacedNameString(provider.Namespace, provider.Name)
	}
	return provider.Type
}

func providerAllowed(provider ProviderResolutionInfo, allowed []string, tokenNamespace string, hasTokenNamespace bool) bool {
	for _, want := range allowed {
		if providerMatches(provider, want, tokenNamespace, hasTokenNamespace) {
			return true
		}
	}
	return false
}

func providerMatches(provider ProviderResolutionInfo, want string, tokenNamespace string, hasTokenNamespace bool) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	if !providerNamespaceMatchesContext(provider, tokenNamespace, hasTokenNamespace) {
		return false
	}
	if provider.Name != "" && namespacedNameString(provider.Namespace, provider.Name) == want {
		return true
	}
	if provider.Name != "" && provider.Name == want {
		return true
	}
	return provider.Type != "" && provider.Type == want
}

func modelAllowed(provider ProviderResolutionInfo, model string, allowed []string, tokenNamespace string, hasTokenNamespace bool) bool {
	if !providerNamespaceMatchesContext(provider, tokenNamespace, hasTokenNamespace) {
		return false
	}
	for _, want := range allowed {
		want = strings.TrimSpace(want)
		switch want {
		case "":
			continue
		case model:
			return true
		}
		if provider.Name != "" && want == provider.Name+"/"+model {
			return true
		}
		if provider.Name != "" && want == namespacedNameString(provider.Namespace, provider.Name)+"/"+model {
			return true
		}
		if provider.Type != "" && want == provider.Type+"/"+model {
			return true
		}
	}
	return false
}

func providerNamespaceMatchesContext(provider ProviderResolutionInfo, tokenNamespace string, hasTokenNamespace bool) bool {
	if !hasTokenNamespace {
		return true
	}
	providerNamespace := strings.TrimSpace(provider.Namespace)
	return providerNamespace == "" || providerNamespace == tokenNamespace
}

func toolNamesAllowed(tools []string, allowed []string) bool {
	for _, tool := range tools {
		if tool == "" {
			continue
		}
		if !slices.Contains(allowed, tool) {
			return false
		}
	}
	return true
}

func hasAnyScope(actual, required []string) bool {
	for _, scope := range actual {
		if slices.Contains(required, scope) {
			return true
		}
	}
	return false
}

func contextString(ctx any, name string) (string, bool) {
	value, ok := contextValue(ctx, name)
	if !ok {
		return "", false
	}
	s, ok := contextValueString(value)
	if !ok || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

func contextStringList(ctx any, name string) ([]string, bool) {
	value, ok := contextValue(ctx, name)
	if !ok {
		return nil, false
	}
	switch v := value.(type) {
	case []string:
		return append([]string{}, v...), len(v) > 0
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, false
			}
			out = append(out, s)
		}
		return out, len(out) > 0
	case string:
		out := workerenv.SplitCSV(v)
		return out, len(out) > 0
	default:
		return contextValueStringSlice(value)
	}
}

func contextValue(ctx any, name string) (any, bool) {
	switch v := ctx.(type) {
	case map[string]any:
		value, ok := v[name]
		return value, ok
	case map[string]string:
		value, ok := v[name]
		return value, ok
	}

	rv := reflect.ValueOf(ctx)
	if !rv.IsValid() || rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}

	key := reflect.ValueOf(name)
	if !key.Type().AssignableTo(rv.Type().Key()) {
		if !key.Type().ConvertibleTo(rv.Type().Key()) {
			return nil, false
		}
		key = key.Convert(rv.Type().Key())
	}

	value := rv.MapIndex(key)
	if !value.IsValid() {
		return nil, false
	}
	return value.Interface(), true
}

func contextValueString(value any) (string, bool) {
	if s, ok := value.(string); ok {
		return s, true
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() || rv.Kind() != reflect.String {
		return "", false
	}
	return rv.String(), true
}

func contextValueStringSlice(value any) ([]string, bool) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() || rv.Kind() != reflect.Slice || rv.Type().Elem().Kind() != reflect.String {
		return nil, false
	}
	out := make([]string, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out = append(out, rv.Index(i).String())
	}
	return out, len(out) > 0
}

func taskRequestWorkspace(req CreateTaskRequest) *corev1alpha1.WorkspaceConfig {
	if req.AgentRuntime == nil {
		return nil
	}
	return req.AgentRuntime.Workspace
}

func workspaceGitRepo(workspace *corev1alpha1.WorkspaceConfig) string {
	if workspace == nil {
		return ""
	}
	return workspace.GitRepo
}

func workspaceBranch(workspace *corev1alpha1.WorkspaceConfig) string {
	if workspace == nil {
		return ""
	}
	return workspace.Branch
}

func workspaceRef(workspace *corev1alpha1.WorkspaceConfig) string {
	if workspace == nil {
		return ""
	}
	return workspace.Ref
}
