/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/metrics"
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

// NewContextTokenAuthorizationConfig builds context-token authorization config.
func NewContextTokenAuthorizationConfig(
	mode,
	taskCreateScopes,
	taskReadScopes,
	taskListScopes,
	taskDeleteScopes,
	toolReadScopes,
	toolUseScopes,
	providerUseScopes,
	agentReadScopes,
	agentWriteScopes,
	memoryReadScopes,
	memoryWriteScopes,
	sessionReadScopes,
	sessionWriteScopes,
	securityReadScopes,
	securityWriteScopes,
	skillReadScopes,
	skillWriteScopes string,
) (ContextTokenAuthorizationConfig, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = ContextTokenAuthorizationModeOff
	}
	switch mode {
	case ContextTokenAuthorizationModeOff, ContextTokenAuthorizationModeAudit, ContextTokenAuthorizationModeEnforce:
	default:
		return ContextTokenAuthorizationConfig{}, fmt.Errorf("unsupported context-token authorization mode %q", mode)
	}

	createScopes := defaultScopes(taskCreateScopes, ContextTokenScopeTaskCreate)
	readScopes := defaultScopes(taskReadScopes, ContextTokenScopeTaskGet)
	listScopes := defaultScopes(taskListScopes, ContextTokenScopeTaskList)
	deleteScopes := defaultScopes(taskDeleteScopes, ContextTokenScopeTaskDelete)
	toolRead := defaultScopes(toolReadScopes, ContextTokenScopeToolsRead)
	toolUse := defaultScopes(toolUseScopes, ContextTokenScopeToolsUse)
	providerUse := defaultScopes(providerUseScopes, ContextTokenScopeProvidersUse)
	agentRead := defaultScopes(agentReadScopes, ContextTokenScopeAgentsRead)
	agentWrite := defaultScopes(agentWriteScopes, ContextTokenScopeAgentsWrite)
	memoryRead := defaultScopes(memoryReadScopes, ContextTokenScopeMemoryRead)
	memoryWrite := defaultScopes(memoryWriteScopes, ContextTokenScopeMemoryWrite)
	sessionRead := defaultScopes(sessionReadScopes, ContextTokenScopeSessionsRead)
	sessionWrite := defaultScopes(sessionWriteScopes, ContextTokenScopeSessionsWrite)
	securityRead := defaultScopes(securityReadScopes, ContextTokenScopeSecurityRead)
	securityWrite := defaultScopes(securityWriteScopes, ContextTokenScopeSecurityWrite)
	skillRead := defaultScopes(skillReadScopes, ContextTokenScopeSkillsRead)
	skillWrite := defaultScopes(skillWriteScopes, ContextTokenScopeSkillsWrite)
	return ContextTokenAuthorizationConfig{
		Mode:                mode,
		TaskCreateScopes:    createScopes,
		TaskReadScopes:      readScopes,
		TaskListScopes:      listScopes,
		TaskDeleteScopes:    deleteScopes,
		ToolReadScopes:      toolRead,
		ToolUseScopes:       toolUse,
		ProviderUseScopes:   providerUse,
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

func defaultScopes(value, fallback string) []string {
	if scopes := splitComma(value); len(scopes) > 0 {
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

	failures := contextTokenTaskCreateFailures(ui.ContextToken, h.contextTokenAuthorization, req, namespace)
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
	if hasAnyScope(ui.ContextToken.Scopes, requiredScopes) {
		metrics.RecordContextTokenAuthorization(action, "allowed", "ok")
		return nil
	}
	failures := []string{fmt.Sprintf("missing one of required scopes %q", strings.Join(requiredScopes, ","))}
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

	if want, ok := contextString(token.TransactionContext, "namespace"); ok && namespace != want {
		failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", namespace, want))
	}
	if want, ok := contextString(token.TransactionContext, "provider"); ok && !providerMatches(provider, want) {
		failures = append(failures, fmt.Sprintf("provider %q is not allowed by token context", provider.Name))
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedProviders"); ok && !providerAllowed(provider, allowed) {
		failures = append(failures, fmt.Sprintf("provider %q is not allowed by token context", provider.Name))
	}
	if want, ok := contextString(token.TransactionContext, "model"); ok && model != want {
		failures = append(failures, fmt.Sprintf("model %q does not match token context %q", model, want))
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedModels"); ok && !modelAllowed(provider, model, allowed) {
		failures = append(failures, fmt.Sprintf("model %q is not allowed by token context", model))
	}

	return failures
}

func contextTokenTaskCreateFailures(token *ContextToken, cfg ContextTokenAuthorizationConfig, req CreateTaskRequest, namespace string) []string {
	failures := []string{}
	if !hasAnyScope(token.Scopes, cfg.TaskCreateScopes) {
		failures = append(failures, fmt.Sprintf("missing one of required scopes %q", strings.Join(cfg.TaskCreateScopes, ",")))
	}

	if want, ok := contextString(token.TransactionContext, "namespace"); ok && namespace != want {
		failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", namespace, want))
	}
	if want, ok := contextString(token.TransactionContext, "taskType"); ok && string(req.Type) != want {
		failures = append(failures, fmt.Sprintf("task type %q does not match token context %q", req.Type, want))
	}
	if want, ok := contextString(token.TransactionContext, "agent"); ok {
		got := ""
		if req.AgentRef != nil {
			got = req.AgentRef.Name
		}
		if got != want {
			failures = append(failures, fmt.Sprintf("agent %q does not match token context %q", got, want))
		}
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedAgents"); ok && req.AgentRef != nil && !slices.Contains(allowed, req.AgentRef.Name) {
		failures = append(failures, fmt.Sprintf("agent %q is not allowed by token context", req.AgentRef.Name))
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
	if allowed, ok := contextStringList(token.TransactionContext, "allowedTools"); ok && req.AgentRuntime != nil {
		for _, tool := range req.AgentRuntime.AllowedTools {
			if !slices.Contains(allowed, tool) {
				failures = append(failures, fmt.Sprintf("tool %q is not allowed by token context", tool))
			}
		}
	}

	return failures
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

func providerAllowed(provider ProviderResolutionInfo, allowed []string) bool {
	for _, want := range allowed {
		if providerMatches(provider, want) {
			return true
		}
	}
	return false
}

func providerMatches(provider ProviderResolutionInfo, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	return provider.Name == want || provider.Type == want
}

func modelAllowed(provider ProviderResolutionInfo, model string, allowed []string) bool {
	for _, want := range allowed {
		want = strings.TrimSpace(want)
		switch want {
		case "":
			continue
		case model, provider.Name + "/" + model, provider.Type + "/" + model:
			return true
		}
	}
	return false
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

func contextString(ctx map[string]any, name string) (string, bool) {
	value, ok := ctx[name]
	if !ok {
		return "", false
	}
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

func contextStringList(ctx map[string]any, name string) ([]string, bool) {
	value, ok := ctx[name]
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
		out := splitComma(v)
		return out, len(out) > 0
	default:
		return nil, false
	}
}

func splitComma(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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
