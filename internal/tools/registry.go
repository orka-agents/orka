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
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes"
)

// MemoryReader is the least-privilege store dependency required by recall_memory.
type MemoryReader interface {
	ListMemories(context.Context, store.MemoryFilter) ([]store.Memory, error)
}

// MemoryProposalWriter is the least-privilege store dependency required by
// remember and propose_memory.
type MemoryProposalWriter interface {
	CreateMemoryProposal(context.Context, *store.MemoryProposal) error
}

// TranscriptSearcher is the least-privilege store dependency required by
// search_transcript.
type TranscriptSearcher interface {
	SearchTranscript(context.Context, store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error)
}

// TaskMessageStore exposes only message send and inbox access to tools.
// Brokered contexts must supply an implementation bound to the authenticated Task.
type TaskMessageStore interface {
	SendMessage(context.Context, *store.Message) error
	GetMessages(context.Context, string, string, string, bool) ([]store.Message, error)
}

// LinkedAccountCredential is a person's linked-account credential resolved
// for one built-in tool call, with the non-secret identity of the Connection
// that supplied it.
type LinkedAccountCredential struct {
	AccessToken   string
	Provider      string
	ConnectionUID string
}

// LinkedAccountCredentials resolves a linked-account credential for a
// built-in tool. BuiltinToolCredential reports false when the Task has no
// Connection bound for the tool, in which case the tool keeps its own
// credential path; an error means a Connection is bound but cannot be used
// now, and the tool fails closed rather than falling back.
type LinkedAccountCredentials interface {
	BuiltinToolCredential(ctx context.Context, toolName string) (LinkedAccountCredential, bool, error)
}

// ToolContext provides dependencies for tools that need K8s client access or other services.
type ToolContext struct {
	Client client.Client
	// PolicyReader bypasses informer lag when coordination tools resolve Task,
	// Agent, Provider, Tool, and AgentRuntime policy. Writes continue through Client.
	PolicyReader              client.Reader
	KubeClient                kubernetes.Interface
	Namespace                 string
	SessionID                 string
	TaskID                    string
	TaskUID                   string
	ParentTaskID              string
	AgentName                 string
	ToolCallID                string
	OperationID               string
	Tenant                    string
	Provider                  string
	ProviderType              string
	ExecutionMode             executionmode.Mode
	WatchNamespace            string
	EnforceNamespaceIsolation bool
	// Brokered marks an authenticated controller-side MCP broker execution.
	// Broker-aware tools must fail closed on missing request-scoped dependencies
	// instead of falling back to controller process environment or credentials.
	Brokered bool
	// TaskProvenanceProtected reports that Task provenance admission reserves
	// controller-authenticated lineage metadata from direct namespace writes.
	TaskProvenanceProtected bool
	// ExternalEffects and OperationID let brokered coordination tools bind
	// created resources to the controller-owned effect receipt for this exact
	// tool call. The receipt remains authoritative when provenance admission is
	// disabled and Task metadata is mutable.
	ExternalEffects store.ExternalEffectStore
	// Least-privilege durable dependencies for brokered memory tools.
	MemoryReader         MemoryReader
	MemoryProposalWriter MemoryProposalWriter
	TranscriptSearcher   TranscriptSearcher
	// RepositoryValidationBindings stores the controller-owned command binding
	// created before a repository validation Task.
	RepositoryValidationBindings RepositoryValidationBindingStore
	// ResultStore for fetching task outputs (store.ResultStore)
	ResultStore interface {
		GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	}
	// MessageStore for inter-agent messaging when tools execute in-process from the controller broker.
	MessageStore TaskMessageStore
	// GatewayReplySender is injected only for a durably eligible gateway Task.
	GatewayReplySender GatewayReplySender
	// SessionDeleter for deleting sessions (controller.SessionManager)
	SessionDeleter interface {
		DeleteSession(ctx context.Context, namespace, sessionID string) error
	}
	// Task creation helpers provided by the chat executor
	GenerateTaskName    func() string
	TaskLabels          func() map[string]string
	CheckTaskLimit      func() *ChatToolError
	AuthorizeTaskCreate func(context.Context, *corev1alpha1.Task) *ChatToolError
	// SealTaskCreate runs right after a Task this tool created exists, so
	// the API can bind server-assigned identity (the UID) to the requester
	// it stamped. A failure is logged by the sealer; the Task stays.
	SealTaskCreate       func(context.Context, client.Client, *corev1alpha1.Task) error
	AuthorizeTaskDelete  func(context.Context, *corev1alpha1.Task) *ChatToolError
	AuthorizeAgentCreate func(context.Context, *corev1alpha1.Agent) *ChatToolError
	// AuthorizeAgentInitialTask preflights the combined Agent/Task operation
	// before creating the Agent. Full Task authorization still runs later.
	AuthorizeAgentInitialTask func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeAgentUpdate      func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeAgentDelete      func(context.Context, *corev1alpha1.Agent) *ChatToolError
	AuthorizeSecretRead       func(context.Context, string, string) *ChatToolError
	AuthorizePodLogs          func(context.Context, string, string) error
	// AuthorizeCodeExecResources checks creation and cleanup permissions for
	// the complete temporary resource set before code_exec creates anything.
	AuthorizeCodeExecResources     func(context.Context, []client.Object) error
	RequireSecretReadAuthorization bool
	// RequireGitHubTaskCredentials disables controller-global repository and
	// credential fallback for external GitHub tool calls.
	RequireGitHubTaskCredentials bool
	// LinkedAccounts resolves the requester's linked-account credential for
	// built-in tools the controller executes on their behalf. Only the
	// controller sets it; worker Pods never hold one and keep their own
	// credential path.
	LinkedAccounts LinkedAccountCredentials
	// Requester is the verified person this call acts for: the signed-in
	// caller for chat, the Task's verified requester for the broker. Tools
	// that show or use linked accounts read it; nothing else does.
	Requester *corev1alpha1.RequestedBy
	// AuthorizeConnectorRead gates list_connections for callers whose
	// delegated token may not read the person's linked accounts.
	AuthorizeConnectorRead func() *ChatToolError
	IncrementTasks         func()
	// CreatedTasks records the Tasks this turn's tools created: the only
	// Tasks a linked account may be scoped by outside a Task. The API
	// owns one per turn and shares it across the turn's tool calls; a
	// context without one never accepts a task_name outside a Task.
	CreatedTasks             *CreatedTasks
	ApprovalEmitter          func(context.Context, approvals.ApprovalTarget) error
	ApprovalTargetSpecDigest func(context.Context, string) (string, error)
	ApprovalTargetArguments  func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	ApprovalTargetRefresh    func(context.Context, string, *corev1alpha1.Tool) error
}

// CreatedTasks is the set of Tasks one turn's tools created, by namespace
// and name with the UID the API server assigned. It is held by pointer so
// the per-call copies of a ToolContext share it.
type CreatedTasks struct {
	mu   sync.Mutex
	uids map[string]string
	// workspaces digests each created Task's spec.workspace as created, so
	// a later linked call can refuse a Task whose repository scope was
	// changed after this turn chose it.
	workspaces map[string]string
}

// NewCreatedTasks returns an empty set for one turn.
func NewCreatedTasks() *CreatedTasks {
	return &CreatedTasks{uids: map[string]string{}, workspaces: map[string]string{}}
}

// WorkspaceDigest is a stable digest of a Task's workspace as the turn
// created it; "" when the Task has no workspace.
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

// RecordCreatedTask notes a Task this turn's tools created, so a later
// call may name it as the repository scope for the requester's linked
// account (see CreatedTaskUID). Without a set the record is dropped.
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

// CreatedTaskWorkspaceDigest returns the workspace digest recorded for a
// Task this turn created, or "" when none was recorded.
func (tc *ToolContext) CreatedTaskWorkspaceDigest(namespace, name string) string {
	if tc == nil || tc.CreatedTasks == nil {
		return ""
	}
	tc.CreatedTasks.mu.Lock()
	defer tc.CreatedTasks.mu.Unlock()
	return tc.CreatedTasks.workspaces[createdTaskKey(namespace, name)]
}

// CreatedTaskUID returns the UID of the Task this turn's tools created
// under namespace and name, or "" when this turn created no such Task.
func (tc *ToolContext) CreatedTaskUID(namespace, name string) string {
	if tc == nil || tc.CreatedTasks == nil {
		return ""
	}
	tc.CreatedTasks.mu.Lock()
	defer tc.CreatedTasks.mu.Unlock()
	return tc.CreatedTasks.uids[createdTaskKey(namespace, name)]
}

// CreatedTaskByName resolves a Task this turn created by name alone, as
// the GitHub tools receive it: the one recorded entry with that name, in
// whichever namespace the creation tool was told. Two entries with the
// same name in different namespaces are ambiguous and resolve to nothing.
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

// Names returns all registered tool names in stable order.
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

// Execute executes a tool by name. It is the DRY instrumentation point for
// built-in registry tools used by chat, proxy-compatible handlers, and workers.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	tool, ok := r.Get(name)
	// call checks argument types inside the instrumented path, so a rejected
	// call is recorded like any other tool error. The check runs on the
	// arguments as sent, before decoding stringified objects collapses
	// duplicate keys.
	call := func(ctx context.Context) (string, error) {
		normalized, err := normalizeArgTypes(tool, args)
		if err != nil {
			return "", err
		}
		return tool.Execute(ctx, decodeStringifiedObjectArgs(tool, normalized))
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
	// Registry tools are in-process functions. External Tool CRD/MCP execution is
	// handled by worker.ToolExecutor and can be modeled as extension later.
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
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String(genai.AttrErrorType, "tool_not_found"))
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
		errType := fmt.Sprintf("%T", err)
		if spanRecording {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
		}
		metricErrType = errType
	} else if spanRecording || meterActive {
		if failed, errType, message := failedToolResult(result); failed {
			if spanRecording {
				span.SetStatus(codes.Error, message)
				span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
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

// recordToolDuration records one gen_ai.client.operation.duration sample on the
// registry's cached histogram. An empty errType records a success sample with
// no error.type attribute.
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

// RecordRejectedToolCall records a failed tool invocation that is rejected before
// dispatch, for example by request-level allowlists or invalid arguments. It
// intentionally does not execute the tool.
func RecordRejectedToolCall(ctx context.Context, name, toolCallID, errType, message string) {
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
	if message == "" {
		message = errType
	}
	tracer := tracing.GenAITracer(genai.InstrumentationName)
	ctx, span := tracer.Start(ctx, genai.OperationExecuteTool+" "+rejectedToolTelemetryName, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	span.SetStatus(codes.Error, message)
	span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
	span.End()
	DefaultRegistry.recordToolDuration(ctx, time.Since(start).Seconds(), rejectedToolTelemetryName, genai.ToolTypeFunction, errType)
}

// FailedToolResultForTelemetry detects structured tool failures for callers
// that reject tool calls before dispatch but still need telemetry.
func FailedToolResultForTelemetry(result string) (bool, string, string) {
	return failedToolResult(result)
}

func failedToolResult(result string) (bool, string, string) {
	// Most successful tool results are compact JSON with "success":true and no
	// false literal. Avoid the generic map unmarshal on that hot path; only parse
	// when a structured failure is possible.
	if !strings.Contains(result, "false") {
		return false, "", ""
	}
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &body) != nil {
		return false, "", ""
	}
	successJSON, ok := body["success"]
	if !ok {
		return false, "", ""
	}
	var success bool
	if json.Unmarshal(successJSON, &success) != nil || success {
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

func registryToolKind(name string) string {
	if name == delegateTaskToolName {
		return tracing.ToolKindDelegate
	}
	return tracing.ToolKindBuiltin
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
	DefaultRegistry.Register(NewRequestApprovalTool())
	DefaultRegistry.Register(NewReplyInConversationTool())
}

// RegisterCoordinationTools registers coordination tools that require a K8s client
func RegisterCoordinationTools(k8sClient client.Client, mode executionmode.Mode) {
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
	DefaultRegistry.Register(NewCreateAgentTool(k8sClient, mode))
	DefaultRegistry.Register(NewDeleteAgentTool(k8sClient))
	DefaultRegistry.Register(NewUpdatePlanTool())
	DefaultRegistry.Register(NewRecallMemoryTool())
	DefaultRegistry.Register(NewRememberMemoryTool())
	DefaultRegistry.Register(NewProposeMemoryTool())
	DefaultRegistry.Register(NewSearchTranscriptTool())
}

// RegisterBrokeredCoordinationTools registers only coordination tools whose
// implementations can execute safely inside the authenticated controller MCP
// broker. Registration is idempotent because Registry.Register replaces the
// implementation for a stable tool name.
//
// Do not broaden this list with worker-local tools or tools that obtain forge,
// repository, or ServiceAccount credentials from process environment. Those
// implementations are not request-scoped in the controller process.
func RegisterBrokeredCoordinationTools(r *Registry, k8sClient client.Client) error {
	if r == nil {
		return fmt.Errorf("brokered coordination tool registry is required")
	}
	if k8sClient == nil {
		return fmt.Errorf("brokered coordination tools require a Kubernetes client")
	}
	r.Register(NewDelegateTaskTool(k8sClient))
	// MCP clients have shorter request deadlines than native worker tool calls.
	// Keep each brokered poll bounded even when a model omits or exceeds timeout.
	r.Register(&WaitForTasksTool{k8sClient: k8sClient, maxWait: RepositoryValidationWaitTimeout})
	r.Register(NewRunValidationTool(k8sClient))
	r.Register(NewSendMessageTool())
	r.Register(NewCheckMessagesTool())
	r.Register(NewRecallMemoryTool())
	r.Register(NewRememberMemoryTool())
	r.Register(NewProposeMemoryTool())
	r.Register(NewSearchTranscriptTool())
	return nil
}

// RegisterBrokeredConnectionTools registers the linked-account tools the
// ACP broker offers only when connectors are enabled on the controller.
func RegisterBrokeredConnectionTools(r *Registry) error {
	if r == nil {
		return fmt.Errorf("registry is required")
	}
	r.Register(&ListConnectionsTool{})
	return nil
}

// RegisterBrokeredWebTools registers public web reads whose implementations
// are safe to execute inside the controller MCP broker. Registration is
// idempotent because Registry.Register replaces the implementation for a
// stable tool name.
func RegisterBrokeredWebTools(r *Registry) error {
	if r == nil {
		return fmt.Errorf("brokered web tool registry is required")
	}
	r.Register(NewBrokeredWebSearchTool())
	r.Register(NewBrokeredWebFetchTool())
	return nil
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
	r.Register(&ListConnectionsTool{})
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

// ProxyPRToolNames are the tools RegisterProxyPRTools adds. Only the
// controller registers them in its default registry; a native worker has
// them only as coordination tools.
func ProxyPRToolNames() []string {
	return []string{createPullRequestToolName, checkPullRequestCIToolName}
}

// KnownBuiltInToolNames returns every built-in tool name known to Orka, including
// tools registered in the default proxy registry and coordination tools that are
// registered in worker processes. Controller-side validation uses this to reject
// approvalRequiredTools entries that would be handled as built-ins rather than
// Tool CRDs.
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

// decodeStringifiedObjectArgs replaces top-level arguments that the tool schema
// declares as objects, but that the model sent as JSON-encoded strings, with
// the decoded object. Models often stringify nested objects, and tools that
// type-switch on map[string]any would otherwise ignore them. Every other value
// is left for the tool to validate.
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

// ChatToolNames returns the names of all chat tools in registration order.
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

// CoordinationToolNames returns the names of all coordination tools registered by
// RegisterCoordinationTools in worker processes.
func CoordinationToolNames() []string {
	return aitools.CoordinationToolNames()
}

func init() {
	RegisterBuiltinTools()
	RegisterChatToolsDefault()
}
