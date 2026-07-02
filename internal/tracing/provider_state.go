/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"reflect"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	otelGlobalPkg      = "go.opentelemetry.io/otel/internal/global"
	otelTraceNoopPkg   = "go.opentelemetry.io/otel/trace/noop"
	otelMetricNoopPkg  = "go.opentelemetry.io/otel/metric/noop"
	otelTracerProvider = "TracerProvider"
	otelMeterProvider  = "MeterProvider"
)

// GlobalTracerProvider returns the currently configured OpenTelemetry tracer provider.
func GlobalTracerProvider() trace.TracerProvider { return otel.GetTracerProvider() }

// GlobalMeterProvider returns the currently configured OpenTelemetry meter provider.
func GlobalMeterProvider() metric.MeterProvider { return otel.GetMeterProvider() }

// GlobalTracerProviderExplicitNoop reports whether tracing was explicitly set
// to the OpenTelemetry noop tracer provider. The default global tracer provider
// is not considered inactive because OpenTelemetry auto-instrumentation can
// activate it without changing its concrete type.
func GlobalTracerProviderExplicitNoop() bool {
	return isOTelProviderType(otel.GetTracerProvider(), otelTraceNoopPkg, otelTracerProvider)
}

// IsDefaultGlobalTracerProvider reports whether provider is OpenTelemetry's
// default delegating global tracer provider. That provider returns no-op spans
// unless an SDK or auto-instrumentation delegate is active, so hot paths can
// defer expensive span attribute construction until a returned span records.
func IsDefaultGlobalTracerProvider(provider any) bool {
	return isOTelProviderType(provider, otelGlobalPkg, "tracerProvider")
}

// GlobalMeterProviderActive reports whether a real OpenTelemetry meter provider
// is currently configured. Unlike the tracer provider, the default global meter
// provider only records no-op measurements until an SDK meter provider is set.
func GlobalMeterProviderActive() bool {
	provider := otel.GetMeterProvider()
	return !isOTelProviderType(provider, otelGlobalPkg, "meterProvider") &&
		!isOTelProviderType(provider, otelMetricNoopPkg, otelMeterProvider)
}

func isOTelProviderType(provider any, pkgPath, name string) bool {
	typ := reflect.TypeOf(provider)
	if typ == nil {
		return true
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ.PkgPath() == pkgPath && typ.Name() == name
}

// SameProvider reports whether two provider interface values refer to the same
// comparable concrete provider. It returns false for non-comparable provider
// implementations instead of panicking on interface equality.
func SameProvider(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	typ := reflect.TypeOf(a)
	if typ != reflect.TypeOf(b) || !typ.Comparable() {
		return false
	}
	return a == b
}
