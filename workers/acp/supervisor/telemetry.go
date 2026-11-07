package supervisor

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/tracing/genai"
)

// OTLP exporter protocols the supervisor accepts.
const (
	otlpProtocolGRPC         = "grpc"
	otlpProtocolHTTPProtobuf = "http/protobuf"
)

const (
	supervisorTelemetryTimeout    = 2 * time.Second
	supervisorTelemetryTrueValue  = "true"
	supervisorTelemetryHTTPScheme = "http"
	supervisorTelemetryTLSScheme  = "https"
)

// Telemetry is the supervisor-owned instrumentation. Nil members are disabled.
type Telemetry struct {
	// Tracer records acp.supervisor.* operation spans.
	Tracer trace.Tracer
	// GenAITracer records invoke_agent, chat and execute_tool spans derived
	// from the ACP session stream and the provider proxy.
	GenAITracer trace.Tracer
	// Meter records GenAI client metrics.
	Meter metric.Meter
}

// NewTelemetryFromEnv creates a supervisor-owned trace provider. It never changes
// global providers, imports resource attributes, or instruments provider children.
// No endpoint (or no explicit enable flag) means no exporter. Callers must treat
// initialization failure as disabled telemetry, not as an execution failure.
func NewTelemetryFromEnv() (trace.Tracer, func(context.Context) error, error) {
	telemetry, shutdown, err := NewSupervisorTelemetryFromEnv()
	return telemetry.Tracer, shutdown, err
}

// NewSupervisorTelemetryFromEnv creates the supervisor's trace and metric
// providers. Traces need a trace endpoint; metrics need a metrics endpoint and
// are independent of traces. Neither changes global providers, and both fail
// closed to disabled on any unsupported configuration.
func NewSupervisorTelemetryFromEnv() (Telemetry, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("ORKA_ENABLE_TELEMETRY") != supervisorTelemetryTrueValue {
		return Telemetry{}, noop, nil
	}
	traceEndpoint := supervisorSignalEnv("TRACES", "ENDPOINT")
	metricsEndpoint := supervisorSignalEnv("METRICS", "ENDPOINT")
	if strings.TrimSpace(os.Getenv(supervisorMetricsExporterEnv)) == supervisorExporterNone {
		// The controller drops metrics it cannot project safely; a generic
		// endpoint kept for traces must not become the metrics collector.
		metricsEndpoint = ""
	}
	if traceEndpoint == "" && metricsEndpoint == "" {
		return Telemetry{}, noop, nil
	}
	if !supervisorTelemetryEnvironmentSafe() {
		return Telemetry{}, noop, errors.New("unsupported supervisor telemetry configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), supervisorTelemetryTimeout)
	defer cancel()
	var telemetry Telemetry
	var shutdowns []func(context.Context) error
	if traceEndpoint != "" {
		exporter, err := newSupervisorTraceExporter(ctx)
		if err != nil {
			return Telemetry{}, noop, err
		}
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithResource(supervisorTraceResource),
			sdktrace.WithBatcher(safeTelemetryExporter{exporter}, sdktrace.WithExportTimeout(supervisorTelemetryTimeout)),
		)
		telemetry.Tracer = provider.Tracer("orka.acp.supervisor")
		telemetry.GenAITracer = provider.Tracer(genai.InstrumentationName, trace.WithSchemaURL(genai.SchemaURL))
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	if metricsEndpoint != "" {
		exporter, err := newSupervisorMetricExporter(ctx)
		if err != nil {
			for _, shutdown := range shutdowns {
				_ = shutdown(ctx)
			}
			return Telemetry{}, noop, err
		}
		provider := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(supervisorTraceResource),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(safeMetricExporter{exporter}, sdkmetric.WithTimeout(supervisorTelemetryTimeout))),
		)
		telemetry.Meter = provider.Meter(genai.InstrumentationName, metric.WithSchemaURL(genai.SchemaURL))
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, supervisorTelemetryTimeout)
		defer cancel()
		// Providers flush concurrently so the total stays within one timeout.
		results := make(chan error, len(shutdowns))
		for _, shutdown := range shutdowns {
			go func() { results <- shutdown(ctx) }()
		}
		var incomplete bool
		for range shutdowns {
			if err := <-results; err != nil {
				incomplete = true
			}
		}
		if incomplete {
			return errors.New("supervisor telemetry shutdown incomplete")
		}
		return nil
	}
	return telemetry, shutdown, nil
}

// supervisorSignalEnv resolves one OTLP exporter setting with SDK precedence:
// the signal-specific value, then the generic value.
func supervisorSignalEnv(signal, suffix string) string {
	if value := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_" + suffix)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_" + suffix))
}

func newSupervisorTraceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	var exporter sdktrace.SpanExporter
	var err error
	switch supervisorSignalEnv("TRACES", "PROTOCOL") {
	case otlpProtocolHTTPProtobuf:
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithHeaders(nil), otlptracehttp.WithTimeout(supervisorTelemetryTimeout))
	case "", otlpProtocolGRPC:
		exporter, err = otlptracegrpc.New(ctx, otlptracegrpc.WithHeaders(nil), otlptracegrpc.WithTimeout(supervisorTelemetryTimeout))
	default:
		return nil, errors.New("unsupported supervisor telemetry protocol")
	}
	if err != nil {
		return nil, errors.New("supervisor telemetry initialization failed")
	}
	return exporter, nil
}

func newSupervisorMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	var exporter sdkmetric.Exporter
	var err error
	switch supervisorSignalEnv("METRICS", "PROTOCOL") {
	case otlpProtocolHTTPProtobuf:
		exporter, err = otlpmetrichttp.New(ctx, otlpmetrichttp.WithHeaders(nil), otlpmetrichttp.WithTimeout(supervisorTelemetryTimeout))
	case "", otlpProtocolGRPC:
		exporter, err = otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithHeaders(nil), otlpmetricgrpc.WithTimeout(supervisorTelemetryTimeout))
	default:
		return nil, errors.New("unsupported supervisor telemetry protocol")
	}
	if err != nil {
		return nil, errors.New("supervisor telemetry initialization failed")
	}
	return exporter, nil
}

// safeMetricExporter enforces the supervisor resource allowlist and keeps
// collector diagnostics out of supervisor logs, as safeTelemetryExporter does.
type safeMetricExporter struct{ sdkmetric.Exporter }

func (e safeMetricExporter) Export(ctx context.Context, metrics *metricdata.ResourceMetrics) error {
	if metrics != nil {
		safe := *metrics
		safe.Resource = supervisorTraceResource
		metrics = &safe
	}
	if err := e.Exporter.Export(ctx, metrics); err != nil {
		return errors.New("supervisor telemetry export failed")
	}
	return nil
}

func (e safeMetricExporter) ForceFlush(ctx context.Context) error {
	if err := e.Exporter.ForceFlush(ctx); err != nil {
		return errors.New("supervisor telemetry flush incomplete")
	}
	return nil
}

func (e safeMetricExporter) Shutdown(ctx context.Context) error {
	if err := e.Exporter.Shutdown(ctx); err != nil {
		return errors.New("supervisor telemetry exporter shutdown incomplete")
	}
	return nil
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

const (
	// supervisorMetricsExporterEnv accepts only "none", which disables metrics.
	supervisorMetricsExporterEnv = "OTEL_METRICS_EXPORTER"
	supervisorExporterNone       = "none"
)

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
		case "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":
			endpoint, err := url.Parse(value)
			if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
				(endpoint.Scheme != supervisorTelemetryHTTPScheme && endpoint.Scheme != supervisorTelemetryTLSScheme) {
				return false
			}
		case "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL":
			if value != otlpProtocolGRPC && value != otlpProtocolHTTPProtobuf {
				return false
			}
		case "OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE", "OTEL_EXPORTER_OTLP_METRICS_INSECURE":
			if !strings.EqualFold(value, supervisorTelemetryTrueValue) && !strings.EqualFold(value, "false") {
				return false
			}
		case "OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION":
			if value != "gzip" && value != "none" {
				return false
			}
		case supervisorMetricsExporterEnv:
			if value != supervisorExporterNone {
				return false
			}
		case "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT":
			// Content capture is deliberately ignored: GenAI spans and metrics
			// derived from the ACP stream never carry prompt, completion, tool
			// argument or tool result content, whatever this value says.
		default:
			return false
		}
	}
	return true
}
