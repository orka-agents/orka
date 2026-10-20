package controller

import (
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
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "https://private-metrics",
	}
	getenv := func(name string) string { return values[name] }
	if got := runtimePoolTelemetryEnv(false, getenv); len(got) != 0 {
		t.Fatal("disabled runtime received telemetry")
	}
	got := runtimePoolTelemetryEnv(true, getenv)
	if len(got) != 3 {
		t.Fatalf("projected variable count = %d", len(got))
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
		case "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":
			if env.Value != "http/protobuf" {
				t.Fatal("protocol changed")
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
