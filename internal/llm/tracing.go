/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/tracing/genai"
)

// TracingProvider wraps an LLM Provider with OpenTelemetry GenAI tracing.
type TracingProvider struct {
	inner Provider

	initMu  sync.Mutex
	tracer  atomic.Pointer[tracingTracer]
	metrics atomic.Pointer[tracingMetrics]
}

type tracingTracer struct {
	tracer               trace.Tracer
	deferStartAttributes bool
}

type tracingMetrics struct {
	operationDuration metric.Float64Histogram
	usage             metric.Int64Histogram
	timeToFirstChunk  metric.Float64Histogram
}

var noopTracingSpan = trace.SpanFromContext(context.Background())

// NewTracingProvider returns a Provider that records a GenAI span and metrics
// for every LLM call once an OpenTelemetry provider is configured. Legacy llm.*
// attributes are dual-emitted during migration.
func NewTracingProvider(p Provider) Provider {
	tp := &TracingProvider{inner: p}
	tp.ensureTelemetry()
	return tp
}

func (tp *TracingProvider) ensureTelemetry() bool {
	if tracer := tp.tracer.Load(); tracer == nil {
		if !tracing.GlobalTracerProviderExplicitNoop() {
			tp.initTracer()
		}
	} else if tracer.deferStartAttributes && !tracing.IsDefaultGlobalTracerProvider(tracing.GlobalTracerProvider()) {
		tp.initTracer()
	}
	if tp.metrics.Load() == nil && tracing.GlobalMeterProviderActive() {
		tp.initMetrics()
	}
	return tp.tracer.Load() != nil || tp.metrics.Load() != nil
}

func (tp *TracingProvider) initTracer() {
	if tracing.GlobalTracerProviderExplicitNoop() {
		return
	}
	provider := tracing.GlobalTracerProvider()
	deferStartAttributes := tracing.IsDefaultGlobalTracerProvider(provider)
	if current := tp.tracer.Load(); current != nil && (!current.deferStartAttributes || deferStartAttributes) {
		return
	}
	tp.initMu.Lock()
	defer tp.initMu.Unlock()
	if tracing.GlobalTracerProviderExplicitNoop() {
		return
	}
	provider = tracing.GlobalTracerProvider()
	deferStartAttributes = tracing.IsDefaultGlobalTracerProvider(provider)
	if current := tp.tracer.Load(); current != nil && (!current.deferStartAttributes || deferStartAttributes) {
		return
	}
	tp.tracer.Store(&tracingTracer{
		tracer:               provider.Tracer(genai.InstrumentationName, trace.WithSchemaURL(genai.SchemaURL)),
		deferStartAttributes: deferStartAttributes,
	})
}

func (tp *TracingProvider) initMetrics() {
	if tp.metrics.Load() != nil {
		return
	}
	tp.initMu.Lock()
	defer tp.initMu.Unlock()
	if tp.metrics.Load() != nil || !tracing.GlobalMeterProviderActive() {
		return
	}

	meter := tracing.GenAIMeter(genai.InstrumentationName)
	operationDuration, _ := meter.Float64Histogram(
		genai.MetricClientOperationDuration,
		metric.WithUnit(genai.UnitSeconds),
		metric.WithExplicitBucketBoundaries(genai.OperationDurationBuckets...),
	)
	usage, _ := meter.Int64Histogram(
		genai.MetricClientTokenUsage,
		metric.WithUnit(genai.UnitTokens),
		metric.WithExplicitBucketBoundaries(genai.TokenUsageBuckets...),
	)
	timeToFirstChunk, _ := meter.Float64Histogram(
		genai.MetricClientTimeToFirstChunk,
		metric.WithUnit(genai.UnitSeconds),
		metric.WithExplicitBucketBoundaries(genai.OperationDurationBuckets...),
	)
	tp.metrics.Store(&tracingMetrics{
		operationDuration: operationDuration,
		usage:             usage,
		timeToFirstChunk:  timeToFirstChunk,
	})
}

func (tp *TracingProvider) Name() string { return tp.inner.Name() }

func (tp *TracingProvider) TelemetryProviderName() string { return ProviderTelemetryName(tp.inner) }

func (tp *TracingProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	if !tp.ensureTelemetry() {
		return tp.inner.Complete(ctx, req)
	}

	var providerName string
	span := noopTracingSpan
	spanStarted := false
	deferStartAttributes := false
	if tracer := tp.tracer.Load(); tracer != nil {
		deferStartAttributes = tracer.deferStartAttributes
		if deferStartAttributes {
			ctx, span = tracer.tracer.Start(ctx, spanName(req), trace.WithSpanKind(trace.SpanKindClient))
		} else {
			providerName = ProviderTelemetryName(tp.inner)
			attrs := requestAttributes(req, providerName, false)
			ctx, span = tracer.tracer.Start(ctx, spanName(req), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
		}
		spanStarted = true
	}

	spanRecording := span.IsRecording()
	metricsActive := tp.metrics.Load() != nil
	if !spanRecording && !metricsActive {
		resp, err := tp.inner.Complete(ctx, req)
		if spanStarted {
			span.End()
		}
		return resp, err
	}
	if spanStarted {
		defer span.End()
	}
	if providerName == "" {
		providerName = ProviderTelemetryName(tp.inner)
	}
	if spanRecording && deferStartAttributes {
		span.SetAttributes(requestAttributes(req, providerName, false)...)
	}

	start := time.Now()
	resp, err := tp.inner.Complete(ctx, req)
	durationSeconds := time.Since(start).Seconds()
	if err != nil {
		if isStreamingRequiredErr(err) {
			if spanRecording {
				span.SetAttributes(attribute.Bool("orka.llm.streaming_fallback_required", true))
			}
			return nil, err
		}
		if IsContextTooLongErr(err) {
			if spanRecording {
				span.SetAttributes(attribute.Bool("orka.llm.context_truncation_retry_required", true))
			}
			return nil, err
		}
		if !spanRecording && !metricsActive {
			return nil, err
		}
		errType := errorType(err)
		if spanRecording {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			if errType != "" {
				span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
			}
		}
		tp.recordOperationDuration(ctx, durationSeconds, providerName, requestModel(req), errType)
		return nil, err
	}

	if !spanRecording && !metricsActive {
		return resp, nil
	}
	providerName = responseProviderName(resp, tp.inner)
	if spanRecording {
		setResponseAttributes(span, resp, providerName)
	}
	modelName := responseModel(resp, req)
	errType := stopReasonErrorType(resp.StopReason)
	if errType != "" && spanRecording {
		span.SetStatus(codes.Error, resp.StopReason)
		span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
	}
	tp.recordOperationDuration(ctx, durationSeconds, providerName, modelName, errType)
	tp.recordTokenUsage(ctx, resp, providerName, modelName)
	return resp, nil
}

//nolint:gocyclo // Streaming telemetry handles independent provider/model/token/error/cancel signals inline.
func (tp *TracingProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	if !tp.ensureTelemetry() {
		return tp.inner.Stream(ctx, req)
	}

	start := time.Now()
	providerName := ProviderTelemetryName(tp.inner)
	span := noopTracingSpan
	if tracer := tp.tracer.Load(); tracer != nil {
		attrs := requestAttributes(req, providerName, true)
		ctx, span = tracer.tracer.Start(ctx, spanName(req), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	}
	spanRecording := span.IsRecording()
	metricsActive := tp.metrics.Load() != nil

	innerCh, err := tp.inner.Stream(ctx, req)
	if err != nil {
		if !spanRecording && !metricsActive {
			return nil, err
		}
		errType := errorType(err)
		if spanRecording {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			if errType != "" {
				span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
			}
		}
		tp.recordOperationDuration(ctx, time.Since(start).Seconds(), providerName, requestModel(req), errType)
		span.End()
		return nil, err
	}
	if !spanRecording && !metricsActive {
		return innerCh, nil
	}

	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		var finishReasons []string
		var errType string
		selectedProviderName := providerName
		selectedModel := requestModel(req)
		streamResp := &CompletionResponse{}
		defer func() {
			if spanRecording && len(finishReasons) > 0 {
				span.SetAttributes(attribute.StringSlice(genai.AttrResponseFinishReasons, finishReasons))
			}
			if streamResp.InputTokens > 0 || streamResp.OutputTokens > 0 {
				streamResp.Model = selectedModel
				if spanRecording {
					setResponseAttributes(span, streamResp, selectedProviderName)
				}
				tp.recordTokenUsage(ctx, streamResp, selectedProviderName, selectedModel)
			}
			tp.recordOperationDuration(ctx, time.Since(start).Seconds(), selectedProviderName, selectedModel, errType)
			span.End()
		}()

		var firstChunkRecorded bool
		for chunk := range innerCh {
			if strings.TrimSpace(chunk.Provider) != "" {
				selectedProviderName = genai.NormalizeProviderName(chunk.Provider)
				span.SetAttributes(attribute.String(genai.AttrProviderName, selectedProviderName))
			}
			if strings.TrimSpace(chunk.Model) != "" {
				selectedModel = chunk.Model
				span.SetAttributes(attribute.String(genai.AttrResponseModel, selectedModel))
			}
			if chunk.InputTokens > 0 {
				streamResp.InputTokens = chunk.InputTokens
			}
			if chunk.OutputTokens > 0 {
				streamResp.OutputTokens = chunk.OutputTokens
			}
			if !firstChunkRecorded && (chunk.Content != "" || chunk.ToolCall != nil || chunk.Done || chunk.Error != nil) {
				firstChunkRecorded = true
				latency := time.Since(start).Seconds()
				span.SetAttributes(attribute.Float64(genai.AttrResponseTimeToFirstChunk, latency))
				tp.recordTimeToFirstChunk(ctx, latency, selectedProviderName, selectedModel)
			}
			if chunk.StopReason != "" {
				finishReasons = []string{chunk.StopReason}
				if stopErrType := stopReasonErrorType(chunk.StopReason); stopErrType != "" && chunk.Error == nil {
					errType = stopErrType
					if spanRecording {
						span.SetStatus(codes.Error, chunk.StopReason)
						span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
					}
				}
			}
			if chunk.Error != nil {
				errType = errorType(chunk.Error)
				if spanRecording {
					span.RecordError(chunk.Error)
					span.SetStatus(codes.Error, chunk.Error.Error())
					if errType != "" {
						span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
					}
				}
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				if ctx.Err() != nil {
					errType = errorType(ctx.Err())
					if spanRecording {
						span.RecordError(ctx.Err())
						span.SetStatus(codes.Error, ctx.Err().Error())
						span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
					}
				}
				return
			}
		}
	}()
	return out, nil
}

func isStreamingRequiredErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "streaming is required")
}

func stopReasonErrorType(reason string) string {
	trimmed := strings.TrimSpace(reason)
	switch strings.ToLower(trimmed) {
	case "failed", "incomplete", "cancelled", "canceled", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return trimmed
	default:
		return ""
	}
}

func spanName(req *CompletionRequest) string {
	model := requestModel(req)
	if model == "" {
		return genai.OperationChat
	}
	return genai.OperationChat + " " + model
}

func requestModel(req *CompletionRequest) string {
	if req == nil {
		return ""
	}
	return req.Model
}

func responseModel(resp *CompletionResponse, req *CompletionRequest) string {
	if resp != nil && strings.TrimSpace(resp.Model) != "" {
		return resp.Model
	}
	return requestModel(req)
}

func requestAttributes(req *CompletionRequest, providerName string, stream bool) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationChat),
		attribute.String(genai.AttrProviderName, providerName),
		attribute.String(genai.AttrRequestModel, requestModel(req)),
		attribute.Bool(genai.AttrRequestStream, stream),
		// Legacy attrs during migration.
		attribute.String("llm.provider", providerName),
		attribute.String("llm.model", requestModel(req)),
	}
	if req == nil {
		return attrs
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, attribute.Int(genai.AttrRequestMaxTokens, req.MaxTokens))
	}
	if req.Temperature != 0 {
		attrs = append(attrs, attribute.Float64(genai.AttrRequestTemperature, req.Temperature))
	}
	if len(req.StopSequences) > 0 {
		attrs = append(attrs, attribute.Int(genai.AttrRequestStopSequences+".count", len(req.StopSequences)))
	}
	if req.ResponseFormat != nil && strings.TrimSpace(req.ResponseFormat.Type) != "" {
		attrs = append(attrs, attribute.String(genai.AttrOutputType, req.ResponseFormat.Type))
	}
	return attrs
}

func setResponseAttributes(span trace.Span, resp *CompletionResponse, providerName string) {
	if resp == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrProviderName, providerName),
		attribute.Int("llm.tool_calls", len(resp.ToolCalls)),
	}
	if resp.InputTokens > 0 {
		attrs = append(attrs,
			attribute.Int(genai.AttrUsageInputTokens, resp.InputTokens),
			attribute.Int("llm.input_tokens", resp.InputTokens),
		)
	}
	if resp.OutputTokens > 0 {
		attrs = append(attrs,
			attribute.Int(genai.AttrUsageOutputTokens, resp.OutputTokens),
			attribute.Int("llm.output_tokens", resp.OutputTokens),
		)
	}
	if resp.Model != "" {
		attrs = append(attrs, attribute.String(genai.AttrResponseModel, resp.Model))
	}
	if resp.ID != "" {
		attrs = append(attrs, attribute.String(genai.AttrResponseID, resp.ID))
	}
	if resp.StopReason != "" {
		attrs = append(attrs, attribute.StringSlice(genai.AttrResponseFinishReasons, []string{resp.StopReason}))
	}
	span.SetAttributes(attrs...)
}

func metricAttributes(providerName, model, errType string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationChat),
		attribute.String(genai.AttrProviderName, providerName),
		attribute.String(genai.AttrRequestModel, model),
	}
	if errType != "" {
		attrs = append(attrs, attribute.String(genai.AttrErrorType, errType))
	}
	return attrs
}

func (tp *TracingProvider) recordOperationDuration(ctx context.Context, seconds float64, providerName, model, errType string) {
	metrics := tp.metrics.Load()
	if metrics == nil || metrics.operationDuration == nil {
		return
	}
	metrics.operationDuration.Record(ctx, seconds, metric.WithAttributes(metricAttributes(providerName, model, errType)...))
}

func (tp *TracingProvider) recordTimeToFirstChunk(ctx context.Context, seconds float64, providerName, model string) {
	metrics := tp.metrics.Load()
	if metrics == nil || metrics.timeToFirstChunk == nil {
		return
	}
	metrics.timeToFirstChunk.Record(ctx, seconds, metric.WithAttributes(metricAttributes(providerName, model, "")...))
}

func (tp *TracingProvider) recordTokenUsage(ctx context.Context, resp *CompletionResponse, providerName, model string) {
	metrics := tp.metrics.Load()
	if metrics == nil || metrics.usage == nil || resp == nil || (resp.InputTokens == 0 && resp.OutputTokens == 0) {
		return
	}
	base := metricAttributes(providerName, model, "")
	if resp.InputTokens > 0 {
		metrics.usage.Record(ctx, int64(resp.InputTokens), metric.WithAttributes(append(base, attribute.String(genai.AttrTokenType, genai.TokenTypeInput))...))
	}
	if resp.OutputTokens > 0 {
		metrics.usage.Record(ctx, int64(resp.OutputTokens), metric.WithAttributes(append(base, attribute.String(genai.AttrTokenType, genai.TokenTypeOutput))...))
	}
}
