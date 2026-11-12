package controller

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// otlpEndpointSuffix names the OTEL_EXPORTER_OTLP_*_ENDPOINT settings.
const otlpEndpointSuffix = "ENDPOINT"

const (
	runtimePoolTelemetryEndpointEnv        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	runtimePoolTelemetryTracesEndpointEnv  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	runtimePoolTelemetryMetricsEndpointEnv = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	runtimePoolTelemetryTracesProtocolEnv  = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	otlpProtocolHTTPProtobuf               = "http/protobuf"
	// otlpHTTPTracesPath is the path OTLP/HTTP appends to a generic base URL
	// for traces.
	otlpHTTPTracesPath = "/v1/traces"
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
	for _, suffix := range append([]string{otlpEndpointSuffix}, runtimePoolTelemetryScalarSuffixes...) {
		name := "OTEL_EXPORTER_OTLP_TRACES_" + suffix
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			value = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + suffix))
			// Generic HTTP endpoints are base URLs; signal endpoints include the
			// signal path. Preserve SDK semantics when projecting a generic URL.
			if suffix == otlpEndpointSuffix {
				name = runtimePoolTelemetryEndpointEnv
			}
		}
		if value != "" {
			env = append(env, corev1.EnvVar{Name: name, Value: safeWorkerOTLPEnvValue(name, value)})
		}
	}
	metrics, ok := runtimePoolMetricsTelemetryEnv(env, getenv)
	if !ok {
		// A generic trace endpoint must not become the metrics collector the
		// controller just refused to project. Without a generic endpoint the
		// supervisor resolves no metrics collector, so it needs no disable
		// entry from the native environment budget.
		return runtimePoolTraceOnlyTelemetryEnv(env)
	}
	return shareRuntimePoolTelemetryScalars(env, metrics)
}

// runtimePoolTraceOnlyTelemetryEnv projects a generic trace endpoint under the
// trace-specific name. OTLP/HTTP treats a generic endpoint as a base URL and a
// signal endpoint as the full URL, so the trace path is appended exactly as the
// SDK would; gRPC treats both alike.
func runtimePoolTraceOnlyTelemetryEnv(env []corev1.EnvVar) []corev1.EnvVar {
	generic := slices.IndexFunc(env, func(variable corev1.EnvVar) bool { return variable.Name == runtimePoolTelemetryEndpointEnv })
	if generic < 0 {
		return env
	}
	env[generic].Name = runtimePoolTelemetryTracesEndpointEnv
	protocol := slices.IndexFunc(env, func(variable corev1.EnvVar) bool { return variable.Name == runtimePoolTelemetryTracesProtocolEnv })
	if protocol < 0 || env[protocol].Value != otlpProtocolHTTPProtobuf {
		return env
	}
	if parsed, err := url.Parse(env[generic].Value); err == nil {
		parsed.Path = path.Join(parsed.Path, otlpHTTPTracesPath)
		parsed.RawPath = ""
		env[generic].Value = parsed.String()
	}
	return env
}

// shareRuntimePoolTelemetryScalars projects a scalar that traces and metrics
// agree on once, under its generic name, which the supervisor and the SDK
// resolve for both signals. The native backend admits 32 environment entries
// per container, so a setting both signals share must not cost two of them.
func shareRuntimePoolTelemetryScalars(traces, metrics []corev1.EnvVar) []corev1.EnvVar {
	for _, suffix := range runtimePoolTelemetryScalarSuffixes {
		trace := slices.IndexFunc(traces, func(variable corev1.EnvVar) bool { return variable.Name == "OTEL_EXPORTER_OTLP_TRACES_"+suffix })
		metric := slices.IndexFunc(metrics, func(variable corev1.EnvVar) bool { return variable.Name == "OTEL_EXPORTER_OTLP_METRICS_"+suffix })
		if trace < 0 || metric < 0 || traces[trace].Value != metrics[metric].Value {
			continue
		}
		traces[trace].Name = "OTEL_EXPORTER_OTLP_" + suffix
		metrics = slices.Delete(metrics, metric, metric+1)
	}
	return append(traces, metrics...)
}

// runtimePoolMetricsTelemetryEnv projects the metrics exporter for supervisor
// GenAI metrics. An unreachable metrics-specific endpoint drops metrics
// instead of falling back to a different collector, as worker Jobs do. The
// supervisor disables all telemetry on any setting it does not accept, so a
// metrics setting it would reject drops metrics rather than traces. It reports
// false when metrics are dropped.
func runtimePoolMetricsTelemetryEnv(projected []corev1.EnvVar, getenv func(string) string) ([]corev1.EnvVar, bool) {
	var env []corev1.EnvVar
	if endpoint := strings.TrimSpace(getenv(runtimePoolTelemetryMetricsEndpointEnv)); endpoint != "" {
		value := safeWorkerOTLPEnvValue(runtimePoolTelemetryMetricsEndpointEnv, endpoint)
		if !isWorkerReachableOTLPEndpoint(endpoint) || !supervisorAcceptsOTLPSetting(otlpEndpointSuffix, value) {
			return nil, false
		}
		env = append(env, corev1.EnvVar{Name: runtimePoolTelemetryMetricsEndpointEnv, Value: value})
	} else {
		generic := strings.TrimSpace(getenv(runtimePoolTelemetryEndpointEnv))
		value := safeWorkerOTLPEnvValue(runtimePoolTelemetryEndpointEnv, generic)
		if !isWorkerReachableOTLPEndpoint(generic) || !supervisorAcceptsOTLPSetting(otlpEndpointSuffix, value) {
			return nil, false
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
			return nil, false
		}
		env = append(env, corev1.EnvVar{Name: name, Value: value})
	}
	return env, true
}

// supervisorAcceptsOTLPSetting mirrors the ACP supervisor's fail-closed
// exporter settings check (workers/acp/supervisor/telemetry.go).
func supervisorAcceptsOTLPSetting(suffix, value string) bool {
	switch suffix {
	case otlpEndpointSuffix:
		parsed, err := url.Parse(value)
		return err == nil && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
			(parsed.Scheme == urlSchemeHTTP || parsed.Scheme == urlSchemeHTTPS)
	case "PROTOCOL":
		return value == "grpc" || value == otlpProtocolHTTPProtobuf
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
