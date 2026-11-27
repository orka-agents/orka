/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	taskCreatedMsg       = "Task created"
	toolNamespaceArg     = "namespace"
	chatCreateAITaskTool = "create_ai_task"
	limitReachedCode     = "limit_reached"
)

type ToolExecutor struct {
	client                    client.Client
	policyReader              client.Reader
	kubeClient                kubernetes.Interface
	sessionManager            *controller.SessionManager
	namespace                 string
	provider                  string
	providerNamespace         string
	providerType              string
	executionMode             executionmode.Mode
	sessionID                 string
	chatSessionEpoch          string
	taskSeq                   atomic.Int32
	taskSeqMu                 sync.Mutex
	taskSeqSeeded             bool
	tasksCreated              int
	maxTasks                  int
	toolTimeout               time.Duration
	watchNamespace            string
	enforceNamespaceIsolation bool
	resultStore               store.ResultStore
	gatewayEventStore         store.GatewayEventStore
	userInfo                  *UserInfo
	requester                 *corev1alpha1.RequestedBy
	linkedAccounts            tools.LinkedAccountCredentials
	authorizeConnectorRead    func() *tools.ChatToolError
	dashboardEnabled          bool
	createdTasks              *tools.CreatedTasks
	registry                  *tools.Registry
	allowedToolNames          map[string]struct{}
	authorizeTaskCreate       func(context.Context, *corev1alpha1.Task) error
	authorizeTaskDelete       func(context.Context, *corev1alpha1.Task) error
	authorizeAgentCreate      func(context.Context, *corev1alpha1.Agent) error
	authorizeAgentReference   func(context.Context, *corev1alpha1.Agent) error
	authorizeAgentUpdate      func(context.Context, *corev1alpha1.Agent) error
	authorizeAgentDelete      func(context.Context, *corev1alpha1.Agent) error
	authorizeSecretRead       func(context.Context, string, string) error
}

func (e *ToolExecutor) SetExecutionMode(mode executionmode.Mode) {
	e.executionMode = mode
}

func (e *ToolExecutor) SetPolicyReader(reader client.Reader) {
	e.policyReader = reader
}

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

func (e *ToolExecutor) SetAllowedTools(allowedTools []llm.Tool) {
	e.allowedToolNames = make(map[string]struct{}, len(allowedTools))
	for _, tool := range allowedTools {
		name := strings.TrimSpace(tool.Name)
		if name != "" {
			e.allowedToolNames[name] = struct{}{}
		}
	}
}

func (e *ToolExecutor) SetTaskCreateAuthorizer(authorize func(context.Context, *corev1alpha1.Task) error) {
	e.authorizeTaskCreate = authorize
}

func (e *ToolExecutor) SetTaskDeleteAuthorizer(authorize func(context.Context, *corev1alpha1.Task) error) {
	e.authorizeTaskDelete = authorize
}

func (e *ToolExecutor) SetAgentCreateAuthorizer(authorize func(context.Context, *corev1alpha1.Agent) error) {
	e.authorizeAgentCreate = authorize
}

func (e *ToolExecutor) SetAgentUpdateAuthorizer(authorize func(context.Context, *corev1alpha1.Agent) error) {
	e.authorizeAgentUpdate = authorize
}

func (e *ToolExecutor) SetAgentDeleteAuthorizer(authorize func(context.Context, *corev1alpha1.Agent) error) {
	e.authorizeAgentDelete = authorize
}

func (e *ToolExecutor) SetSecretReadAuthorizer(authorize func(context.Context, string, string) error) {
	e.authorizeSecretRead = authorize
}

type ToolResult struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorType  string `json:"errorType,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

func (e *ToolExecutor) Execute(ctx context.Context, toolCall llm.ToolCall) (string, error) {
	if e.allowedToolNames != nil {
		if _, ok := e.allowedToolNames[toolCall.Name]; !ok {
			result := toolError("unauthorized_tool", fmt.Sprintf("tool %q is not authorized for this request", toolCall.Name), "Use one of the available tools")
			resultStr, err := marshalResult(result)
			recordRejectedToolCall(ctx, toolCall, resultStr)
			return resultStr, err
		}
	}

	var args map[string]any
	if err := json.Unmarshal(toolCall.Arguments, &args); err != nil {
		result := toolError("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
		resultStr, marshalErr := marshalResult(result)
		recordRejectedToolCall(ctx, toolCall, resultStr)
		return resultStr, marshalErr
	}
	targetNamespace := e.namespace
	if value := args[toolNamespaceArg]; value != nil && fmt.Sprint(value) != "" {
		targetNamespace = fmt.Sprint(value)
	}
	var sessionRef string
	if value := args["sessionRef"]; value != nil {
		sessionRef = fmt.Sprint(value)
	}
	var schedule string
	if value := args["schedule"]; value != nil {
		schedule = fmt.Sprint(value)
	}
	if toolCall.Name == chatCreateAITaskTool && schedule == "" &&
		strings.TrimSpace(sessionRef) != "" && targetNamespace == e.namespace &&
		strings.TrimSpace(sessionRef) == strings.TrimSpace(e.sessionID) {
		result := toolError("invalid_arguments", "child task sessionRef cannot reuse the active chat session", "Use a different session name or omit sessionRef")
		resultStr, err := marshalResult(result)
		recordRejectedToolCall(ctx, toolCall, resultStr)
		return resultStr, err
	}

	toolCtx, cancel := context.WithTimeout(ctx, e.toolTimeout)
	defer cancel()

	tc := &tools.ToolContext{
		Client:                    e.client,
		PolicyReader:              e.policyReader,
		KubeClient:                e.kubeClient,
		Namespace:                 e.namespace,
		SessionID:                 e.sessionID,
		ToolCallID:                toolCall.ID,
		Tenant:                    e.namespace,
		Provider:                  e.provider,
		ProviderNamespace:         e.providerNamespace,
		ProviderType:              e.providerType,
		ExecutionMode:             e.executionMode,
		WatchNamespace:            e.watchNamespace,
		EnforceNamespaceIsolation: e.enforceNamespaceIsolation,
		ResultStore:               e.resultStore,
		SessionDeleter:            e.sessionManager,
		GenerateTaskName:          func() string { return e.generateTaskName(toolCtx) },
		TaskLabels:                e.taskLabels,
		AuthorizeAgentReference: func(ctx context.Context, agent *corev1alpha1.Agent) *tools.ChatToolError {
			return chatToolAuthorizationError(e.authorizeAgentReference, ctx, agent, "Use an authorized Agent reference")
		},
		AuthorizeTaskCreate: func(ctx context.Context, task *corev1alpha1.Task) *tools.ChatToolError {
			return chatToolAuthorizationError(e.authorizeTaskCreate, ctx, task, "Use a task configuration authorized by the context token")
		},
		SealTaskCreate:         requesterStampSealer,
		Requester:              e.requester,
		LinkedAccounts:         e.linkedAccounts,
		AuthorizeConnectorRead: e.authorizeConnectorRead,
		DashboardEnabled:       e.dashboardEnabled,
		CreatedTasks:           e.createdTasks,
		AuthorizeTaskDelete: func(ctx context.Context, task *corev1alpha1.Task) *tools.ChatToolError {
			return chatToolAuthorizationError(e.authorizeTaskDelete, ctx, task, "Use a task authorized by the context token")
		},
		AuthorizeAgentCreate: func(ctx context.Context, agent *corev1alpha1.Agent) *tools.ChatToolError {
			return chatToolAuthorizationError(e.authorizeAgentCreate, ctx, agent, "Use an agent configuration authorized by the context token")
		},
		AuthorizeAgentUpdate: func(ctx context.Context, agent *corev1alpha1.Agent) *tools.ChatToolError {
			return chatToolAuthorizationError(e.authorizeAgentUpdate, ctx, agent, "Use an agent update authorized by the context token")
		},
		AuthorizeAgentDelete: func(ctx context.Context, agent *corev1alpha1.Agent) *tools.ChatToolError {
			return chatToolAuthorizationError(e.authorizeAgentDelete, ctx, agent, "Use an agent authorized by the context token")
		},
		AuthorizeSecretRead: func(ctx context.Context, namespace, secretName string) *tools.ChatToolError {
			authorize := func(ctx context.Context, _ *corev1alpha1.Task) error {
				if e.authorizeSecretRead == nil {
					return nil
				}
				return e.authorizeSecretRead(ctx, namespace, secretName)
			}
			return chatToolAuthorizationError(authorize, ctx, nil, "Use a context token authorized to read the git credential secret")
		},
		RequireSecretReadAuthorization: e.authorizeSecretRead != nil,
		CheckTaskLimit: func() *tools.ChatToolError {
			if e.tasksCreated >= e.maxTasks {
				return &tools.ChatToolError{
					Type:       limitReachedCode,
					Message:    fmt.Sprintf("task creation limit reached (max %d per turn)", e.maxTasks),
					Suggestion: "Wait for existing tasks to complete before creating new ones",
				}
			}
			return nil
		},
		IncrementTasks: func() { e.tasksCreated++ },
	}
	authorizeExternalToolContext(tc, e.userInfo, e.gatewayEventStore)
	toolCtx = tools.WithToolContext(toolCtx, tc)

	resultStr, err := e.registry.Execute(toolCtx, toolCall.Name, toolCall.Arguments)
	if err != nil {
		var argErr *tools.ToolArgumentError
		var notFound *tools.ToolNotFoundError
		switch {
		case errors.As(err, &argErr):
			return marshalResult(toolError("invalid_arguments", argErr.Error(), "Resend the call with "+argErr.Field+" as "+argErr.Want))
		case errors.As(err, &notFound):
			return marshalResult(toolError("unknown_tool", fmt.Sprintf("unknown tool: %s", toolCall.Name), "Use one of the available tools"))
		default:
			return marshalResult(toolError("tool_error", err.Error(), ""))
		}
	}

	var tr ToolResult
	if jsonErr := json.Unmarshal([]byte(resultStr), &tr); jsonErr == nil {
		return resultStr, nil
	}

	result := ToolResult{Success: true, Data: resultStr}
	return marshalResult(result)
}

func (e *ToolExecutor) generateTaskName(ctx context.Context) string {
	seqSuffix := e.nextTaskSeqSuffix(ctx)
	prefix := sanitizeTaskNameComponent(e.sessionID)
	if prefix == "" {
		prefix = "session"
	}
	hashSuffix := e.taskNameHash()

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

func (e *ToolExecutor) taskNameHash() string {
	hash := sha256.Sum256([]byte(e.sessionID))
	return hex.EncodeToString(hash[:])[:8]
}

func (e *ToolExecutor) nextTaskSeqSuffix(ctx context.Context) string {
	e.taskSeqMu.Lock()
	defer e.taskSeqMu.Unlock()
	if !e.taskSeqSeeded {
		latest, err := e.latestSessionTaskSeq(ctx)
		if err != nil {
			chatLog.Error(err, "Failed to list chat session tasks; using a random task name suffix", "session", e.sessionID)
			return "r" + strings.ToLower(rand.Text()[:8])
		}
		e.taskSeq.Store(latest)
		e.taskSeqSeeded = true
	}
	return strconv.FormatInt(int64(e.taskSeq.Add(1)), 10)
}

func (e *ToolExecutor) latestSessionTaskSeq(ctx context.Context) (int32, error) {
	opts := []client.ListOption{client.MatchingLabels{labels.LabelChatSession: e.sessionID}}
	switch {
	case e.watchNamespace != "":
		opts = append(opts, client.InNamespace(e.watchNamespace))
	case e.enforceNamespaceIsolation:
		opts = append(opts, client.InNamespace(e.namespace))
	}
	var tasks corev1alpha1.TaskList
	if err := uncachedReaderOr(e.policyReader, e.client).List(ctx, &tasks, opts...); err != nil {
		return 0, err
	}
	hashMarker := "-" + e.taskNameHash()
	var latest int32
	for i := range tasks.Items {
		name := tasks.Items[i].Name
		dash := strings.LastIndexByte(name, '-')
		if !strings.HasPrefix(name, "chat-") || dash < 0 || !strings.HasSuffix(name[:dash], hashMarker) {
			continue
		}
		if seq, err := strconv.ParseInt(name[dash+1:], 10, 32); err == nil && int32(seq) > latest {
			latest = int32(seq)
		}
	}
	return latest, nil
}

func (e *ToolExecutor) taskLabels() map[string]string {
	taskLabels := map[string]string{
		labels.LabelCreatedBy:            "orchestrator",
		labels.LabelChatSession:          e.sessionID,
		labels.LabelChatSessionNamespace: e.namespace,
	}
	if e.chatSessionEpoch != "" {
		taskLabels[labels.LabelChatSessionEpoch] = e.chatSessionEpoch
	}
	return taskLabels
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

func recordRejectedToolCall(ctx context.Context, toolCall llm.ToolCall, result string) {
	if strings.TrimSpace(toolCall.Name) == "" || result == "" {
		return
	}
	failed, errType := tools.FailedToolResultForTelemetry(result)
	if !failed {
		return
	}
	tools.RecordRejectedToolCall(ctx, toolCall.Name, toolCall.ID, errType)
}
