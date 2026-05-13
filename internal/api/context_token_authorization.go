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
)

// ContextTokenAuthorizationConfig controls optional authorization checks derived
// from verified context-token scope and transaction context claims.
type ContextTokenAuthorizationConfig struct {
	Mode             string
	TaskCreateScopes []string
	TaskReadScopes   []string
	TaskListScopes   []string
	TaskDeleteScopes []string
}

// NewContextTokenAuthorizationConfig builds context-token authorization config.
func NewContextTokenAuthorizationConfig(mode, taskCreateScopes, taskReadScopes, taskListScopes, taskDeleteScopes string) (ContextTokenAuthorizationConfig, error) {
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
	return ContextTokenAuthorizationConfig{
		Mode:             mode,
		TaskCreateScopes: createScopes,
		TaskReadScopes:   readScopes,
		TaskListScopes:   listScopes,
		TaskDeleteScopes: deleteScopes,
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
		return nil
	}

	return h.handleContextTokenAuthorizationFailures(ui.ContextToken, "createTask", failures)
}

func (h *Handlers) authorizeContextTokenAction(c fiber.Ctx, action string, requiredScopes []string) error {
	if !h.contextTokenAuthorization.Enabled() {
		return nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil
	}
	if hasAnyScope(ui.ContextToken.Scopes, requiredScopes) {
		return nil
	}
	failures := []string{fmt.Sprintf("missing one of required scopes %q", strings.Join(requiredScopes, ","))}
	return h.handleContextTokenAuthorizationFailures(ui.ContextToken, action, failures)
}

func (h *Handlers) handleContextTokenAuthorizationFailures(token *ContextToken, action string, failures []string) error {
	log.Info("context-token authorization failed",
		"mode", h.contextTokenAuthorization.Mode,
		"action", action,
		"transactionID", token.TransactionID,
		"subject", token.Subject,
		"issuer", token.Issuer,
		"failures", strings.Join(failures, "; "),
	)
	if h.contextTokenAuthorization.enforcing() {
		return fiber.NewError(fiber.StatusForbidden, "context token is not authorized for "+action)
	}
	return nil
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
