/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func StampTaskTraceContext(ctx context.Context, task *corev1alpha1.Task) {
	if task == nil || !trace.SpanContextFromContext(ctx).IsValid() {
		return
	}
	carrier := MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	traceparent := carrier.Get("traceparent")
	if traceparent == "" {
		return
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationTraceParent] = traceparent
	if tracestate := carrier.Get("tracestate"); tracestate != "" {
		task.Annotations[labels.AnnotationTraceState] = tracestate
	}
	delete(task.Annotations, labels.AnnotationTraceBaggage)
}

func ExtractTaskTraceContext(ctx context.Context, task *corev1alpha1.Task) context.Context {
	if task == nil || task.Annotations == nil {
		return ctx
	}
	carrier := MapCarrier{
		"traceparent": task.Annotations[labels.AnnotationTraceParent],
		"tracestate":  task.Annotations[labels.AnnotationTraceState],
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
