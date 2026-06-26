/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"errors"
	"os"
	"runtime/debug"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/sozercan/orka/internal/tracing/genai"
)

// Init initializes OpenTelemetry tracing and metrics. When enabled is false,
// global tracer/meter providers remain the default noop implementations and the
// returned shutdown function is a no-op. W3C propagation is always installed so
// inject/extract calls work consistently in API/controller/worker code paths.
//
// The OTLP gRPC endpoint is read from the standard OTEL_EXPORTER_OTLP_ENDPOINT
// environment variable (default: localhost:4317).
func Init(serviceName string, enabled bool) (shutdown func(ctx context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	noop := func(context.Context) error { return nil }
	if !enabled {
		return noop, nil
	}

	ctx := context.Background()

	res, err := Resource(ctx, serviceName)
	if err != nil {
		return noop, err
	}

	traceExporter, err := newTraceExporter(ctx)
	if err != nil {
		return noop, err
	}
	metricExporter, err := newMetricExporter(ctx)
	if err != nil {
		return noop, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		return suppressExporterShutdownError(shutdownAll(ctx, mp.Shutdown, tp.Shutdown))
	}, nil
}

// Resource builds the OTel resource shared by traces and metrics.
func Resource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(version),
		),
	)
}

// Tracer returns a named tracer instance.
func Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return otel.Tracer(name, opts...)
}

// GenAITracer returns a tracer with the GenAI development schema URL.
func GenAITracer(name string) trace.Tracer {
	return Tracer(name, trace.WithSchemaURL(genai.SchemaURL))
}

// Meter returns a named meter instance.
func Meter(name string, opts ...metric.MeterOption) metric.Meter {
	return otel.Meter(name, opts...)
}

// GenAIMeter returns a meter with the GenAI development schema URL.
func GenAIMeter(name string) metric.Meter {
	return Meter(name, metric.WithSchemaURL(genai.SchemaURL))
}

func newTraceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	if otlpProtocolUsesHTTP("TRACES") {
		return otlptracehttp.New(ctx)
	}
	return otlptracegrpc.New(ctx)
}

func newMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	if otlpProtocolUsesHTTP("METRICS") {
		return otlpmetrichttp.New(ctx)
	}
	return otlpmetricgrpc.New(ctx)
}

func otlpProtocolUsesHTTP(signal string) bool {
	protocol := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL"))
	if protocol == "" {
		protocol = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	return strings.HasPrefix(strings.ToLower(protocol), "http/")
}

func suppressExporterShutdownError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "failed to upload metrics") &&
		(strings.Contains(msg, "connection refused") || strings.Contains(msg, "context deadline exceeded")) {
		return nil
	}
	return err
}

func shutdownAll(ctx context.Context, shutdowns ...func(context.Context) error) error {
	errCh := make(chan error, len(shutdowns))
	for _, shutdown := range shutdowns {
		go func(shutdown func(context.Context) error) {
			errCh <- shutdown(ctx)
		}(shutdown)
	}
	var errs []error
	for range shutdowns {
		if err := <-errCh; err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
