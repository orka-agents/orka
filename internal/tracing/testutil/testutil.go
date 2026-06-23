package testutil

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
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
