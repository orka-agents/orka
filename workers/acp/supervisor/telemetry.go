package supervisor

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	supervisorTelemetryTimeout    = 2 * time.Second
	supervisorTelemetryTrueValue  = "true"
	supervisorTelemetryHTTPScheme = "http"
	supervisorTelemetryTLSScheme  = "https"
)

// NewTelemetryFromEnv creates a supervisor-owned trace provider. It never changes
// global providers, imports resource attributes, or instruments provider children.
// No endpoint (or no explicit enable flag) means no exporter. Callers must treat
// initialization failure as disabled telemetry, not as an execution failure.
func NewTelemetryFromEnv() (trace.Tracer, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("ORKA_ENABLE_TELEMETRY") != supervisorTelemetryTrueValue {
		return nil, noop, nil
	}
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	if endpoint == "" {
		return nil, noop, nil
	}
	if !supervisorTelemetryEnvironmentSafe() {
		return nil, noop, errors.New("unsupported supervisor telemetry configuration")
	}
	protocol := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"))
	if protocol == "" {
		protocol = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), supervisorTelemetryTimeout)
	defer cancel()
	var exporter sdktrace.SpanExporter
	var err error
	switch protocol {
	case "http/protobuf":
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithHeaders(nil), otlptracehttp.WithTimeout(supervisorTelemetryTimeout))
	case "", "grpc":
		exporter, err = otlptracegrpc.New(ctx, otlptracegrpc.WithHeaders(nil), otlptracegrpc.WithTimeout(supervisorTelemetryTimeout))
	default:
		return nil, noop, errors.New("unsupported supervisor telemetry protocol")
	}
	if err != nil {
		return nil, noop, errors.New("supervisor telemetry initialization failed")
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(supervisorTraceResource),
		sdktrace.WithBatcher(safeTelemetryExporter{exporter}, sdktrace.WithExportTimeout(supervisorTelemetryTimeout)),
	)
	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, supervisorTelemetryTimeout)
		defer cancel()
		if err := provider.Shutdown(ctx); err != nil {
			return errors.New("supervisor telemetry shutdown incomplete")
		}
		return nil
	}
	return provider.Tracer("orka.acp.supervisor"), shutdown, nil
}

// The SDK reports asynchronous exporter errors. Never let collector response
// bodies, URLs or transport diagnostics enter supervisor logs through that path.
type safeTelemetryExporter struct{ sdktrace.SpanExporter }

func (e safeTelemetryExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	// The SDK merges OTEL_RESOURCE_ATTRIBUTES even with an explicit resource.
	// Enforce the supervisor resource allowlist at the exporter boundary.
	safeSpans := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		safeSpans[i] = supervisorExportSpan{span}
	}
	if err := e.SpanExporter.ExportSpans(ctx, safeSpans); err != nil {
		return errors.New("supervisor telemetry export failed")
	}
	return nil
}

func (e safeTelemetryExporter) Shutdown(ctx context.Context) error {
	if err := e.SpanExporter.Shutdown(ctx); err != nil {
		return errors.New("supervisor telemetry exporter shutdown incomplete")
	}
	return nil
}

var supervisorTraceResource = resource.NewSchemaless(attribute.String("service.name", "orka-acp-supervisor"))

type supervisorExportSpan struct{ sdktrace.ReadOnlySpan }

func (supervisorExportSpan) Resource() *resource.Resource { return supervisorTraceResource }

// SDK constructors read environment values before applying explicit options and
// may log malformed inputs. Reject unsupported ambient configuration before any
// SDK initialization; managed Pods receive only these non-secret scalar settings.
func supervisorTelemetryEnvironmentSafe() bool {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		value = strings.TrimSpace(value)
		if value == "" || !strings.HasPrefix(name, "OTEL_") {
			continue
		}
		switch name {
		case "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":
			endpoint, err := url.Parse(value)
			if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
				(endpoint.Scheme != supervisorTelemetryHTTPScheme && endpoint.Scheme != supervisorTelemetryTLSScheme) {
				return false
			}
		case "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":
			if value != "grpc" && value != "http/protobuf" {
				return false
			}
		case "OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE":
			if !strings.EqualFold(value, supervisorTelemetryTrueValue) && !strings.EqualFold(value, "false") {
				return false
			}
		case "OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION":
			if value != "gzip" && value != "none" {
				return false
			}
		case "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT":
			// Content capture is deliberately ignored.
		default:
			return false
		}
	}
	return true
}
