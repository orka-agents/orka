/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/tracing/genai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes"
)

type MemoryReader interface {
	ListMemories(context.Context, store.MemoryFilter) ([]store.Memory, error)
}

type MemoryRecallRecorder interface {
	RecordMemoryRecall(context.Context, store.MemoryRecall) error
}

type MemoryProposalWriter interface {
	CreateMemoryProposal(context.Context, *store.MemoryProposal) error
}

type TranscriptSearcher interface {
	SearchTranscript(context.Context, store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error)
}

type TaskMessageStore interface {
	SendMessage(context.Context, *store.Message) error
	GetMessages(context.Context, string, string, string, bool) ([]store.Message, error)
}

type LinkedAccountCredential struct {
	AccessToken   string
	Provider      string
	ConnectionUID string
}

type LinkedAccountCredentials interface {
	BuiltinToolCredential(ctx context.Context, toolName string) (LinkedAccountCredential, bool, error)
}

type ToolContext struct {
	Client                       client.Client
	PolicyReader                 client.Reader
	KubeClient                   kubernetes.Interface
	Namespace                    string
	SessionID                    string
	TaskID                       string
	TaskUID                      string
	ParentTaskID                 string
	AgentName                    string
	AgentUID                     string
	ToolCallID                   string
	OperationID                  string
	Tenant                       string
	Provider                     string
	ProviderNamespace            string
	ProviderType                 string
	ExecutionMode                executionmode.Mode
	WatchNamespace               string
	EnforceNamespaceIsolation    bool
	Brokered                     bool
	TaskProvenanceProtected      bool
	ExternalEffects              store.ExternalEffectStore
	ExecutionEvents              store.DeduplicatingExecutionEventStore
	MemoryReader                 MemoryReader
	MemoryRecallRecorder         MemoryRecallRecorder
	MemoryProposalWriter         MemoryProposalWriter
	TranscriptSearcher           TranscriptSearcher
	RepositoryValidationBindings RepositoryValidationBindingStore
	ResultStore                  interface {
		GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	}
	MessageStore       TaskMessageStore
	GatewayReplySender GatewayReplySender
	SessionDeleter     interface {
		DeleteSession(ctx context.Context, namespace, sessionID string) error
	}
	GenerateTaskName               func() string
	TaskLabels                     func() map[string]string
	CheckTaskLimit                 func() *ChatToolError
	AuthorizeTaskCreate            func(context.Context, *corev1alpha1.Task) *ChatToolError
	AuthorizeAgentReference        func(context.Context, *corev1alpha1.Agent) *ChatToolError
	SealTaskCreate                 func(context.Context, client.Client, *corev1alpha1.Task) error
	AuthorizeTaskDelete            func(context.Context, *corev1alpha1.Task) *ChatToolError
	AuthorizeAgentCreate           func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeAgentInitialTask      func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeAgentUpdate           func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeAgentDelete           func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeSecretRead            func(context.Context, string, string) *ChatToolError
	AuthorizePodLogs               func(context.Context, string, string) error
	AuthorizeCodeExecResources     func(context.Context, []client.Object) error
	RequireSecretReadAuthorization bool
	RequireGitHubTaskCredentials   bool
	LinkedAccounts                 LinkedAccountCredentials
	Requester                      *corev1alpha1.RequestedBy
	AuthorizeConnectorRead         func() *ChatToolError
	DashboardEnabled               bool
	IncrementTasks                 func()
	CreatedTasks                   *CreatedTasks
	ApprovalEmitter                func(context.Context, approvals.ApprovalTarget) error
	ApprovalTargetSpecDigest       func(context.Context, string) (string, error)
	ApprovalTargetArguments        func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	ApprovalTargetRefresh          func(context.Context, string, *corev1alpha1.Tool) error
}

type CreatedTasks struct {
	mu         sync.Mutex
	uids       map[string]string
	workspaces map[string]string
}

func NewCreatedTasks() *CreatedTasks {
	return &CreatedTasks{uids: map[string]string{}, workspaces: map[string]string{}}
}

func WorkspaceDigest(task *corev1alpha1.Task) string {
	if task == nil || task.Spec.Workspace == nil {
		return ""
	}
	raw, err := json.Marshal(task.Spec.Workspace)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func createdTaskKey(namespace, name string) string {
	return strings.TrimSpace(namespace) + "/" + strings.TrimSpace(name)
}

func (tc *ToolContext) RecordCreatedTask(task *corev1alpha1.Task) {
	if tc == nil || tc.CreatedTasks == nil || task == nil || strings.TrimSpace(task.Name) == "" {
		return
	}
	tc.CreatedTasks.mu.Lock()
	defer tc.CreatedTasks.mu.Unlock()
	key := createdTaskKey(task.Namespace, task.Name)
	tc.CreatedTasks.uids[key] = string(task.UID)
	tc.CreatedTasks.workspaces[key] = WorkspaceDigest(task)
}

func (tc *ToolContext) CreatedTaskWorkspaceDigest(namespace, name string) string {
	if tc == nil || tc.CreatedTasks == nil {
		return ""
	}
	tc.CreatedTasks.mu.Lock()
	defer tc.CreatedTasks.mu.Unlock()
	return tc.CreatedTasks.workspaces[createdTaskKey(namespace, name)]
}

func (tc *ToolContext) CreatedTaskUID(namespace, name string) string {
	if tc == nil || tc.CreatedTasks == nil {
		return ""
	}
	tc.CreatedTasks.mu.Lock()
	defer tc.CreatedTasks.mu.Unlock()
	return tc.CreatedTasks.uids[createdTaskKey(namespace, name)]
}

func (tc *ToolContext) CreatedTaskByName(name string) (namespace, uid string, ok bool) {
	if tc == nil || tc.CreatedTasks == nil {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	tc.CreatedTasks.mu.Lock()
	defer tc.CreatedTasks.mu.Unlock()
	matches := 0
	for key, recorded := range tc.CreatedTasks.uids {
		ns, recordedName, found := strings.Cut(key, "/")
		if !found || recordedName != name {
			continue
		}
		matches++
		namespace, uid = ns, recorded
	}
	if matches != 1 {
		return "", "", false
	}
	return namespace, uid, true
}

type toolContextKey struct{}

func WithToolContext(ctx context.Context, tc *ToolContext) context.Context {
	return context.WithValue(ctx, toolContextKey{}, tc)
}

func GetToolContext(ctx context.Context) *ToolContext {
	tc, _ := ctx.Value(toolContextKey{}).(*ToolContext)
	return tc
}

type ChatToolError struct {
	Type       string `json:"errorType"`
	Message    string `json:"error"`
	Suggestion string `json:"suggestion"`
}

func (e *ChatToolError) Error() string { return e.Message }

type ChatToolResult struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorType  string `json:"errorType,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

func marshalChatResult(r ChatToolResult) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("failed to marshal chat tool result: %w", err)
	}
	return string(b), nil
}

func ChatToolSuccess(data any) (string, error) {
	return marshalChatResult(ChatToolResult{Success: true, Data: data})
}

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

type Tool interface {
	Name() string

	Description() string

	Parameters() json.RawMessage

	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool

	telemetryMu            sync.Mutex
	tracerProvider         any
	tracer                 trace.Tracer
	meterProvider          any
	toolDurationHistogram  metric.Float64Histogram
	toolDurationMetricOpts map[toolDurationMetricKey]metric.MeasurementOption
}

type toolDurationMetricKey struct {
	toolName string
	toolType string
	errType  string
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

func (r *Registry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	tool, ok := r.Get(name)
	call := func(ctx context.Context) (string, error) {
		normalized, err := NormalizeToolArguments(tool, args)
		if err != nil {
			return "", err
		}
		return tool.Execute(ctx, normalized)
	}
	if telemetryDisabled() {
		if !ok {
			return "", &ToolNotFoundError{Name: name}
		}
		return call(ctx)
	}

	toolTelemetryName := name
	if !ok {
		toolTelemetryName = unknownToolTelemetryName
	}
	toolTypeValue := genai.ToolTypeFunction
	toolKind := registryToolKind(name)
	meterActive := tracing.GlobalMeterProviderActive()
	var start time.Time
	if meterActive {
		start = time.Now()
	}

	tracer := r.genAITracer()
	spanName := genai.OperationExecuteTool + " " + toolTelemetryName
	deferStartAttributes := tracing.IsDefaultGlobalTracerProvider(tracing.GlobalTracerProvider())
	var span trace.Span
	if deferStartAttributes {
		ctx, span = tracer.Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindInternal))
	} else {
		attrs := registryToolSpanAttributes(ctx, toolTelemetryName, tool, ok, toolTypeValue, toolKind)
		ctx, span = tracer.Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	}
	spanRecording := span.IsRecording()
	if spanRecording && deferStartAttributes {
		span.SetAttributes(registryToolSpanAttributes(ctx, toolTelemetryName, tool, ok, toolTypeValue, toolKind)...)
	}
	if !spanRecording && !meterActive {
		if !ok {
			span.End()
			return "", &ToolNotFoundError{Name: name}
		}
		result, err := call(ctx)
		span.End()
		return result, err
	}
	defer span.End()

	if !ok {
		err := &ToolNotFoundError{Name: name}
		if spanRecording {
			tracing.RecordSpanErrorType(span, "tool_not_found")
		}
		if meterActive {
			r.recordToolDuration(ctx, time.Since(start).Seconds(), toolTelemetryName, toolTypeValue, "tool_not_found")
		}
		return "", err
	}

	result, err := call(ctx)
	duration := time.Since(start).Seconds()
	if spanRecording {
		span.SetAttributes(attribute.Int(tracing.AttrToolResultSizeBytes, len(result)))
	}
	metricErrType := ""
	if err != nil {
		errType := tracing.ErrorType(err)
		if spanRecording {
			tracing.RecordSpanErrorType(span, errType)
		}
		metricErrType = errType
	} else if spanRecording || meterActive {
		if failed, errType := failedToolResult(result); failed {
			errType = toolErrorClass(errType)
			if spanRecording {
				tracing.RecordSpanErrorType(span, errType)
			}
			metricErrType = errType
		}
	}
	if meterActive {
		r.recordToolDuration(ctx, duration, toolTelemetryName, toolTypeValue, metricErrType)
	}
	return result, err
}

func registryToolSpanAttributes(ctx context.Context, toolTelemetryName string, tool Tool, ok bool, toolTypeValue, toolKind string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 10)
	attrs = append(attrs,
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, toolTelemetryName),
		attribute.String(genai.AttrToolType, toolTypeValue),
		attribute.String(tracing.AttrToolName, toolTelemetryName),
		attribute.String(tracing.AttrToolKind, toolKind),
	)
	if tc := GetToolContext(ctx); tc != nil {
		tenant := tc.Tenant
		if tenant == "" {
			tenant = tc.Namespace
		}
		if tc.TaskID != "" {
			attrs = append(attrs, attribute.String(tracing.AttrTaskID, tc.TaskID))
		}
		if tc.Namespace != "" {
			attrs = append(attrs, attribute.String(tracing.AttrTaskNamespace, tc.Namespace))
		}
		if tenant != "" {
			attrs = append(attrs, attribute.String(tracing.AttrTenant, tenant))
		}
		if tc.ToolCallID != "" {
			attrs = append(attrs, attribute.String(genai.AttrToolCallID, tc.ToolCallID))
		}
		if tc.AgentUID != "" {
			attrs = append(attrs, attribute.String(genai.AttrAgentID, tc.AgentUID))
		}
	}
	if ok {
		if description := tool.Description(); description != "" {
			attrs = append(attrs, attribute.String(genai.AttrToolDescription, description))
		}
	}
	return attrs
}

func (r *Registry) genAITracer() trace.Tracer {
	provider := tracing.GlobalTracerProvider()
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	if r.tracer != nil && tracing.SameProvider(r.tracerProvider, provider) {
		return r.tracer
	}
	r.tracerProvider = provider
	r.tracer = provider.Tracer(genai.InstrumentationName, trace.WithSchemaURL(genai.SchemaURL))
	return r.tracer
}

func (r *Registry) getToolDurationHistogram() (metric.Float64Histogram, bool) {
	if !tracing.GlobalMeterProviderActive() {
		return nil, false
	}
	provider := tracing.GlobalMeterProvider()
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	if r.toolDurationHistogram != nil && tracing.SameProvider(r.meterProvider, provider) {
		return r.toolDurationHistogram, true
	}
	meter := provider.Meter(genai.InstrumentationName, metric.WithSchemaURL(genai.SchemaURL))
	histogram, err := meter.Float64Histogram(
		genai.MetricExecuteToolDuration,
		metric.WithUnit(genai.UnitSeconds),
		metric.WithExplicitBucketBoundaries(genai.ToolDurationBuckets...),
	)
	if err != nil {
		r.meterProvider = provider
		r.toolDurationHistogram = nil
		return nil, false
	}
	r.meterProvider = provider
	r.toolDurationHistogram = histogram
	return histogram, true
}

func (r *Registry) toolDurationMetricOption(toolName, toolType, errType string) metric.MeasurementOption {
	key := toolDurationMetricKey{toolName: toolName, toolType: toolType, errType: errType}
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	if r.toolDurationMetricOpts == nil {
		r.toolDurationMetricOpts = make(map[toolDurationMetricKey]metric.MeasurementOption)
	}
	if opt, ok := r.toolDurationMetricOpts[key]; ok {
		return opt
	}
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, toolName),
		attribute.String(genai.AttrToolType, toolType),
	}
	if errType != "" {
		attrs = append(attrs, attribute.String(genai.AttrErrorType, errType))
	}
	opt := metric.WithAttributeSet(attribute.NewSet(attrs...))
	r.toolDurationMetricOpts[key] = opt
	return opt
}

func (r *Registry) recordToolDuration(ctx context.Context, seconds float64, toolName, toolType, errType string) {
	histogram, ok := r.getToolDurationHistogram()
	if !ok {
		return
	}
	histogram.Record(ctx, seconds, r.toolDurationMetricOption(toolName, toolType, errType))
}

func telemetryDisabled() bool {
	return tracing.GlobalTracerProviderExplicitNoop() && !tracing.GlobalMeterProviderActive()
}

func RecordRejectedToolCall(ctx context.Context, name, toolCallID, errType string) {
	if strings.TrimSpace(name) == "" || telemetryDisabled() {
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
	errType = toolErrorClass(errType)
	tracer := tracing.GenAITracer(genai.InstrumentationName)
	ctx, span := tracer.Start(ctx, genai.OperationExecuteTool+" "+rejectedToolTelemetryName, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	tracing.RecordSpanErrorType(span, errType)
	span.End()
	DefaultRegistry.recordToolDuration(ctx, time.Since(start).Seconds(), rejectedToolTelemetryName, genai.ToolTypeFunction, errType)
}

func FailedToolResultForTelemetry(result string) (bool, string) {
	return failedToolResult(result)
}

func failedToolResult(result string) (bool, string) {
	if !strings.Contains(result, "false") {
		return false, ""
	}
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &body) != nil {
		return false, ""
	}
	successJSON, ok := body["success"]
	if !ok {
		return false, ""
	}
	var success bool
	if json.Unmarshal(successJSON, &success) != nil || success {
		return false, ""
	}

	var tr ChatToolResult
	_ = json.Unmarshal([]byte(result), &tr)
	if tr.ErrorType == "" {
		return true, "tool_error"
	}
	return true, tr.ErrorType
}

func toolErrorClass(errorType string) string {
	switch errorType {
	case "invalid_arguments", "internal_error", "not_found", "already_exists",
		"permission_denied", "authorization_failed", "limit_reached", "api_error",
		"cleanup_failed", "missing_publication_credential", "missing_workspace",
		"no_identity", "pause_label_rejected", "permission_error", "unauthorized_tool",
		"unsupported_container_workspace_field", "unsupported_custom_image_publication",
		"unsupported_schedule", "validation_not_authorized", "validation_task_conflict",
		"tool_not_found", "tool_not_enabled", "tool_rejected":
		return errorType
	default:
		return "tool_error"
	}
}

func registryToolKind(name string) string {
	if name == delegateTaskToolName {
		return tracing.ToolKindDelegate
	}
	return tracing.ToolKindBuiltin
}

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

var DefaultRegistry = NewRegistry()

func RegisterBuiltinTools() {
	DefaultRegistry.Register(NewWebSearchTool())
	DefaultRegistry.Register(NewCodeExecTool())
	DefaultRegistry.Register(NewFileReadTool())
	DefaultRegistry.Register(NewWebFetchTool())
	DefaultRegistry.Register(NewFileWriteTool())
	DefaultRegistry.Register(NewRequestApprovalTool())
	DefaultRegistry.Register(NewReplyInConversationTool())
}

func RegisterCoordinationTools(k8sClient client.Client, mode executionmode.Mode) {
	DefaultRegistry.Register(NewDelegateTaskTool(k8sClient))
	DefaultRegistry.Register(NewWaitForTasksTool(k8sClient))
	DefaultRegistry.Register(NewCreateContainerTaskTool(k8sClient))
	DefaultRegistry.Register(NewCancelTaskTool())
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
	DefaultRegistry.Register(NewCreateAgentTool(k8sClient, mode))
	DefaultRegistry.Register(NewDeleteAgentTool(k8sClient))
	DefaultRegistry.Register(NewUpdatePlanTool())
	DefaultRegistry.Register(NewRecallMemoryTool())
	DefaultRegistry.Register(NewRememberMemoryTool())
	DefaultRegistry.Register(NewProposeMemoryTool())
	DefaultRegistry.Register(NewSearchTranscriptTool())
}

func RegisterBrokeredCoordinationTools(r *Registry, k8sClient client.Client) error {
	if r == nil {
		return fmt.Errorf("brokered coordination tool registry is required")
	}
	if k8sClient == nil {
		return fmt.Errorf("brokered coordination tools require a Kubernetes client")
	}
	r.Register(NewDelegateTaskTool(k8sClient))
	r.Register(&WaitForTasksTool{k8sClient: k8sClient, maxWait: RepositoryValidationWaitTimeout})
	r.Register(NewRunValidationTool(k8sClient))
	r.Register(NewSendMessageTool())
	r.Register(NewCheckMessagesTool())
	r.Register(NewRecallMemoryTool())
	r.Register(NewRememberMemoryTool())
	r.Register(NewProposeMemoryTool())
	r.Register(NewSearchTranscriptTool())
	r.Register(NewParkUntilTool())
	return nil
}

func RegisterBrokeredConnectionTools(r *Registry) error {
	if r == nil {
		return fmt.Errorf("registry is required")
	}
	r.Register(&ListConnectionsTool{})
	return nil
}

func RegisterBrokeredWebTools(r *Registry) error {
	if r == nil {
		return fmt.Errorf("brokered web tool registry is required")
	}
	r.Register(NewBrokeredWebSearchTool())
	r.Register(NewBrokeredWebFetchTool())
	return nil
}

func BrokeredBuiltInToolNames() []string {
	return []string{
		delegateTaskToolName, waitForTasksToolName, RunValidationToolName,
		sendMessageToolName, checkMessagesToolName,
		"recall_memory", "remember", "propose_memory", "search_transcript",
		ParkUntilToolName,
		webSearchToolName, webFetchToolName,
		ListConnectionsToolName,
	}
}

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
	r.Register(&ListConnectionsTool{})
	r.Register(&ChatCreateAgentTool{})
	r.Register(&UpdateAgentTool{})
	r.Register(&ChatDeleteAgentTool{})
	r.Register(&CreateToolCRDTool{})
	r.Register(&DeleteToolTool{})
	r.Register(&DeleteSessionTool{})
}

func RegisterChatToolsDefault() {
	RegisterChatTools(DefaultRegistry)
}

func RegisterProxyPRTools(k8sClient client.Client) {
	DefaultRegistry.Register(NewCreatePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewCheckPullRequestCITool(k8sClient))
}

func ProxyPRToolNames() []string {
	return []string{createPullRequestToolName, checkPullRequestCIToolName}
}

func KnownBuiltInToolNames() []string {
	seen := map[string]bool{}
	for _, group := range [][]string{DefaultRegistry.Names(), ChatToolNames(), CoordinationToolNames()} {
		for _, name := range group {
			if name != "" {
				seen[name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func NormalizeToolArguments(tool Tool, args json.RawMessage) (json.RawMessage, error) {
	normalized, err := normalizeArgTypes(tool, args)
	if err != nil {
		return nil, err
	}
	return decodeStringifiedObjectArgs(tool, normalized), nil
}

func decodeStringifiedObjectArgs(tool Tool, args json.RawMessage) json.RawMessage {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(args, &values); err != nil || len(values) == 0 {
		return args
	}
	var schema struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	changed := false
	for name, value := range values {
		var encoded string
		if json.Unmarshal(value, &encoded) != nil {
			continue
		}
		if schema.Properties == nil {
			if err := json.Unmarshal(tool.Parameters(), &schema); err != nil || schema.Properties == nil {
				return args
			}
		}
		if schema.Properties[name].Type != jsonSchemaTypeObject {
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(encoded), &object) != nil || object == nil {
			continue
		}
		values[name] = json.RawMessage(encoded)
		changed = true
	}
	if !changed {
		return args
	}
	decoded, err := json.Marshal(values)
	if err != nil {
		return args
	}
	return decoded
}

func ChatToolNames() []string {
	return []string{
		createAITaskToolName,
		createPRMonitorToolName,
		createContainerTaskToolName,
		createAgentTaskToolName,
		checkTaskProgressToolName, fetchTaskOutputToolName, waitForTaskToolName, cancelTaskToolName, listAgentsToolName, listToolsToolName, listTasksToolName, ListConnectionsToolName, createAgentToolName, updateAgentToolName, "delete_agent",
		createToolCRDToolName,
		deleteToolToolName,
		deleteSessionToolName,
	}
}

func CoordinationToolNames() []string {
	return aitools.CoordinationToolNames()
}

func init() {
	RegisterBuiltinTools()
	RegisterChatToolsDefault()
}
