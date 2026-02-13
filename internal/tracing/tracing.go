/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"runtime/debug"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Init initializes OpenTelemetry tracing. When enabled is false, the global
// tracer provider remains the default noop implementation and the returned
// shutdown function is a no-op.
//
// The OTLP gRPC endpoint is read from the standard OTEL_EXPORTER_OTLP_ENDPOINT
// environment variable (default: localhost:4317).
func Init(serviceName string, enabled bool) (shutdown func(ctx context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if !enabled {
		return noop, nil
	}

	ctx := context.Background()

	// Build resource with service name and version
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(version),
		),
	)
	if err != nil {
		return noop, err
	}

	// Create OTLP gRPC exporter (reads OTEL_EXPORTER_OTLP_ENDPOINT env var)
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return noop, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}, nil
}

// Tracer returns a named tracer instance.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}
