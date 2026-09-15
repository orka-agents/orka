package controller

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const runtimePoolTelemetryEndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// runtimePoolTelemetryEnv follows the native AI worker scalar allowlist, limited
// to traces. This does not grant collector egress; that route is operator-owned.
// Never copy headers, certificate paths, resource attributes or Task context.
func runtimePoolTelemetryEnv(enabled bool, getenv func(string) string) []corev1.EnvVar {
	if !enabled {
		return nil
	}
	endpoint := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv(runtimePoolTelemetryEndpointEnv))
	}
	if !isWorkerReachableOTLPEndpoint(endpoint) {
		return nil
	}
	env := []corev1.EnvVar{
		{Name: "ORKA_ENABLE_TELEMETRY", Value: strconv.FormatBool(enabled)},
	}
	for _, suffix := range []string{"ENDPOINT", "PROTOCOL", "INSECURE", "COMPRESSION"} {
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
	return env
}
