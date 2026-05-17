/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
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
	kubeClient                kubernetes.Interface
	sessionManager            *controller.SessionManager
	namespace                 string
	provider                  string
	providerType              string
	sessionID                 string
	taskSeq                   atomic.Int32
	tasksCreated              int
	maxTasks                  int
	toolTimeout               time.Duration
	watchNamespace            string
	enforceNamespaceIsolation bool
	resultStore               store.ResultStore
	registry                  *tools.Registry
	allowedToolNames          map[string]struct{}
	authorizeTaskCreate       func(context.Context, *corev1alpha1.Task) error
}

// NewToolExecutor creates a new ToolExecutor.
func NewToolExecutor(c client.Client, sm *controller.SessionManager, namespace, sessionID, watchNamespace string, enforceNS bool, maxTasks int, toolTimeout time.Duration, rs store.ResultStore, kubeClientOpt ...kubernetes.Interface) *ToolExecutor {
	var kubeClient kubernetes.Interface
	if len(kubeClientOpt) > 0 {
		kubeClient = kubeClientOpt[0]
	}

	reg := tools.NewRegistry()
	tools.RegisterChatTools(reg)
	return &ToolExecutor{
		client:                    c,
		kubeClient:                kubeClient,
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

// SetAllowedTools restricts execution to the tools exposed and authorized for
// the current request. A nil allowlist means no restriction, preserving the
// default behavior for non context-token callers.
func (e *ToolExecutor) SetAllowedTools(allowedTools []llm.Tool) {
	e.allowedToolNames = make(map[string]struct{}, len(allowedTools))
	for _, tool := range allowedTools {
		name := strings.TrimSpace(tool.Name)
		if name != "" {
			e.allowedToolNames[name] = struct{}{}
		}
	}
}

// SetTaskCreateAuthorizer installs an authorization hook for tools that create Tasks.
func (e *ToolExecutor) SetTaskCreateAuthorizer(authorize func(context.Context, *corev1alpha1.Task) error) {
	e.authorizeTaskCreate = authorize
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

	if e.allowedToolNames != nil {
		if _, ok := e.allowedToolNames[toolCall.Name]; !ok {
			span.SetStatus(codes.Error, "unauthorized tool")
			result := toolError("unauthorized_tool", fmt.Sprintf("tool %q is not authorized for this request", toolCall.Name), "Use one of the available tools")
			return marshalResult(result)
		}
	}

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
		KubeClient:                e.kubeClient,
		Namespace:                 e.namespace,
		SessionID:                 e.sessionID,
		ToolCallID:                toolCall.ID,
		Tenant:                    e.namespace,
		Provider:                  e.provider,
		ProviderType:              e.providerType,
		WatchNamespace:            e.watchNamespace,
		EnforceNamespaceIsolation: e.enforceNamespaceIsolation,
		ResultStore:               e.resultStore,
		SessionDeleter:            e.sessionManager,
		GenerateTaskName:          e.generateTaskName,
		TaskLabels:                e.taskLabels,
		AuthorizeTaskCreate: func(ctx context.Context, task *corev1alpha1.Task) *tools.ChatToolError {
			if e.authorizeTaskCreate == nil {
				return nil
			}
			if err := e.authorizeTaskCreate(ctx, task); err != nil {
				return &tools.ChatToolError{
					Type:       "authorization_failed",
					Message:    err.Error(),
					Suggestion: "Use a task configuration authorized by the context token",
				}
			}
			return nil
		},
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
	prefix := sanitizeTaskNameComponent(e.sessionID)
	if prefix == "" {
		prefix = "session"
	}
	hash := sha256.Sum256([]byte(e.sessionID))
	hashSuffix := hex.EncodeToString(hash[:])[:8]
	seqSuffix := strconv.FormatInt(int64(seq), 10)

	// Kubernetes task names must fit DNS label rules (max 63 chars).
	const taskNameOverhead = len("chat-") + len("-") + len("-")
	maxPrefixLen := max(63-taskNameOverhead-len(hashSuffix)-len(seqSuffix), 1)
	if len(prefix) > maxPrefixLen {
		prefix = strings.Trim(prefix[:maxPrefixLen], "-")
		if prefix == "" {
			prefix = "session"
		}
	}

	return fmt.Sprintf("chat-%s-%s-%s", prefix, hashSuffix, seqSuffix)
}

func (e *ToolExecutor) taskLabels() map[string]string {
	return map[string]string{
		labels.LabelCreatedBy:   "orchestrator",
		labels.LabelChatSession: e.sessionID,
	}
}

func sanitizeTaskNameComponent(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false

	for _, r := range s {
		isLower := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if isLower || isDigit {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	return strings.Trim(b.String(), "-")
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
