/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

func detachedSpanContext(ctx context.Context) context.Context {
	detached := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
	return baggage.ContextWithBaggage(detached, baggage.FromContext(ctx))
}
