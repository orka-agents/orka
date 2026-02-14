/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sozercan/mercan/internal/tracing"
)

// TracingProvider wraps an LLM Provider with OpenTelemetry tracing.
type TracingProvider struct {
	inner Provider
}

// NewTracingProvider returns a Provider that records a span for every LLM call.
func NewTracingProvider(p Provider) Provider {
	return &TracingProvider{inner: p}
}

func (tp *TracingProvider) Name() string { return tp.inner.Name() }

func (tp *TracingProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	tracer := tracing.Tracer("mercan.llm")
	ctx, span := tracer.Start(ctx, "llm.complete",
		trace.WithAttributes(
			attribute.String("llm.provider", tp.inner.Name()),
			attribute.String("llm.model", req.Model),
		),
	)
	defer span.End()

	resp, err := tp.inner.Complete(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	span.SetAttributes(
		attribute.Int("llm.input_tokens", resp.InputTokens),
		attribute.Int("llm.output_tokens", resp.OutputTokens),
		attribute.Int("llm.tool_calls", len(resp.ToolCalls)),
	)
	return resp, nil
}

func (tp *TracingProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	return tp.inner.Stream(ctx, req)
}
