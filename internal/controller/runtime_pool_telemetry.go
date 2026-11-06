package controller

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	runtimePoolTelemetryEndpointEnv        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	runtimePoolTelemetryTracesEndpointEnv  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	runtimePoolTelemetryMetricsEndpointEnv = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
)

// runtimePoolTelemetryScalarSuffixes are the non-secret exporter settings a
// supervisor accepts for each signal.
var runtimePoolTelemetryScalarSuffixes = []string{"PROTOCOL", "INSECURE", "COMPRESSION"}

// runtimePoolTelemetryEnv follows the native AI worker scalar allowlist for
// supervisor traces and GenAI metrics. Traces gate the projection: a
// metrics-only controller configuration enables nothing. Collector egress is
// operator-owned unless --runtime-telemetry-collector-egress admits the
// projected origins for gateway-enforced runtimes. Never copy headers,
// certificate paths, resource attributes or Task context.
func runtimePoolTelemetryEnv(enabled bool, getenv func(string) string) []corev1.EnvVar {
	if !enabled {
		return nil
	}
	endpoint := strings.TrimSpace(getenv(runtimePoolTelemetryTracesEndpointEnv))
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv(runtimePoolTelemetryEndpointEnv))
	}
	if !isWorkerReachableOTLPEndpoint(endpoint) {
		return nil
	}
	env := []corev1.EnvVar{
		{Name: "ORKA_ENABLE_TELEMETRY", Value: strconv.FormatBool(enabled)},
	}
	for _, suffix := range append([]string{"ENDPOINT"}, runtimePoolTelemetryScalarSuffixes...) {
		name := "OTEL_EXPORTER_OTLP_TRACES_" + suffix
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			value = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + suffix))
			// Generic HTTP endpoints are base URLs; signal endpoints include the
			// signal path. Preserve SDK semantics when projecting a generic URL.
			if suffix == "ENDPOINT" {
				name = runtimePoolTelemetryEndpointEnv
			}
		}
		if value != "" {
			env = append(env, corev1.EnvVar{Name: name, Value: safeWorkerOTLPEnvValue(name, value)})
		}
	}
	return append(env, runtimePoolMetricsTelemetryEnv(env, getenv)...)
}

// runtimePoolMetricsTelemetryEnv projects the metrics exporter for supervisor
// GenAI metrics. An unreachable metrics-specific endpoint drops metrics
// instead of falling back to a different collector, as worker Jobs do. The
// supervisor disables all telemetry on any setting it does not accept, so a
// metrics setting it would reject drops metrics rather than traces.
func runtimePoolMetricsTelemetryEnv(projected []corev1.EnvVar, getenv func(string) string) []corev1.EnvVar {
	var env []corev1.EnvVar
	if endpoint := strings.TrimSpace(getenv(runtimePoolTelemetryMetricsEndpointEnv)); endpoint != "" {
		value := safeWorkerOTLPEnvValue(runtimePoolTelemetryMetricsEndpointEnv, endpoint)
		if !isWorkerReachableOTLPEndpoint(endpoint) || !supervisorAcceptsOTLPSetting("ENDPOINT", value) {
			return nil
		}
		env = append(env, corev1.EnvVar{Name: runtimePoolTelemetryMetricsEndpointEnv, Value: value})
	} else {
		generic := strings.TrimSpace(getenv(runtimePoolTelemetryEndpointEnv))
		value := safeWorkerOTLPEnvValue(runtimePoolTelemetryEndpointEnv, generic)
		if !isWorkerReachableOTLPEndpoint(generic) || !supervisorAcceptsOTLPSetting("ENDPOINT", value) {
			return nil
		}
		if !slices.ContainsFunc(projected, func(variable corev1.EnvVar) bool { return variable.Name == runtimePoolTelemetryEndpointEnv }) {
			// The generic base URL keeps its SDK meaning; a trace-specific
			// endpoint still takes precedence for traces.
			env = append(env, corev1.EnvVar{Name: runtimePoolTelemetryEndpointEnv, Value: value})
		}
	}
	for _, suffix := range runtimePoolTelemetryScalarSuffixes {
		name := "OTEL_EXPORTER_OTLP_METRICS_" + suffix
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			value = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + suffix))
		}
		if value == "" {
			continue
		}
		if !supervisorAcceptsOTLPSetting(suffix, value) {
			return nil
		}
		env = append(env, corev1.EnvVar{Name: name, Value: value})
	}
	return env
}

// supervisorAcceptsOTLPSetting mirrors the ACP supervisor's fail-closed
// exporter settings check (workers/acp/supervisor/telemetry.go).
func supervisorAcceptsOTLPSetting(suffix, value string) bool {
	switch suffix {
	case "ENDPOINT":
		parsed, err := url.Parse(value)
		return err == nil && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
			(parsed.Scheme == urlSchemeHTTP || parsed.Scheme == urlSchemeHTTPS)
	case "PROTOCOL":
		return value == "grpc" || value == "http/protobuf"
	case "INSECURE":
		return strings.EqualFold(value, "true") || strings.EqualFold(value, "false")
	case "COMPRESSION":
		return value == "gzip" || value == "none"
	default:
		return false
	}
}

// runtimePoolTelemetryCollectorEndpoints returns the collector endpoints the
// supervisor environment projection hands to runtimes, in a stable order.
func runtimePoolTelemetryCollectorEndpoints(enabled bool, getenv func(string) string) []string {
	var endpoints []string
	for _, variable := range runtimePoolTelemetryEnv(enabled, getenv) {
		if strings.HasSuffix(variable.Name, "_ENDPOINT") {
			endpoints = append(endpoints, variable.Value)
		}
	}
	return endpoints
}

// validateRuntimeTelemetryCollectorOrigin validates one projected OTLP endpoint for
// the gateway egress allowlist. Admission is fail-closed: the endpoint must be
// an absolute http(s) URL naming a fully qualified DNS host, never an IP
// address, wildcard or scheme-less gRPC target, so the allowlisted origin is
// exactly the collector the supervisor exports to.
func validateRuntimeTelemetryCollectorOrigin(raw string) error {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), urlSchemeHTTP+"://") && !strings.HasPrefix(strings.ToLower(raw), urlSchemeHTTPS+"://") {
		return fmt.Errorf("OTLP collector endpoint must be an absolute http or https URL")
	}
	host, _, _, err := externalEgressOrigin(raw)
	if err != nil {
		return fmt.Errorf("OTLP collector endpoint: %w", err)
	}
	if strings.Contains(host, "*") {
		return fmt.Errorf("OTLP collector endpoint must name one host")
	}
	return nil
}

// ValidateRuntimeTelemetryCollectorEgress checks the controller configuration
// for --runtime-telemetry-collector-egress at startup. The opt-in requires
// telemetry and at least one projected collector endpoint, and every projected
// endpoint must be expressible as an allowlisted origin.
func ValidateRuntimeTelemetryCollectorEgress(telemetryEnabled, collectorEgress bool, getenv func(string) string) error {
	if !collectorEgress {
		return nil
	}
	if !telemetryEnabled {
		return fmt.Errorf("runtime telemetry collector egress requires --enable-telemetry")
	}
	endpoints := runtimePoolTelemetryCollectorEndpoints(true, getenv)
	if len(endpoints) == 0 {
		return fmt.Errorf("runtime telemetry collector egress requires a runtime-reachable OTLP trace endpoint")
	}
	for _, endpoint := range endpoints {
		if err := validateRuntimeTelemetryCollectorOrigin(endpoint); err != nil {
			return err
		}
	}
	return nil
}
