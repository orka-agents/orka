/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

func TestInit(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{
			name:    "disabled returns noop shutdown",
			enabled: false,
		},
		{
			name:    "enabled creates provider",
			enabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.enabled {
				t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "100")
			}
			shutdown, err := Init("test-service", tt.enabled)
			if err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if shutdown == nil {
				t.Fatal("Init() returned nil shutdown")
			}
			// Shutdown should not error, even when no local collector is running.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				t.Fatalf("shutdown() error = %v", err)
			}
		})
	}
}

func TestTracer(t *testing.T) {
	tracer := Tracer("test")
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}

func TestResourceIncludesEnvironmentAttributes(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=dev,orka.cluster=kind")

	res, err := Resource(context.Background(), "test-service")
	if err != nil {
		t.Fatalf("Resource() error = %v", err)
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, attr := range res.Attributes() {
		attrs[attr.Key] = attr.Value
	}
	if got := attrs["deployment.environment"].AsString(); got != "dev" {
		t.Fatalf("deployment.environment = %q, want dev", got)
	}
	if got := attrs["orka.cluster"].AsString(); got != "kind" {
		t.Fatalf("orka.cluster = %q, want kind", got)
	}
}

func TestOTLPSignalEnabled(t *testing.T) {
	t.Run("default endpoint enables both signals", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4317")
		if !otlpSignalEnabled("TRACES") || !otlpSignalEnabled("METRICS") {
			t.Fatal("generic endpoint should enable both signals")
		}
	})

	t.Run("trace specific endpoint enables traces only", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "collector:4317")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
		if !otlpSignalEnabled("TRACES") {
			t.Fatal("traces endpoint should enable traces")
		}
		if otlpSignalEnabled("METRICS") {
			t.Fatal("traces-only endpoint should not enable metrics")
		}
	})

	t.Run("no endpoints keeps localhost default behavior", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
		if !otlpSignalEnabled("TRACES") || !otlpSignalEnabled("METRICS") {
			t.Fatal("no endpoint override should keep both default exporters enabled")
		}
	})
}

func TestOTLPProtocolSelectsHTTPExporter(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")

	if !otlpProtocolUsesHTTP("TRACES") {
		t.Fatal("traces protocol should select HTTP exporter")
	}
	if !otlpProtocolUsesHTTP("METRICS") {
		t.Fatal("metrics protocol should select HTTP exporter")
	}
	traceExporter, err := newTraceExporter(context.Background())
	if err != nil {
		t.Fatalf("newTraceExporter() error = %v", err)
	}
	defer func() { _ = traceExporter.Shutdown(context.Background()) }()

	metricExporter, err := newMetricExporter(context.Background())
	if err != nil {
		t.Fatalf("newMetricExporter() error = %v", err)
	}
	defer func() { _ = metricExporter.Shutdown(context.Background()) }()
}
