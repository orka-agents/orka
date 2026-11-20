/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestWithRemoteTraceParentContinuesTheCallerTrace(t *testing.T) {
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx, err := withRemoteTraceParent(context.Background(), traceparent)
	if err != nil {
		t.Fatal(err)
	}
	if got := trace.SpanContextFromContext(ctx).TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace ID = %s", got)
	}
	// The tool executor injects exactly this context into the proxy request.
	header := http.Header{}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(header))
	if got := header.Get("traceparent"); got != traceparent {
		t.Fatalf("injected traceparent = %q", got)
	}
}

func TestWithRemoteTraceParentIsOptionalAndRejectsInvalidValues(t *testing.T) {
	ctx := context.Background()
	got, err := withRemoteTraceParent(ctx, "  ")
	if err != nil || got != ctx {
		t.Fatalf("empty traceparent changed the context: %v", err)
	}
	if _, err := withRemoteTraceParent(ctx, "00-not-a-trace-01"); err == nil {
		t.Fatal("invalid traceparent was accepted")
	}
}
