/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package testutil

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// SpanHarness installs an in-memory span recorder for a test.
type SpanHarness struct {
	Recorder *tracetest.SpanRecorder
	Provider *sdktrace.TracerProvider
}

func NewSpanHarness(t *testing.T) *SpanHarness {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return &SpanHarness{Recorder: recorder, Provider: provider}
}

// MetricHarness installs an in-memory manual metric reader for a test.
type MetricHarness struct {
	Reader   *sdkmetric.ManualReader
	Provider *sdkmetric.MeterProvider
}

func NewMetricHarness(t *testing.T) *MetricHarness {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previous)
	})
	return &MetricHarness{Reader: reader, Provider: provider}
}

func (h *MetricHarness) Collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.Reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	return rm
}

// AttributeMap converts span attributes to a key/value map for assertions.
func AttributeMap(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	attrs := map[string]attribute.Value{}
	if span == nil {
		return attrs
	}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	return attrs
}

// SpanNamed returns the first span with the given name.
func SpanNamed(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

// SpansNamed returns all spans with the given name in recorder order.
func SpansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	out := []sdktrace.ReadOnlySpan{}
	for _, span := range spans {
		if span.Name() == name {
			out = append(out, span)
		}
	}
	return out
}
