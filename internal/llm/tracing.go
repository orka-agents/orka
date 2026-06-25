/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"strings"
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
	inner             Provider
	operationDuration metric.Float64Histogram
	usage             metric.Int64Histogram
	timeToFirstChunk  metric.Float64Histogram
}

// NewTracingProvider returns a Provider that records a GenAI span and metrics
// for every LLM call. Legacy llm.* attributes are dual-emitted during migration.
func NewTracingProvider(p Provider) Provider {
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
	return &TracingProvider{
		inner:             p,
		operationDuration: operationDuration,
		usage:             usage,
		timeToFirstChunk:  timeToFirstChunk,
	}
}

func (tp *TracingProvider) Name() string { return tp.inner.Name() }

func (tp *TracingProvider) TelemetryProviderName() string { return ProviderTelemetryName(tp.inner) }

func (tp *TracingProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	providerName := ProviderTelemetryName(tp.inner)
	attrs := requestAttributes(req, providerName, false)
	tracer := tracing.GenAITracer(genai.InstrumentationName)
	ctx, span := tracer.Start(ctx, spanName(req), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()

	resp, err := tp.inner.Complete(ctx, req)
	durationSeconds := time.Since(start).Seconds()
	if err != nil {
		errType := errorType(err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if errType != "" {
			span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
		}
		tp.recordOperationDuration(ctx, durationSeconds, providerName, requestModel(req), errType)
		return nil, err
	}

	providerName = responseProviderName(resp, tp.inner)
	setResponseAttributes(span, resp, providerName)
	modelName := responseModel(resp, req)
	tp.recordOperationDuration(ctx, durationSeconds, providerName, modelName, "")
	tp.recordTokenUsage(ctx, resp, providerName, modelName)
	return resp, nil
}

func (tp *TracingProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	start := time.Now()
	providerName := ProviderTelemetryName(tp.inner)
	attrs := requestAttributes(req, providerName, true)
	tracer := tracing.GenAITracer(genai.InstrumentationName)
	ctx, span := tracer.Start(ctx, spanName(req), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))

	innerCh, err := tp.inner.Stream(ctx, req)
	if err != nil {
		errType := errorType(err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if errType != "" {
			span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
		}
		tp.recordOperationDuration(ctx, time.Since(start).Seconds(), providerName, requestModel(req), errType)
		span.End()
		return nil, err
	}

	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		var finishReasons []string
		var errType string
		defer func() {
			if len(finishReasons) > 0 {
				span.SetAttributes(attribute.StringSlice(genai.AttrResponseFinishReasons, finishReasons))
			}
			tp.recordOperationDuration(ctx, time.Since(start).Seconds(), providerName, requestModel(req), errType)
			span.End()
		}()

		var firstChunkRecorded bool
		for chunk := range innerCh {
			if !firstChunkRecorded && (chunk.Content != "" || chunk.ToolCall != nil || chunk.Done || chunk.Error != nil) {
				firstChunkRecorded = true
				latency := time.Since(start).Seconds()
				span.SetAttributes(attribute.Float64(genai.AttrResponseTimeToFirstChunk, latency))
				tp.recordTimeToFirstChunk(ctx, latency, providerName, requestModel(req))
			}
			if chunk.StopReason != "" {
				finishReasons = []string{chunk.StopReason}
			}
			if chunk.Error != nil {
				errType = errorType(chunk.Error)
				span.RecordError(chunk.Error)
				span.SetStatus(codes.Error, chunk.Error.Error())
				if errType != "" {
					span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
				}
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				if ctx.Err() != nil {
					errType = errorType(ctx.Err())
					span.RecordError(ctx.Err())
					span.SetStatus(codes.Error, ctx.Err().Error())
					span.SetAttributes(attribute.String(genai.AttrErrorType, errType))
				}
				return
			}
		}
	}()
	return out, nil
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
		attrs = append(attrs, attribute.StringSlice(genai.AttrRequestStopSequences, req.StopSequences))
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
		attribute.Int(genai.AttrUsageInputTokens, resp.InputTokens),
		attribute.Int(genai.AttrUsageOutputTokens, resp.OutputTokens),
		attribute.Int("llm.input_tokens", resp.InputTokens),
		attribute.Int("llm.output_tokens", resp.OutputTokens),
		attribute.Int("llm.tool_calls", len(resp.ToolCalls)),
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
	if tp.operationDuration == nil {
		return
	}
	tp.operationDuration.Record(ctx, seconds, metric.WithAttributes(metricAttributes(providerName, model, errType)...))
}

func (tp *TracingProvider) recordTimeToFirstChunk(ctx context.Context, seconds float64, providerName, model string) {
	if tp.timeToFirstChunk == nil {
		return
	}
	tp.timeToFirstChunk.Record(ctx, seconds, metric.WithAttributes(metricAttributes(providerName, model, "")...))
}

func (tp *TracingProvider) recordTokenUsage(ctx context.Context, resp *CompletionResponse, providerName, model string) {
	if tp.usage == nil || resp == nil {
		return
	}
	base := metricAttributes(providerName, model, "")
	tp.usage.Record(ctx, int64(resp.InputTokens), metric.WithAttributes(append(base, attribute.String(genai.AttrTokenType, genai.TokenTypeInput))...))
	tp.usage.Record(ctx, int64(resp.OutputTokens), metric.WithAttributes(append(base, attribute.String(genai.AttrTokenType, genai.TokenTypeOutput))...))
}
