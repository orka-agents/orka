package supervisor

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func (s *Server) traceOperation(r *http.Request, metadata harnessv2.MutationMetadata, operation string) (*http.Request, trace.Span) {
	ctx := trace.ContextWithSpanContext(r.Context(), trace.SpanContext{})
	ctx = propagation.TraceContext{}.Extract(ctx, propagation.HeaderCarrier(r.Header))
	if s.cfg.Tracer == nil {
		return r.WithContext(ctx), noop.Span{}
	}
	ctx, span := s.cfg.Tracer.Start(ctx, "acp.supervisor."+operation,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("orka.task.uid", string(metadata.TaskUID)),
			attribute.Int64("orka.acp.task.attempt", int64(metadata.TaskAttempt)),
			attribute.String("orka.acp.prompt.id", string(metadata.PromptID)),
			attribute.String("orka.acp.operation.id", string(metadata.OperationID)),
			attribute.String("orka.acp.runtime_pool.uid", string(metadata.Fence.RuntimePoolUID)),
			attribute.String("orka.acp.runtime_session.uid", string(metadata.Fence.RuntimeSessionUID)),
			attribute.Int64("orka.acp.runtime_session.generation", int64(metadata.Fence.RuntimeSessionGeneration)),
		),
	)
	return r.WithContext(ctx), span
}
