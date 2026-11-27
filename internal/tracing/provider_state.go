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

func GlobalTracerProvider() trace.TracerProvider { return otel.GetTracerProvider() }

func GlobalMeterProvider() metric.MeterProvider { return otel.GetMeterProvider() }

func GlobalTracerProviderExplicitNoop() bool {
	return isOTelProviderType(otel.GetTracerProvider(), otelTraceNoopPkg, otelTracerProvider)
}

func IsDefaultGlobalTracerProvider(provider any) bool {
	return isOTelProviderType(provider, otelGlobalPkg, "tracerProvider")
}

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
