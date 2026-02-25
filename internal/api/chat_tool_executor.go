/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sozercan/orka/internal/controller"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/tracing"
)

const taskCreatedMsg = "Task created"

// ToolExecutor executes orchestrator LLM tool calls by creating and managing
// Kubernetes resources (Tasks, Agents, Tools, Sessions).
type ToolExecutor struct {
	client                    client.Client
	sessionManager            *controller.SessionManager
	namespace                 string
	sessionID                 string
	taskSeq                   atomic.Int32
	tasksCreated              int
	maxTasks                  int
	toolTimeout               time.Duration
	watchNamespace            string
	enforceNamespaceIsolation bool
	resultStore               store.ResultStore
	registry                  *tools.Registry
}

// NewToolExecutor creates a new ToolExecutor.
func NewToolExecutor(c client.Client, sm *controller.SessionManager, namespace, sessionID, watchNamespace string, enforceNS bool, maxTasks int, toolTimeout time.Duration, rs store.ResultStore) *ToolExecutor {
	reg := tools.NewRegistry()
	tools.RegisterChatTools(reg)
	return &ToolExecutor{
		client:                    c,
		sessionManager:            sm,
		namespace:                 namespace,
		sessionID:                 sessionID,
		maxTasks:                  maxTasks,
		toolTimeout:               toolTimeout,
		watchNamespace:            watchNamespace,
		enforceNamespaceIsolation: enforceNS,
		resultStore:               rs,
		registry:                  reg,
	}
}

// ToolResult represents the result of a tool execution.
type ToolResult struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorType  string `json:"errorType,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Execute dispatches a tool call to the appropriate handler and returns
// the JSON-serialized result.
func (e *ToolExecutor) Execute(ctx context.Context, toolCall llm.ToolCall) (string, error) {
	tracer := tracing.Tracer("orka.tools")
	ctx, span := tracer.Start(ctx, "tool.execute",
		trace.WithAttributes(
			attribute.String("tool.name", toolCall.Name),
		),
	)
	defer span.End()

	var args map[string]any
	if err := json.Unmarshal(toolCall.Arguments, &args); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		result := toolError("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
		return marshalResult(result)
	}

	toolCtx, cancel := context.WithTimeout(ctx, e.toolTimeout)
	defer cancel()

	// Set up ToolContext for registry-based tools
	tc := &tools.ToolContext{
		Client:                    e.client,
		Namespace:                 e.namespace,
		WatchNamespace:            e.watchNamespace,
		EnforceNamespaceIsolation: e.enforceNamespaceIsolation,
		ResultStore:               e.resultStore,
		SessionDeleter:            e.sessionManager,
		GenerateTaskName:          e.generateTaskName,
		TaskLabels:                e.taskLabels,
		CheckTaskLimit: func() *tools.ChatToolError {
			if e.tasksCreated >= e.maxTasks {
				return &tools.ChatToolError{
					Type:       "limit_reached",
					Message:    fmt.Sprintf("task creation limit reached (max %d per turn)", e.maxTasks),
					Suggestion: "Wait for existing tasks to complete before creating new ones",
				}
			}
			return nil
		},
		IncrementTasks: func() { e.tasksCreated++ },
		FindGitSecret:  e.findGitSecret,
	}
	toolCtx = tools.WithToolContext(toolCtx, tc)

	// Marshal args to JSON for the Tool interface
	argsJSON, err := json.Marshal(args)
	if err != nil {
		result := toolError("internal_error", fmt.Sprintf("failed to marshal arguments: %v", err), "")
		return marshalResult(result)
	}

	// Execute via registry
	resultStr, err := e.registry.Execute(toolCtx, toolCall.Name, argsJSON)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		result := toolError("unknown_tool", fmt.Sprintf("unknown tool: %s", toolCall.Name), "Use one of the available tools")
		return marshalResult(result)
	}

	// The registry tools return JSON-marshaled ChatToolResult strings.
	// Parse them back into ToolResult for consistent span attributes.
	var tr ToolResult
	if jsonErr := json.Unmarshal([]byte(resultStr), &tr); jsonErr == nil {
		if tr.Success {
			span.SetAttributes(attribute.Bool("tool.success", true))
		} else {
			span.SetStatus(codes.Error, tr.Error)
		}
		return resultStr, nil
	}

	// Fallback: wrap raw string as success
	span.SetAttributes(attribute.Bool("tool.success", true))
	result := ToolResult{Success: true, Data: resultStr}
	return marshalResult(result)
}

// ---- Task creation helpers ----

func (e *ToolExecutor) generateTaskName() string {
	seq := e.taskSeq.Add(1)
	prefix := e.sessionID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return fmt.Sprintf("chat-%s-%d", prefix, seq)
}

func (e *ToolExecutor) taskLabels() map[string]string {
	return map[string]string{
		labels.LabelCreatedBy:   "orchestrator",
		labels.LabelChatSession: e.sessionID,
	}
}

// ---- Error handling helpers ----

func toolError(errType, message, suggestion string) ToolResult {
	return ToolResult{
		Success:    false,
		Error:      message,
		ErrorType:  errType,
		Suggestion: suggestion,
	}
}

func marshalResult(result ToolResult) (string, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tool result: %w", err)
	}
	return string(b), nil
}

// findGitSecret looks for a git credentials secret in the namespace.
func (e *ToolExecutor) findGitSecret(ctx context.Context, namespace string) string {
	for _, name := range []string{"github-credentials", "git-credentials", "github-token", "git-token"} {
		secret := &corev1.Secret{}
		if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err == nil {
			if _, hasToken := secret.Data["token"]; hasToken {
				return name
			}
			if _, hasPassword := secret.Data["password"]; hasPassword {
				return name
			}
		}
	}
	return ""
}
