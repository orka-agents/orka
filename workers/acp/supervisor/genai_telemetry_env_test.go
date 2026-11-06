package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSupervisorTelemetryMetricsFromEnv(t *testing.T) {
	var metricRequests atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			metricRequests.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", collector.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "true")
	telemetry, shutdown, err := NewSupervisorTelemetryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if telemetry.Tracer != nil || telemetry.GenAITracer != nil || telemetry.Meter == nil {
		t.Fatal("metrics-only configuration must enable metrics and no tracing")
	}
	g := newSupervisorGenAI(nil, telemetry.Meter, providerKindCodex, "gpt-5")
	g.invokeAgentDuration.Record(context.Background(), 1)
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if metricRequests.Load() == 0 {
		t.Fatal("supervisor metrics were not exported on shutdown")
	}
}

func TestSupervisorTelemetryMetricsFailClosed(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{"metrics headers", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "authorization=PRIVATE_CANARY"},
		{"metrics protocol", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "invalid-PRIVATE_CANARY"},
		{"metrics endpoint", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://user:PRIVATE_CANARY@collector.otel.svc"},
		{"temporality", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "delta"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/v1/metrics")
			t.Setenv(test.key, test.value)
			telemetry, shutdown, err := NewSupervisorTelemetryFromEnv()
			if err == nil || telemetry.Tracer != nil || telemetry.GenAITracer != nil || telemetry.Meter != nil {
				t.Fatal("unsupported metrics configuration did not disable supervisor telemetry")
			}
			if strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("configuration error exposed value")
			}
			if err := shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSupervisorTelemetryTracesProvideGenAITracer(t *testing.T) {
	t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
	telemetry, shutdown, err := NewSupervisorTelemetryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if telemetry.Tracer == nil || telemetry.GenAITracer == nil || telemetry.Meter != nil {
		t.Fatal("trace-only configuration must enable both tracers and no metrics")
	}
}
