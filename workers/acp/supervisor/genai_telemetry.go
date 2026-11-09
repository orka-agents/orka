package supervisor

import (
	"context"
	"regexp"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tracing/genai"
)

// GenAI telemetry derived from the ACP session stream and provider proxy.
//
// Every span and metric here is metadata only. Prompts, completions, tool
// titles, tool arguments and tool results never become attributes or events,
// whatever OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT says: content
// capture stays off and fails closed. Spans are written with explicit start
// timestamps when their outcome is known, so no span is left open on a
// broken stream.

const (
	// maxPromptGenAITools bounds per-prompt tool span state: calls still open
	// plus markers for calls already recorded. Calls past it are not observed.
	maxPromptGenAITools = harnessv2.MaxRuntimeSessionTombstoneOperations

	genAIErrorTypeTool       = "tool_error"
	genAIErrorTypeIncomplete = "incomplete"
	genAIErrorTypeCancelled  = "cancelled"
	genAIErrorTypeAbandoned  = "abandoned"
	genAIToolNameFallback    = "other"

	acpToolKindRead    = "read"
	acpToolKindExecute = "execute"
)

// genAIToolNamePattern admits adapter tool identities that are safe,
// low-cardinality span names and metric labels.
var genAIToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)

// acpToolKinds is the closed ACP ToolKind enum.
var acpToolKinds = map[string]bool{
	acpToolKindRead: true, "edit": true, "delete": true, "move": true, "search": true,
	acpToolKindExecute: true, "think": true, "fetch": true, "switch_mode": true, genAIToolNameFallback: true,
}

// supervisorGenAI holds the GenAI instruments for one supervisor. Nil
// disables all GenAI telemetry.
type supervisorGenAI struct {
	tracer              trace.Tracer
	operationDuration   metric.Float64Histogram
	tokenUsage          metric.Int64Histogram
	toolDuration        metric.Float64Histogram
	invokeAgentDuration metric.Float64Histogram
	agentName           string
	providerName        string
	requestModel        string
}

func newSupervisorGenAI(tracer trace.Tracer, meter metric.Meter, providerKind, model string) *supervisorGenAI {
	if tracer == nil && meter == nil {
		return nil
	}
	provider, requestModel := genai.AgentRuntimeProvider(providerKind, model)
	g := &supervisorGenAI{tracer: tracer, agentName: providerKind, providerName: provider, requestModel: requestModel}
	if meter != nil {
		// Instrument creation failures leave that instrument disabled.
		g.operationDuration, _ = meter.Float64Histogram(genai.MetricClientOperationDuration,
			metric.WithUnit(genai.UnitSeconds), metric.WithExplicitBucketBoundaries(genai.OperationDurationBuckets...))
		g.tokenUsage, _ = meter.Int64Histogram(genai.MetricClientTokenUsage,
			metric.WithUnit(genai.UnitTokens), metric.WithExplicitBucketBoundaries(genai.TokenUsageBuckets...))
		g.toolDuration, _ = meter.Float64Histogram(genai.MetricExecuteToolDuration,
			metric.WithUnit(genai.UnitSeconds), metric.WithExplicitBucketBoundaries(genai.ToolDurationBuckets...))
		g.invokeAgentDuration, _ = meter.Float64Histogram(genai.MetricInvokeAgentDuration,
			metric.WithUnit(genai.UnitSeconds), metric.WithExplicitBucketBoundaries(genai.InvokeAgentDurationBuckets...))
	}
	return g
}

func (g *supervisorGenAI) modelAttributes() []attribute.KeyValue {
	var attributes []attribute.KeyValue
	if g.providerName != "" {
		attributes = append(attributes, attribute.String(genai.AttrProviderName, g.providerName))
	}
	if g.requestModel != "" {
		attributes = append(attributes, attribute.String(genai.AttrRequestModel, g.requestModel))
	}
	return attributes
}

// promptGenAI records one prompt's invoke_agent span and the model and tool
// spans beneath it. All methods are safe on a nil receiver.
type promptGenAI struct {
	g       *supervisorGenAI
	start   time.Time
	session string

	mu     sync.Mutex
	span   trace.Span // nil when only metrics are enabled
	ended  bool
	usage  *acp.PromptUsage
	tools  map[string]*promptGenAITool
	parent context.Context
}

type promptGenAITool struct {
	name  string
	kind  string
	start time.Time
	// recorded marks a call whose span is written. The marker stays so that a
	// repeated or late update for the same ID does not record the call again.
	recorded bool
}

// startInvokeAgent opens the prompt's invoke_agent span under the
// authenticated acp.supervisor.prompt operation span in ctx.
func (s *Server) startInvokeAgent(ctx context.Context, state *sessionState, request harnessv2.StartPromptRequest, now time.Time) *promptGenAI {
	g := s.genai
	if g == nil {
		return nil
	}
	p := &promptGenAI{g: g, start: now, session: string(state.id), parent: context.Background()}
	if g.tracer != nil {
		attributes := append([]attribute.KeyValue{
			attribute.String(genai.AttrOperationName, genai.OperationInvokeAgent),
			attribute.String(genai.AttrAgentName, g.agentName),
			attribute.String(genai.AttrConversationID, p.session),
			attribute.String("orka.task.uid", string(request.Metadata.TaskUID)),
			attribute.Int64("orka.acp.task.attempt", int64(request.Metadata.TaskAttempt)),
			attribute.String("orka.acp.prompt.id", string(request.Metadata.PromptID)),
		}, g.modelAttributes()...)
		var span trace.Span
		p.parent, span = g.tracer.Start(trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx)),
			genai.OperationInvokeAgent+" "+g.agentName,
			trace.WithSpanKind(trace.SpanKindClient), trace.WithTimestamp(now), trace.WithAttributes(attributes...))
		p.span = span
	}
	return p
}

// setUsage keeps the agent-reported turn usage for the invoke_agent span.
func (p *promptGenAI) setUsage(usage *acp.PromptUsage) {
	if p == nil || usage == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usage = usage
}

// observeTool tracks one ACP tool call. The first update starts the call;
// a completed or failed status records its execute_tool span.
func (p *promptGenAI) observeTool(update *harnessv2.ToolCallUpdate, name string, at time.Time) {
	if p == nil || update == nil || update.ToolCallID == "" {
		return
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return
	}
	tool := p.tools[update.ToolCallID]
	if tool != nil && tool.recorded {
		return
	}
	if tool == nil {
		if len(p.tools) >= maxPromptGenAITools {
			return
		}
		if p.tools == nil {
			p.tools = make(map[string]*promptGenAITool)
		}
		tool = &promptGenAITool{start: at}
		p.tools[update.ToolCallID] = tool
	}
	if name != "" {
		tool.name = name
	}
	if update.Kind != "" {
		tool.kind = update.Kind
	}
	switch update.Status {
	case harnessv2.ToolCallStatusCompleted:
		p.recordToolLocked(update.ToolCallID, tool, at, "")
	case harnessv2.ToolCallStatusFailed:
		p.recordToolLocked(update.ToolCallID, tool, at, genAIErrorTypeTool)
	}
}

func (p *promptGenAI) recordToolLocked(id string, tool *promptGenAITool, end time.Time, errorType string) {
	tool.recorded = true
	name := genAIToolName(tool.name, tool.kind)
	toolType := genai.ToolTypeFunction
	if end.Before(tool.start) {
		end = tool.start
	}
	if p.g.tracer != nil {
		attributes := []attribute.KeyValue{
			attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
			attribute.String(genai.AttrToolName, name),
			attribute.String(genai.AttrToolType, toolType),
			attribute.String(genai.AttrToolCallID, boundedGenAIIdentifier(id)),
			attribute.String(genai.AttrConversationID, p.session),
			attribute.String("orka.tool.name", name),
		}
		if acpToolKinds[tool.kind] {
			attributes = append(attributes, attribute.String("orka.acp.tool.kind", tool.kind))
		}
		if errorType != "" {
			attributes = append(attributes, attribute.String(genai.AttrErrorType, errorType))
		}
		_, span := p.g.tracer.Start(p.parent, genai.OperationExecuteTool+" "+name,
			trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(tool.start), trace.WithAttributes(attributes...))
		if errorType != "" {
			span.SetStatus(codes.Error, errorType)
		}
		span.End(trace.WithTimestamp(end))
	}
	if p.g.toolDuration != nil {
		attributes := []attribute.KeyValue{
			attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
			attribute.String(genai.AttrToolName, name),
			attribute.String(genai.AttrToolType, toolType),
		}
		if errorType != "" {
			attributes = append(attributes, attribute.String(genai.AttrErrorType, errorType))
		}
		p.g.toolDuration.Record(context.Background(), end.Sub(tool.start).Seconds(), metric.WithAttributes(attributes...))
	}
}

// genAIToolName returns a content-free, low-cardinality tool name: the
// adapter's structured identity when it is a plain identifier, else the ACP
// tool kind. Titles are free text and are never used.
func genAIToolName(name, kind string) string {
	if genAIToolNamePattern.MatchString(name) {
		return name
	}
	if acpToolKinds[kind] {
		return kind
	}
	return genAIToolNameFallback
}

func boundedGenAIIdentifier(id string) string {
	if genAIToolNamePattern.MatchString(id) {
		return id
	}
	return ""
}

// finish records the invoke_agent span and metrics once for the prompt's
// settled result, and closes tool calls that never reported an outcome.
func (p *promptGenAI) finish(result acp.PromptResult, settledAt time.Time) {
	if p == nil {
		return
	}
	errorType := ""
	finishReason := string(result.StopReason)
	switch result.Outcome {
	case acp.PromptOutcomeCompleted:
	case acp.PromptOutcomeCancelled:
		finishReason = string(acp.StopReasonCancelled)
	case acp.PromptOutcomeFailed:
		errorType = string(acp.PromptOutcomeFailed)
		if result.StopReason != "" && result.StopReason != acp.StopReasonEndTurn && result.StopReason.Validate() == nil {
			errorType = string(result.StopReason)
		}
	case acp.PromptOutcomeOutcomeUnknown:
		errorType = string(acp.PromptOutcomeOutcomeUnknown)
	default:
		errorType = genai.ErrorTypeOther
	}
	if result.StopReason.Validate() != nil {
		finishReason = ""
	}
	p.end(settledAt, errorType, finishReason, result.Outcome == acp.PromptOutcomeCancelled)
}

// abandon ends a prompt that never reached settlement, such as one rejected
// before the agent accepted it.
func (p *promptGenAI) abandon() {
	if p == nil {
		return
	}
	p.end(time.Now().UTC(), genAIErrorTypeAbandoned, "", false)
}

func (p *promptGenAI) end(at time.Time, errorType, finishReason string, cancelled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return
	}
	if at.IsZero() || at.Before(p.start) {
		at = time.Now().UTC()
	}
	open := genAIErrorTypeIncomplete
	if cancelled {
		open = genAIErrorTypeCancelled
	}
	for id, tool := range p.tools {
		if !tool.recorded {
			p.recordToolLocked(id, tool, at, open)
		}
	}
	p.tools = nil
	p.ended = true
	modelAttributes := p.g.modelAttributes()
	if p.span != nil {
		var attributes []attribute.KeyValue
		if finishReason != "" {
			attributes = append(attributes, attribute.StringSlice(genai.AttrResponseFinishReasons, []string{finishReason}))
		}
		if usage := p.usage; usage != nil {
			for _, count := range []struct {
				key   string
				value *int64
			}{
				{genai.AttrUsageInputTokens, usage.InputTokens},
				{genai.AttrUsageOutputTokens, usage.OutputTokens},
				{genai.AttrUsageCacheReadInputTokens, usage.CachedReadTokens},
				{genai.AttrUsageCacheCreateInputTokens, usage.CachedWriteTokens},
			} {
				if count.value != nil {
					attributes = append(attributes, attribute.Int64(count.key, *count.value))
				}
			}
		}
		if errorType != "" {
			attributes = append(attributes, attribute.String(genai.AttrErrorType, errorType))
			p.span.SetStatus(codes.Error, errorType)
		}
		p.span.SetAttributes(attributes...)
		p.span.End(trace.WithTimestamp(at))
	}
	base := append([]attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationInvokeAgent),
		attribute.String(genai.AttrAgentName, p.g.agentName),
	}, modelAttributes...)
	if p.g.invokeAgentDuration != nil {
		attributes := base
		if errorType != "" {
			attributes = append(append([]attribute.KeyValue(nil), base...), attribute.String(genai.AttrErrorType, errorType))
		}
		p.g.invokeAgentDuration.Record(context.Background(), at.Sub(p.start).Seconds(), metric.WithAttributes(attributes...))
	}
	if p.g.tokenUsage != nil && p.usage != nil {
		for _, count := range []struct {
			tokenType string
			value     *int64
		}{
			{genai.TokenTypeInput, p.usage.InputTokens},
			{genai.TokenTypeOutput, p.usage.OutputTokens},
		} {
			if count.value == nil {
				continue
			}
			attributes := append(append([]attribute.KeyValue(nil), base...), attribute.String(genai.AttrTokenType, count.tokenType))
			p.g.tokenUsage.Record(context.Background(), *count.value, metric.WithAttributes(attributes...))
		}
	}
}

// promptGenAIChat is one model call relayed by the provider proxy.
type promptGenAIChat struct {
	prompt *promptGenAI
	start  time.Time
	// status is the accounted inference outcome; 0 when none was accounted.
	status int
}

// startChat begins a chat observation for one admitted inference request.
func (p *promptGenAI) startChat(now time.Time) *promptGenAIChat {
	if p == nil {
		return nil
	}
	return &promptGenAIChat{prompt: p, start: now}
}

// end records the chat span and operation duration. cancelled reports that
// the child or the prompt gate abandoned the request before an outcome was
// accounted.
func (c *promptGenAIChat) end(at time.Time, status int, cancelled bool) {
	if c == nil {
		return
	}
	p := c.prompt
	errorType := ""
	switch {
	case status >= 400:
		errorType = strconv.Itoa(status)
	case status > 0:
	case cancelled:
		errorType = genAIErrorTypeCancelled
	default:
		errorType = genai.ErrorTypeOther
	}
	if at.Before(c.start) {
		at = c.start
	}
	attributes := append([]attribute.KeyValue{attribute.String(genai.AttrOperationName, genai.OperationChat)}, p.g.modelAttributes()...)
	if p.g.tracer != nil {
		spanAttributes := append(append([]attribute.KeyValue(nil), attributes...), attribute.String(genai.AttrConversationID, p.session))
		if errorType != "" {
			spanAttributes = append(spanAttributes, attribute.String(genai.AttrErrorType, errorType))
		}
		_, span := p.g.tracer.Start(p.parent, genai.OperationChat+" "+p.g.requestModel,
			trace.WithSpanKind(trace.SpanKindClient), trace.WithTimestamp(c.start), trace.WithAttributes(spanAttributes...))
		if errorType != "" {
			span.SetStatus(codes.Error, errorType)
		}
		span.End(trace.WithTimestamp(at))
	}
	if p.g.operationDuration != nil {
		if errorType != "" {
			attributes = append(attributes, attribute.String(genai.AttrErrorType, errorType))
		}
		p.g.operationDuration.Record(context.Background(), at.Sub(c.start).Seconds(), metric.WithAttributes(attributes...))
	}
}
