package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tracing/genai"
)

type genAITestHarness struct {
	recorder *tracetest.SpanRecorder
	reader   *sdkmetric.ManualReader
	server   *Server
}

func newGenAITestHarness(t *testing.T, providerKind, model string) *genAITestHarness {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reader := sdkmetric.NewManualReader()
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = traces.Shutdown(context.Background())
		_ = metrics.Shutdown(context.Background())
	})
	server := &Server{genai: newSupervisorGenAI(
		traces.Tracer(genai.InstrumentationName, trace.WithSchemaURL(genai.SchemaURL)),
		metrics.Meter(genai.InstrumentationName), providerKind, model,
	)}
	return &genAITestHarness{recorder: recorder, reader: reader, server: server}
}

func (h *genAITestHarness) spans(name string) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, span := range h.recorder.Ended() {
		if span.Name() == name {
			spans = append(spans, span)
		}
	}
	return spans
}

func (h *genAITestHarness) metrics(t *testing.T) map[string]metricdata.Aggregation {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	result := map[string]metricdata.Aggregation{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			result[m.Name] = m.Data
		}
	}
	return result
}

func genAISpanAttributes(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	attrs := map[string]attribute.Value{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	return attrs
}

func testGenAIPromptRequest() harnessv2.StartPromptRequest {
	var request harnessv2.StartPromptRequest
	request.Metadata.TaskUID = "task-uid"
	request.Metadata.TaskAttempt = 2
	request.Metadata.PromptID = "prompt-1"
	request.Input.Content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "PRIVATE_PROMPT"}}
	return request
}

func TestPromptGenAISpansAndMetricsFromACPStream(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "true")
	h := newGenAITestHarness(t, providerKindClaude, "claude-sonnet-4")
	operation := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{7}, SpanID: trace.SpanID{7}, TraceFlags: trace.FlagsSampled})
	start := time.Now().UTC()
	prompt := h.server.startInvokeAgent(trace.ContextWithSpanContext(context.Background(), operation),
		&sessionState{id: "session-1"}, testGenAIPromptRequest(), start)

	// A provider session relays two model calls: one succeeds and one fails.
	session := &providerProxySession{turnPromptID: "prompt-1"}
	session.setPromptTelemetry("prompt-1", prompt)
	if chat := session.startChat("other-prompt", 1, start); chat != nil {
		t.Fatal("a different prompt's request was attributed to this prompt")
	}
	succeeded := session.startChat("prompt-1", 1, start)
	failed := session.startChat("prompt-1", 2, start)
	session.recordInferenceOutcome("prompt-1", providerRequestInference, 1, http.StatusOK, "")
	session.recordInferenceOutcome("prompt-1", providerRequestInference, 2, http.StatusTooManyRequests, "PRIVATE_DETAIL")
	session.finishChat(1, succeeded, false)
	session.finishChat(2, failed, false)
	abandoned := session.startChat("prompt-1", 3, start)
	session.finishChat(3, abandoned, true)

	// Tool calls: a named completed call, a failed call whose name is free
	// text, and a call that never reports an outcome.
	prompt.observeTool(&harnessv2.ToolCallUpdate{ToolCallID: "call-1", Kind: "execute", Title: "Run PRIVATE_TITLE", Status: harnessv2.ToolCallStatusPending}, "Bash", start)
	prompt.observeTool(&harnessv2.ToolCallUpdate{ToolCallID: "call-1", Status: harnessv2.ToolCallStatusCompleted,
		Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "PRIVATE_RESULT"}}}, "Bash", start.Add(time.Second))
	prompt.observeTool(&harnessv2.ToolCallUpdate{ToolCallID: "call-2", Kind: "edit", Status: harnessv2.ToolCallStatusFailed}, "Edit PRIVATE_FILE now", start.Add(time.Second))
	prompt.observeTool(&harnessv2.ToolCallUpdate{ToolCallID: "call-3", Kind: "not-a-kind", Status: harnessv2.ToolCallStatusInProgress}, "", start)

	prompt.setUsage(acp.ParsePromptUsage(json.RawMessage(`{"inputTokens":120,"outputTokens":30,"totalTokens":150,"cachedReadTokens":10}`)))
	prompt.finish(acp.PromptResult{Outcome: acp.PromptOutcomeCompleted, StopReason: acp.StopReasonEndTurn}, start.Add(2*time.Second))
	prompt.finish(acp.PromptResult{Outcome: acp.PromptOutcomeFailed}, start.Add(3*time.Second))
	prompt.abandon()

	invoke := assertInvokeAgentSpan(t, h, operation)
	assertChatSpans(t, h, invoke)
	assertToolSpans(t, h, invoke)
	// Content capture stays off whatever the environment says.
	assertNoGenAIContent(t, h.recorder.Ended())
	assertACPGenAIMetrics(t, h)
}

func assertInvokeAgentSpan(t *testing.T, h *genAITestHarness, operation trace.SpanContext) sdktrace.ReadOnlySpan {
	t.Helper()
	invocations := h.spans("invoke_agent claude")
	if len(invocations) != 1 {
		t.Fatalf("invoke_agent spans = %d, want exactly one", len(invocations))
	}
	invoke := invocations[0]
	if invoke.Parent().SpanID() != operation.SpanID() || invoke.SpanKind() != trace.SpanKindClient || invoke.Status().Code == codes.Error {
		t.Fatalf("invoke_agent parent/kind/status = %v/%v/%v", invoke.Parent().SpanID(), invoke.SpanKind(), invoke.Status())
	}
	attrs := genAISpanAttributes(invoke)
	for key, want := range map[string]string{
		genai.AttrOperationName: genai.OperationInvokeAgent, genai.AttrAgentName: providerKindClaude,
		genai.AttrProviderName: genai.ProviderAnthropic, genai.AttrRequestModel: "claude-sonnet-4",
		genai.AttrConversationID: "session-1", "orka.task.uid": "task-uid", "orka.acp.prompt.id": "prompt-1",
	} {
		if attrs[key].AsString() != want {
			t.Errorf("invoke_agent %s = %q, want %q", key, attrs[key].String(), want)
		}
	}
	if attrs[genai.AttrUsageInputTokens].AsInt64() != 120 || attrs[genai.AttrUsageOutputTokens].AsInt64() != 30 ||
		attrs[genai.AttrUsageCacheReadInputTokens].AsInt64() != 10 {
		t.Errorf("invoke_agent usage = %v", attrs)
	}
	if _, ok := attrs[genai.AttrUsageCacheCreateInputTokens]; ok {
		t.Error("unreported cache creation tokens were recorded")
	}
	if got := attrs[genai.AttrResponseFinishReasons].AsStringSlice(); !slices.Equal(got, []string{"end_turn"}) {
		t.Errorf("finish reasons = %v", got)
	}
	return invoke
}

func assertChatSpans(t *testing.T, h *genAITestHarness, invoke sdktrace.ReadOnlySpan) {
	t.Helper()
	chats := h.spans("chat claude-sonnet-4")
	if len(chats) != 3 {
		t.Fatalf("chat spans = %d, want 3", len(chats))
	}
	errorTypes := map[string]int{}
	for _, chat := range chats {
		if chat.Parent().SpanID() != invoke.SpanContext().SpanID() || chat.SpanKind() != trace.SpanKindClient {
			t.Fatalf("chat parent/kind = %v/%v", chat.Parent().SpanID(), chat.SpanKind())
		}
		chatAttrs := genAISpanAttributes(chat)
		if chatAttrs[genai.AttrOperationName].AsString() != genai.OperationChat || chatAttrs[genai.AttrProviderName].AsString() != genai.ProviderAnthropic {
			t.Fatalf("chat attributes = %v", chatAttrs)
		}
		errorTypes[chatAttrs[genai.AttrErrorType].AsString()]++
	}
	if errorTypes[""] != 1 || errorTypes["429"] != 1 || errorTypes[genAIErrorTypeCancelled] != 1 {
		t.Fatalf("chat error types = %v", errorTypes)
	}
}

func assertToolSpans(t *testing.T, h *genAITestHarness, invoke sdktrace.ReadOnlySpan) {
	t.Helper()
	tools := map[string]sdktrace.ReadOnlySpan{}
	for _, span := range h.recorder.Ended() {
		if strings.HasPrefix(span.Name(), "execute_tool ") {
			tools[span.Name()] = span
			if span.Parent().SpanID() != invoke.SpanContext().SpanID() {
				t.Fatalf("%s is not a child of invoke_agent", span.Name())
			}
		}
	}
	for name, want := range map[string]string{"execute_tool Bash": "", "execute_tool edit": genAIErrorTypeTool, "execute_tool other": genAIErrorTypeIncomplete} {
		span := tools[name]
		if span == nil {
			t.Fatalf("missing %s; got %v", name, tools)
		}
		toolAttrs := genAISpanAttributes(span)
		if toolAttrs[genai.AttrErrorType].AsString() != want || toolAttrs[genai.AttrOperationName].AsString() != genai.OperationExecuteTool {
			t.Errorf("%s attributes = %v", name, toolAttrs)
		}
	}
	if got := tools["execute_tool Bash"].EndTime().Sub(tools["execute_tool Bash"].StartTime()); got != time.Second {
		t.Errorf("tool duration = %v, want 1s", got)
	}
}

func assertNoGenAIContent(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, span := range spans {
		for _, kv := range span.Attributes() {
			if strings.Contains(kv.Value.String(), "PRIVATE_") {
				t.Fatalf("%s exported content %s=%s", span.Name(), kv.Key, kv.Value.String())
			}
			switch string(kv.Key) {
			case genai.AttrInputMessages, genai.AttrOutputMessages, genai.AttrSystemInstructions, genai.AttrToolCallArguments, genai.AttrToolCallResult:
				t.Fatalf("%s exported content attribute %s", span.Name(), kv.Key)
			}
		}
		if len(span.Events()) != 0 || strings.Contains(span.Status().Description, "PRIVATE_") {
			t.Fatalf("%s exported events or status content", span.Name())
		}
	}
}

func assertACPGenAIMetrics(t *testing.T, h *genAITestHarness) {
	t.Helper()
	metrics := h.metrics(t)
	for _, name := range []string{genai.MetricClientOperationDuration, genai.MetricExecuteToolDuration, genai.MetricInvokeAgentDuration} {
		histogram, ok := metrics[name].(metricdata.Histogram[float64])
		if !ok || len(histogram.DataPoints) == 0 {
			t.Fatalf("missing %s", name)
		}
	}
	tokens, ok := metrics[genai.MetricClientTokenUsage].(metricdata.Histogram[int64])
	if !ok || len(tokens.DataPoints) != 2 {
		t.Fatalf("token usage = %#v", metrics[genai.MetricClientTokenUsage])
	}
	for _, point := range tokens.DataPoints {
		tokenType, _ := point.Attributes.Value(attribute.Key(genai.AttrTokenType))
		operation, _ := point.Attributes.Value(attribute.Key(genai.AttrOperationName))
		if operation.AsString() != genai.OperationInvokeAgent || (tokenType.AsString() == genai.TokenTypeInput) != (point.Sum == 120) {
			t.Fatalf("token datapoint = %v sum=%d", point.Attributes, point.Sum)
		}
		if _, ok := point.Attributes.Value("orka.task.uid"); ok {
			t.Fatal("metric carries a high-cardinality Task identity")
		}
	}
}

func TestPromptGenAIOutcomesAndOpenTools(t *testing.T) {
	for _, test := range []struct {
		name          string
		result        acp.PromptResult
		wantError     string
		wantFinish    []string
		wantOpenError string
	}{
		{"cancelled", acp.PromptResult{Outcome: acp.PromptOutcomeCancelled, StopReason: acp.StopReasonCancelled}, "", []string{"cancelled"}, genAIErrorTypeCancelled},
		{"max tokens", acp.PromptResult{Outcome: acp.PromptOutcomeFailed, StopReason: acp.StopReasonMaxTokens}, "max_tokens", []string{"max_tokens"}, genAIErrorTypeIncomplete},
		{"transport failure", acp.PromptResult{Outcome: acp.PromptOutcomeFailed}, "failed", nil, genAIErrorTypeIncomplete},
		{"unknown", acp.PromptResult{Outcome: acp.PromptOutcomeOutcomeUnknown}, "outcome_unknown", nil, genAIErrorTypeIncomplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newGenAITestHarness(t, providerKindCodex, "gpt-5")
			prompt := h.server.startInvokeAgent(context.Background(), &sessionState{id: "session-1"}, testGenAIPromptRequest(), time.Now())
			prompt.observeTool(&harnessv2.ToolCallUpdate{ToolCallID: "call-1", Kind: "read", Status: harnessv2.ToolCallStatusPending}, "", time.Now())
			prompt.finish(test.result, time.Now())
			invoke := h.spans("invoke_agent codex")
			if len(invoke) != 1 {
				t.Fatalf("invoke spans = %d", len(invoke))
			}
			attrs := genAISpanAttributes(invoke[0])
			if attrs[genai.AttrErrorType].AsString() != test.wantError || (test.wantError != "") != (invoke[0].Status().Code == codes.Error) {
				t.Fatalf("error.type = %q status = %v", attrs[genai.AttrErrorType].AsString(), invoke[0].Status())
			}
			if attrs[genai.AttrProviderName].AsString() != genai.ProviderOpenAI {
				t.Fatalf("provider = %q", attrs[genai.AttrProviderName].AsString())
			}
			if got := attrs[genai.AttrResponseFinishReasons].AsStringSlice(); !slices.Equal(got, test.wantFinish) {
				t.Fatalf("finish reasons = %v, want %v", got, test.wantFinish)
			}
			tool := h.spans("execute_tool read")
			if len(tool) != 1 || genAISpanAttributes(tool[0])[genai.AttrErrorType].AsString() != test.wantOpenError {
				t.Fatalf("open tool spans = %v", tool)
			}
		})
	}
}

func TestPromptGenAIDisabledAndAbandoned(t *testing.T) {
	disabled := &Server{genai: newSupervisorGenAI(nil, nil, providerKindCodex, "gpt-5")}
	prompt := disabled.startInvokeAgent(context.Background(), &sessionState{id: "session-1"}, testGenAIPromptRequest(), time.Now())
	if prompt != nil {
		t.Fatal("disabled telemetry created a prompt recorder")
	}
	// Every recorder method is a no-op on nil.
	prompt.observeTool(&harnessv2.ToolCallUpdate{ToolCallID: "call-1", Status: harnessv2.ToolCallStatusCompleted}, "Bash", time.Now())
	prompt.setUsage(&acp.PromptUsage{})
	prompt.finish(acp.PromptResult{Outcome: acp.PromptOutcomeCompleted}, time.Now())
	prompt.abandon()
	session := &providerProxySession{turnPromptID: "prompt-1"}
	session.setPromptTelemetry("prompt-1", prompt)
	if chat := session.startChat("prompt-1", 1, time.Now()); chat != nil {
		t.Fatal("disabled telemetry started a chat observation")
	}

	h := newGenAITestHarness(t, providerKindOpencode, "anthropic/claude-sonnet-4")
	prompt = h.server.startInvokeAgent(context.Background(), &sessionState{id: "session-1"}, testGenAIPromptRequest(), time.Now())
	prompt.abandon()
	prompt.finish(acp.PromptResult{Outcome: acp.PromptOutcomeCompleted}, time.Now())
	invoke := h.spans("invoke_agent opencode")
	if len(invoke) != 1 {
		t.Fatalf("invoke spans = %d", len(invoke))
	}
	attrs := genAISpanAttributes(invoke[0])
	if attrs[genai.AttrErrorType].AsString() != genAIErrorTypeAbandoned || attrs[genai.AttrProviderName].AsString() != genai.ProviderAnthropic ||
		attrs[genai.AttrRequestModel].AsString() != "claude-sonnet-4" {
		t.Fatalf("abandoned opencode invoke attributes = %v", attrs)
	}
}

func TestGenAIToolNameIsContentFree(t *testing.T) {
	for _, test := range []struct{ name, kind, want string }{
		{"Bash", "execute", "Bash"},
		{"mcp__orka__web_fetch", "fetch", "mcp__orka__web_fetch"},
		{"Run rm -rf PRIVATE", "execute", "execute"},
		{strings.Repeat("a", 129), "read", "read"},
		{"", "unknown-kind", genAIToolNameFallback},
	} {
		if got := genAIToolName(test.name, test.kind); got != test.want {
			t.Errorf("genAIToolName(%q, %q) = %q, want %q", test.name, test.kind, got, test.want)
		}
	}
}

func TestSupervisorPromptEmitsGenAISpansWithoutContent(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "true")
	server, cfg, profile := newTestServer(t, genAITelemetryMode)
	recorder := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reader := sdkmetric.NewManualReader()
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() {
		_ = traces.Shutdown(context.Background())
		_ = metrics.Shutdown(context.Background())
	}()
	server.cfg.Tracer = traces.Tracer("orka.acp.supervisor")
	server.genai = newSupervisorGenAI(traces.Tracer(genai.InstrumentationName), metrics.Meter(genai.InstrumentationName), cfg.Provider.Kind, cfg.Provider.Model)
	runGenAITelemetryPrompt(t, server, cfg, profile)

	assertNoGenAIContent(t, recorder.Ended())
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Ended() {
		byName[span.Name()] = span
	}
	operation, invoke := byName["acp.supervisor.prompt"], byName["invoke_agent codex"]
	chat, tool := byName["chat test-model"], byName["execute_tool execute"]
	if operation == nil || invoke == nil || chat == nil || tool == nil {
		t.Fatalf("missing GenAI spans; got %v", slices.Collect(maps.Keys(byName)))
	}
	if invoke.Parent().SpanID() != operation.SpanContext().SpanID() || chat.Parent().SpanID() != invoke.SpanContext().SpanID() ||
		tool.Parent().SpanID() != invoke.SpanContext().SpanID() {
		t.Fatal("GenAI spans are not nested under the authenticated prompt operation")
	}
	invokeAttrs, chatAttrs := genAISpanAttributes(invoke), genAISpanAttributes(chat)
	if invokeAttrs[genai.AttrUsageInputTokens].AsInt64() != 120 || invokeAttrs[genai.AttrUsageOutputTokens].AsInt64() != 30 ||
		invokeAttrs[genai.AttrErrorType].AsString() != "" || invokeAttrs[genai.AttrProviderName].AsString() != genai.ProviderOpenAI {
		t.Fatalf("invoke_agent attributes = %v", invokeAttrs)
	}
	if chatAttrs[genai.AttrErrorType].AsString() != "" || chatAttrs[genai.AttrRequestModel].AsString() != "test-model" {
		t.Fatalf("chat attributes = %v", chatAttrs)
	}
	// The ACP call ID is the canonical digest harness v2 events also carry.
	if !strings.HasPrefix(genAISpanAttributes(tool)[genai.AttrToolCallID].AsString(), "tool-call-v1-sha256-") {
		t.Fatalf("tool attributes = %v", genAISpanAttributes(tool))
	}
	names := collectedGenAIMetricNames(t, reader)
	for _, name := range []string{genai.MetricClientOperationDuration, genai.MetricClientTokenUsage, genai.MetricExecuteToolDuration, genai.MetricInvokeAgentDuration} {
		if !names[name] {
			t.Fatalf("missing metric %s; got %v", name, names)
		}
	}
}

// runGenAITelemetryPrompt creates a runtime session and runs one prompt of
// the genAITelemetryMode helper to completion.
func runGenAITelemetryPrompt(t *testing.T, server *Server, cfg Config, profile harnessv2.RuntimeProfile) {
	t.Helper()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := harnessv2.NewClient(httpServer.URL, harnessv2.WithControllerBearerToken(cfg.ControllerBearerToken), harnessv2.WithOperationCapabilitySecret(cfg.CapabilitySecret))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	create := testCreateSessionRequest(t, cfg, profile)
	if _, err := client.CreateRuntimeSession(ctx, create); err != nil {
		t.Fatal(err)
	}
	stream, err := client.StartPrompt(ctx, create.RuntimeSessionID, testStartPromptRequest(t, cfg, create.Metadata.Fence))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close() //nolint:errcheck
	// Read to EOF: the handler ends its spans before the response completes.
	completed := false
	for {
		event, err := stream.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == harnessv2.EventCompleted {
			completed = true
			if text := event.Completed.Result.Content[0].Text; !strings.Contains(text, "model=200") {
				t.Fatalf("provider proxy call did not succeed: %q", text)
			}
		}
	}
	if !completed {
		t.Fatal("prompt did not complete")
	}
}

func collectedGenAIMetricNames(t *testing.T, reader *sdkmetric.ManualReader) map[string]bool {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			names[m.Name] = true
		}
	}
	return names
}
