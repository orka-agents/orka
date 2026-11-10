package controller

import (
	"maps"
	"strings"
	"testing"
)

func TestRuntimePoolTelemetryProjection(t *testing.T) {
	values := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "https://user:private@collector.otel.svc:4318/base?token=private#private",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
		"OTEL_EXPORTER_OTLP_HEADERS":          "authorization=private",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":   "authorization=private",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY":       "/private/key",
		"OTEL_RESOURCE_ATTRIBUTES":            "private=value",
		"ORKA_TRACEPARENT":                    "private",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "https://metrics.otel.svc:4318/v1/metrics?token=private",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "authorization=private",
	}
	getenv := func(name string) string { return values[name] }
	if got := runtimePoolTelemetryEnv(false, getenv); len(got) != 0 {
		t.Fatal("disabled runtime received telemetry")
	}
	got := runtimePoolTelemetryEnv(true, getenv)
	// The protocol both signals share is projected once, as the generic name.
	if len(got) != 4 {
		t.Fatalf("projected variable count = %d: %v", len(got), got)
	}
	for _, env := range got {
		if env.ValueFrom != nil || strings.Contains(env.Value, "private") {
			t.Fatal("projected a private collector setting")
		}
		switch env.Name {
		case "ORKA_ENABLE_TELEMETRY":
			if env.Value != "true" {
				t.Fatal("missing explicit enable")
			}
		case "OTEL_EXPORTER_OTLP_ENDPOINT":
			if env.Value != "https://collector.otel.svc:4318/base" {
				t.Fatal("generic endpoint semantics changed")
			}
		case "OTEL_EXPORTER_OTLP_PROTOCOL":
			if env.Value != "http/protobuf" {
				t.Fatal("protocol changed")
			}
		case "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":
			if env.Value != "https://metrics.otel.svc:4318/v1/metrics" {
				t.Fatalf("metrics endpoint = %q", env.Value)
			}
		default:
			t.Fatalf("unexpected telemetry variable %s", env.Name)
		}
	}
	for _, endpoint := range []string{"http://localhost:4318", "http://127.0.0.1:4318", "http://[::1]:4318", "http://0.0.0.0:4318", "%invalid"} {
		values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = endpoint
		if len(runtimePoolTelemetryEnv(true, getenv)) != 0 {
			t.Fatal("unreachable signal endpoint fell back to a different collector")
		}
	}
	values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "https://trace.otel.svc:4318/custom"
	got = runtimePoolTelemetryEnv(true, getenv)
	for _, env := range got {
		if env.Name == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			t.Fatal("signal endpoint unexpectedly projected generic endpoint")
		}
	}
}

func TestRuntimePoolTelemetryChangesTemplateRevision(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.otel.svc:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	pool := runtimePoolTestObject(1)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	before := r.runtimePoolPodTemplate(pool, cfg, selector, "auth", "provider")
	r.EnableTelemetry = true
	after := r.runtimePoolPodTemplate(pool, cfg, selector, "auth", "provider")
	if before.Annotations[runtimePoolTemplateRevisionAnnotation] == after.Annotations[runtimePoolTemplateRevisionAnnotation] {
		t.Fatal("enabling telemetry bypassed the existing drained template rollout")
	}
	env := runtimePoolLiteralEnvironment(after.Spec.Containers[0].Env)
	if env["ORKA_ENABLE_TELEMETRY"] != "true" || env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://collector.otel.svc:4318" {
		t.Fatal("supervisor template lacks telemetry settings")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=private")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "private=value")
	withPrivateSettings := r.runtimePoolPodTemplate(pool, cfg, selector, "auth", "provider")
	if withPrivateSettings.Annotations[runtimePoolTemplateRevisionAnnotation] != after.Annotations[runtimePoolTemplateRevisionAnnotation] {
		t.Fatal("private controller settings entered the supervisor template")
	}
}

func TestRuntimePoolTelemetryMetricsProjection(t *testing.T) {
	values := map[string]string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://traces.otel.svc:4318/v1/traces",
		"OTEL_EXPORTER_OTLP_ENDPOINT":        "http://collector.otel.svc:4318",
		"OTEL_EXPORTER_OTLP_PROTOCOL":        "http/protobuf",
	}
	getenv := func(name string) string { return values[name] }
	env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, getenv))
	// The generic base URL is projected for metrics; trace-specific settings
	// keep precedence for traces under SDK semantics.
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://collector.otel.svc:4318" || env["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" ||
		env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != "http://traces.otel.svc:4318/v1/traces" {
		t.Fatalf("metrics projection = %v", env)
	}
	values["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] = "http://localhost:4318/v1/metrics"
	env = runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, getenv))
	for name := range env {
		if strings.HasPrefix(name, "OTEL_EXPORTER_OTLP_METRICS_") || name == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			t.Fatalf("unreachable metrics endpoint fell back to %s", name)
		}
	}
	if env["ORKA_ENABLE_TELEMETRY"] != "true" {
		t.Fatal("unreachable metrics endpoint disabled traces")
	}
	// No generic endpoint is projected, so the supervisor resolves no metrics
	// collector and needs no explicit disable.
	if _, disabled := env["OTEL_METRICS_EXPORTER"]; disabled || len(env) != 3 {
		t.Fatalf("dropped metrics without a generic endpoint projected %v", env)
	}
	// A metrics setting the supervisor would reject drops metrics, never traces.
	for name, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "metrics.otel.svc:4317",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json",
	} {
		values := map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://traces.otel.svc:4318/v1/traces", name: value}
		env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(key string) string { return values[key] }))
		if env["ORKA_ENABLE_TELEMETRY"] != "true" {
			t.Fatalf("%s=%s disabled traces", name, value)
		}
		for projected := range env {
			if strings.HasPrefix(projected, "OTEL_EXPORTER_OTLP_METRICS_") {
				t.Fatalf("%s=%s projected unsupported metrics setting %s", name, value, projected)
			}
		}
	}
	// Traces exported through the generic endpoint keep its base-path
	// semantics, but dropped metrics are explicitly disabled so the supervisor
	// cannot fall back to that generic collector for metrics.
	for name, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://localhost:4318/v1/metrics",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json",
	} {
		values := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.svc:4318/base", name: value}
		env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(key string) string { return values[key] }))
		if env["ORKA_ENABLE_TELEMETRY"] != "true" || env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://collector.otel.svc:4318/base" {
			t.Fatalf("%s=%s changed generic trace projection: %v", name, value, env)
		}
		if env["OTEL_METRICS_EXPORTER"] != "none" {
			t.Fatalf("%s=%s left metrics to fall back to the generic endpoint: %v", name, value, env)
		}
		for projected := range env {
			if strings.HasPrefix(projected, "OTEL_EXPORTER_OTLP_METRICS_") {
				t.Fatalf("%s=%s projected unsupported metrics setting %s", name, value, projected)
			}
		}
	}
	// A generic endpoint that serves both signals leaves metrics enabled.
	generic := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.svc:4318"}
	if env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(key string) string { return generic[key] })); env["OTEL_METRICS_EXPORTER"] != "" {
		t.Fatalf("generic endpoint disabled metrics: %v", env)
	}
	// Metrics alone enable nothing.
	metricsOnly := map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://metrics.otel.svc:4318/v1/metrics"}
	if got := runtimePoolTelemetryEnv(true, func(name string) string { return metricsOnly[name] }); len(got) != 0 {
		t.Fatalf("metrics-only configuration enabled supervisor telemetry: %v", got)
	}
}

// The native backend caps a container at 32 environment entries, so a setting
// traces and metrics share costs one entry, and one they disagree on keeps
// both signal-specific entries.
func TestRuntimePoolTelemetrySharesScalarsBothSignalsAgreeOn(t *testing.T) {
	scalars := map[string]string{"PROTOCOL": "http/protobuf", "INSECURE": "true", "COMPRESSION": "gzip"}
	for mask := range 1 << len(runtimePoolTelemetryScalarSuffixes) {
		values := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.svc:4318"}
		want := map[string]string{"ORKA_ENABLE_TELEMETRY": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.svc:4318"}
		for i, suffix := range runtimePoolTelemetryScalarSuffixes {
			if mask&(1<<i) != 0 {
				values["OTEL_EXPORTER_OTLP_"+suffix] = scalars[suffix]
				want["OTEL_EXPORTER_OTLP_"+suffix] = scalars[suffix]
			}
		}
		got := runtimePoolTelemetryEnv(true, func(name string) string { return values[name] })
		if env := runtimePoolLiteralEnvironment(got); len(got) != len(want) || !maps.Equal(env, want) {
			t.Fatalf("shared settings %v projected %v, want %v", values, got, want)
		}
	}
	// The same value set per signal is still one shared setting.
	perSignal := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://collector.otel.svc:4318",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":  "http/protobuf",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf",
	}
	env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(name string) string { return perSignal[name] }))
	if len(env) != 3 || env["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" {
		t.Fatalf("equal per-signal protocols projected %v", env)
	}
	// Signals that disagree, or a setting only one signal has, stay specific:
	// the generic name would change the other signal.
	for _, values := range []map[string]string{
		{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"},
		{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf"},
		{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"},
	} {
		values["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://collector.otel.svc:4318"
		env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(name string) string { return values[name] }))
		if _, generic := env["OTEL_EXPORTER_OTLP_PROTOCOL"]; generic || env["OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"] != values["OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"] ||
			env["OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"] != values["OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"] {
			t.Fatalf("differing protocols %v projected %v", values, env)
		}
	}
}

func TestValidateRuntimeTelemetryCollectorEgress(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	valid := env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.otel.svc:4317"})
	if err := ValidateRuntimeTelemetryCollectorEgress(false, false, valid); err != nil {
		t.Fatalf("opt-out rejected: %v", err)
	}
	if err := ValidateRuntimeTelemetryCollectorEgress(true, true, valid); err != nil {
		t.Fatalf("valid collector rejected: %v", err)
	}
	if err := ValidateRuntimeTelemetryCollectorEgress(false, true, valid); err == nil {
		t.Fatal("collector egress admitted without telemetry")
	}
	for name, values := range map[string]map[string]string{
		"no endpoint":       {},
		"loopback only":     {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317"},
		"IP address":        {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://10.0.0.7:4317"},
		"IPv6 address":      {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://[fd00::7]:4317"},
		"scheme-less gRPC":  {"OTEL_EXPORTER_OTLP_ENDPOINT": "collector.otel.svc:4317"},
		"single label":      {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317"},
		"unsupported":       {"OTEL_EXPORTER_OTLP_ENDPOINT": "grpc://collector.otel.svc:4317"},
		"wildcard":          {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://*.otel.svc:4317"},
		"bad metrics":       {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.svc:4317", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://10.0.0.8:4318/v1/metrics"},
		"bad trace signal":  {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://10.0.0.9:4318/v1/traces"},
		"numeric top label": {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.123:4317"},
	} {
		if err := ValidateRuntimeTelemetryCollectorEgress(true, true, env(values)); err == nil {
			t.Errorf("%s: invalid collector endpoint admitted", name)
		}
	}
}
