package supervisor

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type telemetryGRPCReceiver struct {
	collectortrace.UnimplementedTraceServiceServer
	t        *testing.T
	received chan *collectortrace.ExportTraceServiceRequest
}

func (r *telemetryGRPCReceiver) Export(ctx context.Context, request *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get("authorization")) != 0 {
		r.t.Error("inherited exporter credential reached collector")
	}
	r.received <- request
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

// telemetryGRPCMetricsReceiver accepts the GenAI metrics a generic OTLP
// endpoint also enables.
type telemetryGRPCMetricsReceiver struct {
	collectormetrics.UnimplementedMetricsServiceServer
}

func (telemetryGRPCMetricsReceiver) Export(context.Context, *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

func TestSupervisorTelemetryGRPCEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	receiver := &telemetryGRPCReceiver{t: t, received: make(chan *collectortrace.ExportTraceServiceRequest, 2)}
	collectortrace.RegisterTraceServiceServer(server, receiver)
	collectormetrics.RegisterMetricsServiceServer(server, telemetryGRPCMetricsReceiver{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	for _, prefix := range []string{"http://"} {
		t.Run("scheme="+prefix, func(t *testing.T) {
			t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", prefix+listener.Addr().String())
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")
			tracer, shutdown, err := NewTelemetryFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			_, span := tracer.Start(context.Background(), "acp.supervisor.test")
			span.End()
			func() {
				defer func() { _ = recover() }()
				_, panicking := tracer.Start(context.Background(), "acp.supervisor.panic")
				defer panicking.End()
				panic("SYNTHETIC_TOKEN_PANIC_VALUE")
			}()
			if err := shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-receiver.received:
				if len(request.ResourceSpans) != 1 {
					t.Fatal("missing exported span")
				}
				attrs := request.ResourceSpans[0].Resource.Attributes
				if len(attrs) != 1 || attrs[0].Key != "service.name" || attrs[0].Value.GetStringValue() != "orka-acp-supervisor" {
					t.Fatal("ambient resource attributes crossed supervisor exporter boundary")
				}
				exported := 0
				for _, scope := range request.ResourceSpans[0].ScopeSpans {
					for _, span := range scope.Spans {
						exported++
						if len(span.Events) != 0 {
							t.Fatalf("span %q exported %d events, want none", span.Name, len(span.Events))
						}
					}
				}
				if exported != 2 {
					t.Fatalf("exported spans = %d, want 2", exported)
				}
			default:
				t.Fatal("no real OTLP gRPC export")
			}
		})
	}
}

// OTEL_METRICS_EXPORTER=none keeps a generic endpoint for traces from becoming
// the metrics collector.
func TestSupervisorTelemetryMetricsExporterNone(t *testing.T) {
	t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	telemetry, shutdown, err := NewSupervisorTelemetryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if telemetry.Tracer == nil || telemetry.GenAITracer == nil {
		t.Fatal("metrics opt-out disabled traces")
	}
	if telemetry.Meter != nil {
		t.Fatal("generic trace endpoint enabled metrics despite OTEL_METRICS_EXPORTER=none")
	}
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	if _, _, err := NewSupervisorTelemetryFromEnv(); err == nil {
		t.Fatal("OTEL_METRICS_EXPORTER other than none was accepted")
	}
}

// A separate process captures the SDK's actual initialization diagnostics without
// changing global logging hooks in the test process.
func TestSupervisorTelemetryRejectsUnsafeEnvironment(t *testing.T) {
	if os.Getenv("ORKA_TELEMETRY_PREFLIGHT_FIXTURE") == "true" {
		tracer, shutdown, err := NewTelemetryFromEnv()
		if err == nil || tracer != nil || err.Error() != "unsupported supervisor telemetry configuration" {
			t.Fatal("unsafe telemetry configuration was not rejected with a fixed diagnostic")
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatal("disabled telemetry shutdown failed")
		}
		return
	}
	for name, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS":            "authorization=PRIVATE_CANARY%invalid",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":     "PRIVATE_CANARY",
		"OTEL_RESOURCE_ATTRIBUTES":              "private=PRIVATE_CANARY%invalid",
		"OTEL_SERVICE_NAME":                     "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": "/PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_TIMEOUT":            "PRIVATE_CANARY",
		"OTEL_TRACES_SAMPLER":                   "PRIVATE_CANARY",
		"OTEL_BSP_MAX_QUEUE_SIZE":               "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_PROTOCOL":           "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":    "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_INSECURE":           "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE":    "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_COMPRESSION":        "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION": "PRIVATE_CANARY",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":    "http://PRIVATE_CANARY%invalid",
		"OTEL_EXPORTER_OTLP_ENDPOINT":           "http://PRIVATE_CANARY%invalid",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorTelemetryRejectsUnsafeEnvironment$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "OTEL_") && !strings.HasPrefix(entry, "ORKA_ENABLE_TELEMETRY=") {
					command.Env = append(command.Env, entry)
				}
			}
			command.Env = append(command.Env, "ORKA_TELEMETRY_PREFLIGHT_FIXTURE=true", "ORKA_ENABLE_TELEMETRY=true",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:1", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc", name+"="+value)
			output, err := command.CombinedOutput()
			if bytes.Contains(output, []byte("PRIVATE_CANARY")) {
				t.Fatal("SDK initialization logged an unsafe value")
			}
			if err != nil {
				t.Fatalf("preflight subprocess failed: %s", output)
			}
		})
	}
}
