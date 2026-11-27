package controller

import (
	"cmp"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	otlpEndpointSuffix = "ENDPOINT"
	otlpProtocolSuffix = "PROTOCOL"
)

const (
	runtimePoolTelemetryEndpointEnv        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	runtimePoolTelemetryTracesEndpointEnv  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	runtimePoolTelemetryMetricsEndpointEnv = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	runtimePoolMetricsExporterEnv          = "OTEL_METRICS_EXPORTER"
	otlpProtocolHTTPProtobuf               = "http/protobuf"
	otlpHTTPTracesPath                     = "/v1/traces"
	otlpHTTPMetricsPath                    = "/v1/metrics"
)

var runtimePoolTelemetryScalarSuffixes = []string{otlpProtocolSuffix, "INSECURE", "COMPRESSION"}

const nativeRuntimeEnvironmentLimit = 32

const runtimePoolTelemetryEnableEnv = "ORKA_ENABLE_TELEMETRY"

type runtimePoolOTLPSignal struct {
	endpoint string
	generic  bool
	path     string
	scalars  map[string]string
}

func runtimePoolTelemetryEnv(enabled bool, getenv func(string) string) []corev1.EnvVar {
	traces, metrics, ok := runtimePoolTelemetrySignals(enabled, getenv)
	if !ok {
		return nil
	}
	return encodeRuntimePoolTelemetryEnv(traces, metrics)
}

func isRuntimePoolTelemetryEnvName(name string) bool {
	return name == runtimePoolTelemetryEnableEnv || strings.HasPrefix(name, "OTEL_EXPORTER_OTLP_")
}

func fitNativeRuntimeTelemetry(env []corev1.EnvVar, enabled bool, getenv func(string) string) ([]corev1.EnvVar, bool, string) {
	base := make([]corev1.EnvVar, 0, len(env))
	var current []corev1.EnvVar
	for _, variable := range env {
		if isRuntimePoolTelemetryEnvName(variable.Name) {
			current = append(current, variable)
			continue
		}
		base = append(base, variable)
	}
	traces, metrics, ok := runtimePoolTelemetrySignals(enabled, getenv)
	if !ok {
		return base, true, ""
	}
	room := nativeRuntimeEnvironmentLimit - len(base)
	full := encodeRuntimePoolTelemetryEnv(traces, metrics)
	if len(full) <= room {
		if reflect.DeepEqual(current, full) {
			return env, true, "supervisor telemetry is configured on the native runtime"
		}
		return append(base, full...), true, "supervisor telemetry is configured on the native runtime"
	}
	if metrics != nil {
		if traceOnly := encodeRuntimePoolTelemetryEnv(traces, nil); len(traceOnly) <= room {
			return append(base, traceOnly...), false, fmt.Sprintf(
				"supervisor metrics are not exported: the native runtime has room for %d of its %d telemetry environment entries (backend limit %d)",
				max(room, 0), len(full), nativeRuntimeEnvironmentLimit)
		}
	}
	return base, false, fmt.Sprintf(
		"supervisor telemetry is not exported: the native runtime has room for %d of its %d telemetry environment entries (backend limit %d)",
		max(room, 0), len(full), nativeRuntimeEnvironmentLimit)
}

func telemetryCollectorEndpoints(env []corev1.EnvVar) []string {
	var endpoints []string
	for _, variable := range env {
		if isRuntimePoolTelemetryEnvName(variable.Name) && strings.HasSuffix(variable.Name, "_ENDPOINT") {
			endpoints = append(endpoints, variable.Value)
		}
	}
	return endpoints
}

func runtimePoolTelemetrySignals(enabled bool, getenv func(string) string) (runtimePoolOTLPSignal, *runtimePoolOTLPSignal, bool) {
	traces := runtimePoolOTLPSignal{path: otlpHTTPTracesPath, scalars: map[string]string{}}
	if !enabled {
		return traces, nil, false
	}
	endpoint := strings.TrimSpace(getenv(runtimePoolTelemetryTracesEndpointEnv))
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv(runtimePoolTelemetryEndpointEnv))
		traces.generic = true
	}
	if !isWorkerReachableOTLPEndpoint(endpoint) {
		return traces, nil, false
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
		return traces, nil, true
	}
	return traces, &metrics, true
}

func normalizedOTLPScalar(suffix, value string) string {
	if suffix == otlpProtocolSuffix && value == "grpc" {
		return ""
	}
	return value
}

func encodeRuntimePoolTelemetryEnv(traces runtimePoolOTLPSignal, metrics *runtimePoolOTLPSignal) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: runtimePoolTelemetryEnableEnv, Value: strconv.FormatBool(true)}}
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

func (signal runtimePoolOTLPSignal) httpProtocol() bool {
	return signal.scalars[otlpProtocolSuffix] == otlpProtocolHTTPProtobuf
}

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

func runtimePoolMetricsTelemetrySignal(getenv func(string) string) (runtimePoolOTLPSignal, bool) {
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

func runtimePoolTelemetryCollectorEndpoints(enabled bool, getenv func(string) string) []string {
	return telemetryCollectorEndpoints(runtimePoolTelemetryEnv(enabled, getenv))
}

func runtimePoolConfiguredCollectorEndpoints(getenv func(string) string) []string {
	return runtimePoolTelemetryCollectorEndpoints(true, getenv)
}

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

func ValidateRuntimeTelemetryCollectorEgress(telemetryEnabled, collectorEgress bool, getenv func(string) string) error {
	if !collectorEgress {
		return nil
	}
	if !telemetryEnabled {
		return fmt.Errorf("runtime telemetry collector egress requires --enable-telemetry")
	}
	endpoints := runtimePoolConfiguredCollectorEndpoints(getenv)
	if len(endpoints) == 0 {
		return fmt.Errorf("runtime telemetry collector egress requires a runtime-reachable OTLP trace endpoint")
	}
	for _, endpoint := range endpoints {
		if err := validateRuntimeTelemetryCollectorOrigin(endpoint); err != nil {
			return err
		}
		host, _, _, _ := externalEgressOrigin(endpoint)
		if err := externalEgressHostResolvable("the OTLP collector endpoint", host); err != nil {
			return fmt.Errorf("runtime telemetry collector egress: %w", err)
		}
	}
	return runtimePoolTelemetryTransportConflict(runtimePoolTelemetryEnv(true, getenv))
}

func runtimePoolTelemetryTransportConflict(env []corev1.EnvVar) error {
	values := runtimePoolLiteralEnvironment(env)
	for _, signal := range []string{"traces", "metrics"} {
		prefix := "OTEL_EXPORTER_OTLP_" + strings.ToUpper(signal) + "_"
		endpoint := cmp.Or(values[prefix+otlpEndpointSuffix], values[runtimePoolTelemetryEndpointEnv])
		insecure := cmp.Or(values[prefix+"INSECURE"], values["OTEL_EXPORTER_OTLP_INSECURE"])
		if endpoint == "" || insecure == "" {
			continue
		}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			continue
		}
		if cleartext := strings.EqualFold(parsed.Scheme, urlSchemeHTTP); strings.EqualFold(insecure, "true") != cleartext {
			return fmt.Errorf("runtime telemetry collector egress: the %s collector endpoint %q uses %s but its OTLP INSECURE setting is %q; "+
				"the egress rule follows the scheme, so make them agree", signal, endpoint, parsed.Scheme, insecure)
		}
	}
	return nil
}
