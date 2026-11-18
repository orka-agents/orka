package supervisor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
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

// The controller projects a setting both signals share under its generic
// name. Both exporters must honour it, and each supervisor's metrics must
// carry its own instance identity and nothing else from the environment.
func TestSupervisorTelemetrySharedSettingsAndMetricInstanceIdentity(t *testing.T) {
	var traceRequests atomic.Int32
	bodies := make(chan []byte, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/traces":
			traceRequests.Add(1)
		case "/v1/metrics":
			body, _ := io.ReadAll(r.Body)
			bodies <- body
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	for _, name := range []string{"TRACES_ENDPOINT", "METRICS_ENDPOINT", "TRACES_PROTOCOL", "METRICS_PROTOCOL"} {
		t.Setenv("OTEL_EXPORTER_OTLP_"+name, "")
	}
	instances := map[string]bool{}
	for range 2 {
		telemetry, shutdown, err := NewSupervisorTelemetryFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		_, span := telemetry.Tracer.Start(context.Background(), "acp.supervisor.test")
		span.End()
		newSupervisorGenAI(nil, telemetry.Meter, providerKindCodex, "gpt-5").invokeAgentDuration.Record(context.Background(), 1)
		if err := shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		var request collectormetrics.ExportMetricsServiceRequest
		select {
		case body := <-bodies:
			if err := proto.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("the generic protocol did not reach the metrics exporter")
		}
		if len(request.ResourceMetrics) != 1 {
			t.Fatalf("resource metrics = %d", len(request.ResourceMetrics))
		}
		attributes := map[string]string{}
		for _, kv := range request.ResourceMetrics[0].Resource.Attributes {
			attributes[kv.Key] = kv.Value.GetStringValue()
		}
		instance := attributes["service.instance.id"]
		if _, err := uuid.Parse(instance); err != nil || len(attributes) != 2 || attributes["service.name"] != "orka-acp-supervisor" {
			t.Fatalf("metric resource = %v, want only service.name and a UUID service.instance.id", attributes)
		}
		instances[instance] = true
	}
	if len(instances) != 2 {
		t.Fatal("two supervisor boots exported metrics as one instance")
	}
	if traceRequests.Load() == 0 {
		t.Fatal("the generic protocol did not reach the trace exporter")
	}
}

func TestSupervisorMetricExporterDropsAmbientResourceAttributes(t *testing.T) {
	recorded := &recordingMetricExporter{}
	own := newSupervisorMetricResource()
	ambient := resource.NewSchemaless(attribute.String("service.name", "other"), attribute.String("service.instance.id", "other"),
		attribute.String("host.name", "PRIVATE_HOST"), attribute.String("k8s.pod.name", "PRIVATE_POD"))
	if err := (safeMetricExporter{Exporter: recorded, resource: own}).Export(context.Background(), &metricdata.ResourceMetrics{Resource: ambient}); err != nil {
		t.Fatal(err)
	}
	if recorded.resource == nil || !recorded.resource.Equal(own) || recorded.resource.Len() != 2 {
		t.Fatalf("exported resource = %v, want %v", recorded.resource, own)
	}
}

type recordingMetricExporter struct {
	sdkmetric.Exporter
	resource *resource.Resource
}

func (e *recordingMetricExporter) Export(_ context.Context, metrics *metricdata.ResourceMetrics) error {
	e.resource = metrics.Resource
	return nil
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
