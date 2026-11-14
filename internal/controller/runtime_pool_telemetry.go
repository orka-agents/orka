package controller

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// otlpEndpointSuffix and otlpProtocolSuffix name the
// OTEL_EXPORTER_OTLP_*_ENDPOINT and OTEL_EXPORTER_OTLP_*_PROTOCOL settings.
const (
	otlpEndpointSuffix = "ENDPOINT"
	otlpProtocolSuffix = "PROTOCOL"
)

const (
	runtimePoolTelemetryEndpointEnv        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	runtimePoolTelemetryTracesEndpointEnv  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	runtimePoolTelemetryMetricsEndpointEnv = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	// runtimePoolMetricsExporterEnv set to "none" opts out of metrics export.
	runtimePoolMetricsExporterEnv = "OTEL_METRICS_EXPORTER"
	otlpProtocolHTTPProtobuf      = "http/protobuf"
	// otlpHTTPTracesPath and otlpHTTPMetricsPath are the paths OTLP/HTTP
	// appends to a generic base URL for each signal.
	otlpHTTPTracesPath  = "/v1/traces"
	otlpHTTPMetricsPath = "/v1/metrics"
)

// runtimePoolTelemetryScalarSuffixes are the non-secret exporter settings a
// supervisor accepts for each signal.
var runtimePoolTelemetryScalarSuffixes = []string{otlpProtocolSuffix, "INSECURE", "COMPRESSION"}

// runtimePoolTelemetryEnvBudget is how many environment entries supervisor
// telemetry may use. The native backend admits 32 environment entries per
// container and a native supervisor that cannot suspend already needs 29.
const runtimePoolTelemetryEnvBudget = 3

// runtimePoolOTLPSignal is one signal's resolved exporter configuration.
type runtimePoolOTLPSignal struct {
	// endpoint is a base URL when generic, else the signal's full URL.
	endpoint string
	generic  bool
	// path is the path OTLP/HTTP appends to a generic base URL.
	path string
	// scalars holds PROTOCOL, INSECURE and COMPRESSION values; an explicit
	// grpc protocol is stored empty because the supervisor defaults to it.
	scalars map[string]string
}

// runtimePoolTelemetryEnv follows the native AI worker scalar allowlist for
// supervisor traces and GenAI metrics. Traces gate the projection: a
// metrics-only controller configuration enables nothing. Settings are encoded
// with as few entries as keep their SDK meaning, and metrics are dropped when
// they would take supervisor telemetry past runtimePoolTelemetryEnvBudget.
// Collector egress is operator-owned unless --runtime-telemetry-collector-egress
// admits the projected origins for gateway-enforced runtimes. Never copy
// headers, certificate paths, resource attributes or Task context.
func runtimePoolTelemetryEnv(enabled bool, getenv func(string) string) []corev1.EnvVar {
	if !enabled {
		return nil
	}
	traces := runtimePoolOTLPSignal{path: otlpHTTPTracesPath, scalars: map[string]string{}}
	endpoint := strings.TrimSpace(getenv(runtimePoolTelemetryTracesEndpointEnv))
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv(runtimePoolTelemetryEndpointEnv))
		traces.generic = true
	}
	if !isWorkerReachableOTLPEndpoint(endpoint) {
		return nil
	}
	traces.endpoint = safeWorkerOTLPEnvValue(runtimePoolTelemetryTracesEndpointEnv, endpoint)
	for _, suffix := range runtimePoolTelemetryScalarSuffixes {
		value := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_TRACES_" + suffix))
		if value == "" {
			value = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + suffix))
		}
		traces.scalars[suffix] = normalizedOTLPScalar(suffix, value)
	}
	metrics, ok := runtimePoolMetricsTelemetrySignal(getenv)
	if !ok {
		// Dropped metrics need no disable entry: no generic endpoint is
		// projected, so the supervisor resolves no metrics collector.
		return encodeRuntimePoolTelemetryEnv(traces, nil)
	}
	env := encodeRuntimePoolTelemetryEnv(traces, &metrics)
	if len(env) > runtimePoolTelemetryEnvBudget {
		// Traces take precedence over metrics for the native budget. Metrics
		// are dropped only when that brings telemetry within it; trace
		// settings that exceed it on their own already exceed the limit.
		if traceOnly := encodeRuntimePoolTelemetryEnv(traces, nil); len(traceOnly) <= runtimePoolTelemetryEnvBudget {
			return traceOnly
		}
	}
	return env
}

// normalizedOTLPScalar stores an explicit grpc protocol as unset: the
// supervisor exports gRPC when no protocol is set, so it costs no entry.
func normalizedOTLPScalar(suffix, value string) string {
	if suffix == otlpProtocolSuffix && value == "grpc" {
		return ""
	}
	return value
}

// encodeRuntimePoolTelemetryEnv projects traces, and metrics when non-nil.
// A generic endpoint is projected only when it resolves to both signals'
// URLs, so a dropped signal never falls back to it. A scalar both signals
// agree on is projected once under its generic name.
func encodeRuntimePoolTelemetryEnv(traces runtimePoolOTLPSignal, metrics *runtimePoolOTLPSignal) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "ORKA_ENABLE_TELEMETRY", Value: strconv.FormatBool(true)}}
	switch base, shared := runtimePoolSharedOTLPBase(traces, metrics); {
	case shared:
		env = append(env, corev1.EnvVar{Name: runtimePoolTelemetryEndpointEnv, Value: base})
	case metrics == nil:
		env = append(env, corev1.EnvVar{Name: runtimePoolTelemetryTracesEndpointEnv, Value: traces.signalURL()})
	default:
		env = append(env,
			corev1.EnvVar{Name: runtimePoolTelemetryTracesEndpointEnv, Value: traces.signalURL()},
			corev1.EnvVar{Name: runtimePoolTelemetryMetricsEndpointEnv, Value: metrics.signalURL()},
		)
	}
	for _, suffix := range runtimePoolTelemetryScalarSuffixes {
		trace := traces.scalars[suffix]
		if metrics == nil {
			if trace != "" {
				env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_TRACES_" + suffix, Value: trace})
			}
			continue
		}
		metric := metrics.scalars[suffix]
		if trace == metric {
			if trace != "" {
				env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_" + suffix, Value: trace})
			}
			continue
		}
		if trace != "" {
			env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_TRACES_" + suffix, Value: trace})
		}
		if metric != "" {
			env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_METRICS_" + suffix, Value: metric})
		}
	}
	return env
}

// httpProtocol reports whether the signal exports OTLP/HTTP, whose generic
// endpoint is a base URL; gRPC treats generic and signal endpoints alike.
func (signal runtimePoolOTLPSignal) httpProtocol() bool {
	return signal.scalars[otlpProtocolSuffix] == otlpProtocolHTTPProtobuf
}

// signalURL is the signal-specific endpoint equivalent to the signal's
// configuration, appending the signal path to a generic OTLP/HTTP base URL
// exactly as the SDK does.
func (signal runtimePoolOTLPSignal) signalURL() string {
	if !signal.generic {
		return signal.endpoint
	}
	return otlpHTTPSignalURL(signal.endpoint, signal.path, signal.httpProtocol())
}

func otlpHTTPSignalURL(base, signalPath string, httpProtocol bool) string {
	if !httpProtocol {
		return base
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return base
	}
	parsed.Path = path.Join(parsed.Path, signalPath)
	parsed.RawPath = ""
	return parsed.String()
}

// runtimePoolSharedOTLPBase returns a generic endpoint that resolves to every
// exported signal's URL under SDK semantics. A trace-only projection never
// shares one, because metrics would resolve it too.
func runtimePoolSharedOTLPBase(traces runtimePoolOTLPSignal, metrics *runtimePoolOTLPSignal) (string, bool) {
	if metrics == nil {
		return "", false
	}
	var candidates []string
	for _, signal := range []runtimePoolOTLPSignal{traces, *metrics} {
		switch {
		case signal.generic:
			candidates = append(candidates, signal.endpoint)
		case !signal.httpProtocol():
			candidates = append(candidates, signal.endpoint)
		default:
			if parsed, err := url.Parse(signal.endpoint); err == nil && strings.HasSuffix(parsed.Path, signal.path) {
				parsed.Path = strings.TrimSuffix(parsed.Path, signal.path)
				parsed.RawPath = ""
				candidates = append(candidates, parsed.String())
			}
		}
	}
	for _, base := range candidates {
		if sameOTLPURL(otlpHTTPSignalURL(base, traces.path, traces.httpProtocol()), traces.signalURL(), traces.httpProtocol()) &&
			sameOTLPURL(otlpHTTPSignalURL(base, metrics.path, metrics.httpProtocol()), metrics.signalURL(), metrics.httpProtocol()) {
			return base, true
		}
	}
	return "", false
}

// sameOTLPURL compares two endpoint URLs as the exporter would use them. An
// OTLP/HTTP signal URL without a path posts to "/".
func sameOTLPURL(a, b string, httpProtocol bool) bool {
	left, err := url.Parse(a)
	if err != nil {
		return false
	}
	right, err := url.Parse(b)
	if err != nil {
		return false
	}
	if httpProtocol {
		for _, parsed := range []*url.URL{left, right} {
			if parsed.Path == "" {
				parsed.Path = "/"
			}
			parsed.RawPath = ""
		}
	}
	return left.String() == right.String()
}

// runtimePoolMetricsTelemetrySignal resolves the metrics exporter for
// supervisor GenAI metrics. An unreachable metrics-specific endpoint drops
// metrics instead of falling back to a different collector, as worker Jobs
// do. The supervisor disables all telemetry on any setting it does not
// accept, so a metrics setting it would reject drops metrics rather than
// traces. It reports false when metrics are dropped.
func runtimePoolMetricsTelemetrySignal(getenv func(string) string) (runtimePoolOTLPSignal, bool) {
	// The standard opt-out wins over any endpoint, including the generic one.
	if strings.EqualFold(strings.TrimSpace(getenv(runtimePoolMetricsExporterEnv)), "none") {
		return runtimePoolOTLPSignal{}, false
	}
	metrics := runtimePoolOTLPSignal{path: otlpHTTPMetricsPath, scalars: map[string]string{}}
	endpoint := strings.TrimSpace(getenv(runtimePoolTelemetryMetricsEndpointEnv))
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv(runtimePoolTelemetryEndpointEnv))
		metrics.generic = true
	}
	metrics.endpoint = safeWorkerOTLPEnvValue(runtimePoolTelemetryMetricsEndpointEnv, endpoint)
	if !isWorkerReachableOTLPEndpoint(endpoint) || !supervisorAcceptsOTLPSetting(otlpEndpointSuffix, metrics.endpoint) {
		return runtimePoolOTLPSignal{}, false
	}
	for _, suffix := range runtimePoolTelemetryScalarSuffixes {
		value := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_METRICS_" + suffix))
		if value == "" {
			value = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + suffix))
		}
		if value != "" && !supervisorAcceptsOTLPSetting(suffix, value) {
			return runtimePoolOTLPSignal{}, false
		}
		metrics.scalars[suffix] = normalizedOTLPScalar(suffix, value)
	}
	return metrics, true
}

// supervisorAcceptsOTLPSetting mirrors the ACP supervisor's fail-closed
// exporter settings check (workers/acp/supervisor/telemetry.go).
func supervisorAcceptsOTLPSetting(suffix, value string) bool {
	switch suffix {
	case otlpEndpointSuffix:
		parsed, err := url.Parse(value)
		return err == nil && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
			(parsed.Scheme == urlSchemeHTTP || parsed.Scheme == urlSchemeHTTPS)
	case otlpProtocolSuffix:
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
