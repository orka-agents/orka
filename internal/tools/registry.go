/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/tracing/genai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ToolContext provides dependencies for tools that need K8s client access or other services.
type ToolContext struct {
	Client                    client.Client
	KubeClient                kubernetes.Interface
	Namespace                 string
	SessionID                 string
	TaskID                    string
	ToolCallID                string
	Tenant                    string
	Provider                  string
	ProviderType              string
	WatchNamespace            string
	EnforceNamespaceIsolation bool
	// ResultStore for fetching task outputs (store.ResultStore)
	ResultStore interface {
		GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	}
	// SessionDeleter for deleting sessions (controller.SessionManager)
	SessionDeleter interface {
		DeleteSession(ctx context.Context, namespace, sessionID string) error
	}
	// Task creation helpers provided by the chat executor
	GenerateTaskName               func() string
	TaskLabels                     func() map[string]string
	CheckTaskLimit                 func() *ChatToolError
	AuthorizeTaskCreate            func(context.Context, *corev1alpha1.Task) *ChatToolError
	AuthorizeTaskDelete            func(context.Context, *corev1alpha1.Task) *ChatToolError
	AuthorizeAgentCreate           func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeAgentUpdate           func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeAgentDelete           func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeSecretRead            func(context.Context, string, string) *ChatToolError
	RequireSecretReadAuthorization bool
	IncrementTasks                 func()
}

type toolContextKey struct{}

// WithToolContext adds a ToolContext to a context.
func WithToolContext(ctx context.Context, tc *ToolContext) context.Context {
	return context.WithValue(ctx, toolContextKey{}, tc)
}

// GetToolContext extracts a ToolContext from a context.
func GetToolContext(ctx context.Context) *ToolContext {
	tc, _ := ctx.Value(toolContextKey{}).(*ToolContext)
	return tc
}

// ChatToolError represents a structured error from a chat tool.
type ChatToolError struct {
	Type       string `json:"errorType"`
	Message    string `json:"error"`
	Suggestion string `json:"suggestion"`
}

func (e *ChatToolError) Error() string { return e.Message }

// ChatToolResult represents the result of a chat tool execution.
type ChatToolResult struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorType  string `json:"errorType,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// marshalChatResult marshals a ChatToolResult to a JSON string.
func marshalChatResult(r ChatToolResult) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("failed to marshal chat tool result: %w", err)
	}
	return string(b), nil
}

// ChatToolSuccess returns a successful ChatToolResult JSON string.
func ChatToolSuccess(data any) (string, error) {
	return marshalChatResult(ChatToolResult{Success: true, Data: data})
}

// ChatToolErrorResult returns a failed ChatToolResult JSON string.
func ChatToolErrorResult(errType, message, suggestion string) (string, error) {
	return marshalChatResult(ChatToolResult{
		Error:      message,
		ErrorType:  errType,
		Suggestion: suggestion,
	})
}

const githubAPIBaseURL = "https://api.github.com"

const defaultNamespace = "default"

const trueStr = "true"

const defaultMergeMethod = "squash"

const (
	unknownToolTelemetryName  = "unknown_tool"
	rejectedToolTelemetryName = "rejected_tool"
)

// Tool is the interface for built-in tools
type Tool interface {
	// Name returns the tool name
	Name() string

	// Description returns the tool description for the LLM
	Description() string

	// Parameters returns the JSON Schema for the tool parameters
	Parameters() json.RawMessage

	// Execute executes the tool with the given arguments
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry manages registered tools
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates a new tool registry
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register registers a tool
func (r *Registry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Get returns a tool by name
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// List returns all registered tools
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// Execute executes a tool by name. It is the DRY instrumentation point for
// built-in registry tools used by chat, proxy-compatible handlers, and workers.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	start := time.Now()
	tool, ok := r.Get(name)
	toolTelemetryName := name
	if !ok {
		toolTelemetryName = unknownToolTelemetryName
	}
	toolTypeValue := toolType(ctx, tool)
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, toolTelemetryName),
		attribute.String(genai.AttrToolType, toolTypeValue),
	}
	if tc := GetToolContext(ctx); tc != nil && tc.ToolCallID != "" {
		attrs = append(attrs, attribute.String(genai.AttrToolCallID, tc.ToolCallID))
	}
	if ok {
		if description := tool.Description(); description != "" {
			attrs = append(attrs, attribute.String(genai.AttrToolDescription, description))
		}
	}
	tracer := tracing.GenAITracer(genai.InstrumentationName)
	ctx, span := tracer.Start(ctx, genai.OperationExecuteTool+" "+toolTelemetryName, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	defer span.End()

	metricAttrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, toolTelemetryName),
		attribute.String(genai.AttrToolType, toolTypeValue),
	}
	if !ok {
		err := fmt.Errorf("tool %q not found", name)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metricAttrs = append(metricAttrs, attribute.String(genai.AttrErrorType, "tool_not_found"))
		recordToolDuration(ctx, time.Since(start).Seconds(), metricAttrs...)
		return "", err
	}

	result, err := tool.Execute(ctx, args)
	duration := time.Since(start).Seconds()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		metricAttrs = append(metricAttrs, attribute.String(genai.AttrErrorType, fmt.Sprintf("%T", err)))
	} else if failed, errType, message := failedToolResult(result); failed {
		span.SetStatus(codes.Error, message)
		metricAttrs = append(metricAttrs, attribute.String(genai.AttrErrorType, errType))
	}
	recordToolDuration(ctx, duration, metricAttrs...)
	return result, err
}

// RecordRejectedToolCall records a failed tool invocation that is rejected before
// dispatch, for example by request-level allowlists or invalid arguments. It
// intentionally does not execute the tool.
func RecordRejectedToolCall(ctx context.Context, name, toolCallID, errType, message string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	start := time.Now()
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, rejectedToolTelemetryName),
		attribute.String(genai.AttrToolType, genai.ToolTypeFunction),
	}
	if toolCallID != "" {
		attrs = append(attrs, attribute.String(genai.AttrToolCallID, toolCallID))
	}
	if errType == "" {
		errType = "tool_rejected"
	}
	if message == "" {
		message = errType
	}
	tracer := tracing.GenAITracer(genai.InstrumentationName)
	ctx, span := tracer.Start(ctx, genai.OperationExecuteTool+" "+rejectedToolTelemetryName, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	span.SetStatus(codes.Error, message)
	span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
	span.End()
	metricAttrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, rejectedToolTelemetryName),
		attribute.String(genai.AttrToolType, genai.ToolTypeFunction),
		attribute.String(genai.AttrErrorType, errType),
	}
	recordToolDuration(ctx, time.Since(start).Seconds(), metricAttrs...)
}

// FailedToolResultForTelemetry detects structured tool failures for callers
// that reject tool calls before dispatch but still need telemetry.
func FailedToolResultForTelemetry(result string) (bool, string, string) {
	return failedToolResult(result)
}

func failedToolResult(result string) (bool, string, string) {
	var body map[string]any
	if json.Unmarshal([]byte(result), &body) != nil {
		return false, "", ""
	}
	success, ok := body["success"].(bool)
	if !ok || success {
		return false, "", ""
	}

	var tr ChatToolResult
	_ = json.Unmarshal([]byte(result), &tr)
	errType := tr.ErrorType
	if errType == "" {
		errType = "tool_error"
	}
	message := tr.Error
	if message == "" {
		message = errType
	}
	return true, errType, message
}

func toolType(ctx context.Context, _ Tool) string {
	// Registry tools are in-process functions. External Tool CRD/MCP execution is
	// handled by worker.ToolExecutor and can be modeled as extension later.
	return genai.ToolTypeFunction
}

func recordToolDuration(ctx context.Context, seconds float64, attrs ...attribute.KeyValue) {
	meter := tracing.GenAIMeter(genai.InstrumentationName)
	histogram, err := meter.Float64Histogram(
		genai.MetricExecuteToolDuration,
		metric.WithUnit(genai.UnitSeconds),
		metric.WithExplicitBucketBoundaries(genai.ToolDurationBuckets...),
	)
	if err != nil {
		return
	}
	histogram.Record(ctx, seconds, metric.WithAttributes(attrs...))
}

// ToLLMTools converts the registry to LLM tool definitions
func (r *Registry) ToLLMTools(names []string) []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]llm.Tool, 0)
	for _, name := range names {
		if tool, ok := r.tools[name]; ok {
			tools = append(tools, llm.Tool{
				Name:        tool.Name(),
				Description: tool.Description(),
				Parameters:  tool.Parameters(),
			})
		}
	}
	return tools
}

// DefaultRegistry is the default tool registry with built-in tools
var DefaultRegistry = NewRegistry()

// RegisterBuiltinTools registers all built-in tools
func RegisterBuiltinTools() {
	DefaultRegistry.Register(NewWebSearchTool())
	DefaultRegistry.Register(NewCodeExecTool())
	DefaultRegistry.Register(NewFileReadTool())
	DefaultRegistry.Register(NewWebFetchTool())
	DefaultRegistry.Register(NewFileWriteTool())
}

// RegisterCoordinationTools registers coordination tools that require a K8s client
func RegisterCoordinationTools(k8sClient client.Client) {
	DefaultRegistry.Register(NewDelegateTaskTool(k8sClient))
	DefaultRegistry.Register(NewWaitForTasksTool(k8sClient))
	DefaultRegistry.Register(NewCreateContainerTaskTool(k8sClient))
	DefaultRegistry.Register(NewCancelTaskTool(k8sClient))
	DefaultRegistry.Register(NewSendMessageTool())
	DefaultRegistry.Register(NewCheckMessagesTool())
	DefaultRegistry.Register(NewCreatePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewCheckPullRequestCITool(k8sClient))
	DefaultRegistry.Register(NewMergePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewAutoMergePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewReviewPullRequestTool(k8sClient))
	DefaultRegistry.Register(NewPostReviewCommentTool(k8sClient))
	DefaultRegistry.Register(NewCheckPRReviewMarkerTool(k8sClient))
	DefaultRegistry.Register(NewListIssuesTool(k8sClient))
	DefaultRegistry.Register(NewListPullRequestsTool(k8sClient))
	DefaultRegistry.Register(NewGetIssueTool(k8sClient))
	DefaultRegistry.Register(NewCommentOnIssueTool(k8sClient))
	DefaultRegistry.Register(NewCreateAgentTool(k8sClient))
	DefaultRegistry.Register(NewDeleteAgentTool(k8sClient))
	DefaultRegistry.Register(NewUpdatePlanTool())
	DefaultRegistry.Register(NewRecallMemoryTool())
	DefaultRegistry.Register(NewRememberMemoryTool())
	DefaultRegistry.Register(NewProposeMemoryTool())
	DefaultRegistry.Register(NewSearchTranscriptTool())
}

// RegisterChatTools registers the chat/management tools into the given registry.
func RegisterChatTools(r *Registry) {
	r.Register(&CreateAITaskTool{})
	r.Register(&CreatePRMonitorTool{})
	r.Register(&CreateContainerTaskTool{})
	r.Register(&CreateAgentTaskTool{})
	r.Register(&CheckTaskProgressTool{})
	r.Register(&FetchTaskOutputTool{})
	r.Register(&WaitForTaskTool{})
	r.Register(&ChatCancelTaskTool{})
	r.Register(&ListAgentsTool{})
	r.Register(&ListToolsTool{})
	r.Register(&ListTasksTool{})
	r.Register(&ChatCreateAgentTool{})
	r.Register(&UpdateAgentTool{})
	r.Register(&ChatDeleteAgentTool{})
	r.Register(&CreateToolCRDTool{})
	r.Register(&DeleteToolTool{})
	r.Register(&DeleteSessionTool{})
}

// RegisterChatToolsDefault registers chat tools into DefaultRegistry for use by the proxy.
func RegisterChatToolsDefault() {
	RegisterChatTools(DefaultRegistry)
}

// RegisterProxyPRTools registers the GitHub PR coordination tools that the
// Anthropic and OpenAI proxies advertise in coordinatorProxyTools but that
// RegisterChatTools does not provide. Without this the proxy lists the tools
// for the model, ToLLMTools silently drops them (they are missing from the
// registry), and the model gets back "tool not available in this request"
// when it tries to open the PR after all the real work is done.
//
// Callers must invoke this once after the controller manager's client is
// available. Tests that exercise injectOrkaTools should also call this so the
// advertised tool set matches the runtime registration set.
func RegisterProxyPRTools(k8sClient client.Client) {
	DefaultRegistry.Register(NewCreatePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewCheckPullRequestCITool(k8sClient))
}

// ChatToolNames returns the names of all chat tools in registration order.
func ChatToolNames() []string {
	return []string{
		createAITaskToolName,
		createPRMonitorToolName,
		createContainerTaskToolName,
		createAgentTaskToolName,
		checkTaskProgressToolName, fetchTaskOutputToolName, waitForTaskToolName, cancelTaskToolName, listAgentsToolName, listToolsToolName, listTasksToolName, createAgentToolName, updateAgentToolName, "delete_agent",
		createToolCRDToolName,
		deleteToolToolName,
		deleteSessionToolName,
	}
}

func init() {
	RegisterBuiltinTools()
	RegisterChatToolsDefault()
}
