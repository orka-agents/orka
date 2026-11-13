package controller

import (
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
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
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "https://collector.otel.svc:4318/base/v1/metrics?token=private",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "authorization=private",
	}
	getenv := func(name string) string { return values[name] }
	if got := runtimePoolTelemetryEnv(false, getenv); len(got) != 0 {
		t.Fatal("disabled runtime received telemetry")
	}
	got := runtimePoolTelemetryEnv(true, getenv)
	// The metrics URL is the generic base URL's, so both signals share one
	// endpoint, and the protocol they share is projected once.
	if len(got) != 3 {
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
	// Different OTLP/HTTP collectors per signal need an endpoint each plus the
	// protocol, past the native budget, so traces win and metrics are dropped.
	env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, getenv))
	if len(env) != 3 || env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != "http://traces.otel.svc:4318/v1/traces" || env["OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"] != "http/protobuf" {
		t.Fatalf("over-budget metrics projection = %v", env)
	}
	// Over gRPC the generic URL is the metrics endpoint, projected under the
	// metrics name; trace-specific settings keep precedence for traces.
	grpc := map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"], "OTEL_EXPORTER_OTLP_ENDPOINT": values["OTEL_EXPORTER_OTLP_ENDPOINT"]}
	env = runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(name string) string { return grpc[name] }))
	if len(env) != 3 || env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != "http://traces.otel.svc:4318/v1/traces" || env["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] != "http://collector.otel.svc:4318" {
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
	// Dropped metrics must not fall back to a generic collector projected for
	// traces, so that endpoint moves to the trace-specific name: gRPC keeps it
	// as is, and OTLP/HTTP gets the trace path the SDK appends to a base URL.
	// No disable entry is spent from the native environment budget.
	for _, protocol := range []string{"", "grpc", "http/protobuf"} {
		want := "http://collector.otel.svc:4318/base"
		if protocol == "http/protobuf" {
			want += "/v1/traces"
		}
		for name, value := range map[string]string{
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://localhost:4318/v1/metrics",
			"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json",
		} {
			values := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.otel.svc:4318/base", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": protocol, name: value}
			env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(key string) string { return values[key] }))
			if env["ORKA_ENABLE_TELEMETRY"] != "true" || env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != want {
				t.Fatalf("%s %s=%s changed generic trace projection: %v", protocol, name, value, env)
			}
			if _, generic := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; generic {
				t.Fatalf("%s %s=%s left metrics to fall back to the generic endpoint: %v", protocol, name, value, env)
			}
			for projected := range env {
				if strings.HasPrefix(projected, "OTEL_EXPORTER_OTLP_METRICS_") || projected == "OTEL_METRICS_EXPORTER" {
					t.Fatalf("%s %s=%s projected metrics setting %s", protocol, name, value, projected)
				}
			}
		}
	}
	// The trace path joins a root or empty base path the way the SDK does.
	for base, want := range map[string]string{
		"http://collector.otel.svc:4318":  "http://collector.otel.svc:4318/v1/traces",
		"http://collector.otel.svc:4318/": "http://collector.otel.svc:4318/v1/traces",
	} {
		values := map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": base, "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://localhost:4318/v1/metrics",
		}
		if got := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(key string) string { return values[key] })); got["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] != want {
			t.Fatalf("base %q projected %v", base, got)
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
	// the generic name would change the other signal. gRPC is the
	// supervisor's default, so an explicit grpc costs no entry.
	for _, test := range []struct{ values, want map[string]string }{
		{
			map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"},
			map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf"},
		},
		{
			map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf"},
			map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf"},
		},
		{
			map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf"},
			map[string]string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf"},
		},
		{map[string]string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"}, map[string]string{}},
	} {
		test.values["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://collector.otel.svc:4318"
		test.want["ORKA_ENABLE_TELEMETRY"] = "true"
		test.want["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://collector.otel.svc:4318"
		if env := runtimePoolLiteralEnvironment(runtimePoolTelemetryEnv(true, func(name string) string { return test.values[name] })); !maps.Equal(env, test.want) {
			t.Fatalf("differing protocols %v projected %v, want %v", test.values, env, test.want)
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

// runtimePoolTelemetryCombination is one controller OTLP configuration.
type runtimePoolTelemetryCombination struct {
	values map[string]string
	// metrics reports that the configuration fits the native budget with
	// metrics, so they must stay enabled.
	metrics bool
}

// runtimePoolTelemetryCombinations covers generic, trace and metrics
// endpoints, equivalent or different, with gRPC, OTLP/HTTP and mixed
// protocols. Configurations without a trace endpoint enable nothing and are
// left out.
func runtimePoolTelemetryCombinations() map[string]runtimePoolTelemetryCombination {
	const generic = "http://collector.otel.svc.cluster.local:4318"
	protocols := map[string]map[string]string{
		"default protocol":          {},
		"grpc":                      {"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
		"http":                      {"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"},
		"http traces, grpc metrics": {"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"},
		"grpc traces, http metrics": {"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf"},
	}
	// equivalent is the signal URL the generic endpoint resolves to.
	equivalent := func(protocol map[string]string, signal, signalPath string) string {
		value := protocol["OTEL_EXPORTER_OTLP_"+signal+"_PROTOCOL"]
		if value == "" {
			value = protocol["OTEL_EXPORTER_OTLP_PROTOCOL"]
		}
		if value == "http/protobuf" {
			return generic + signalPath
		}
		return generic
	}
	combinations := map[string]runtimePoolTelemetryCombination{}
	for protocolName, protocol := range protocols {
		anyHTTP := slices.Contains(slices.Collect(maps.Values(protocol)), "http/protobuf")
		for _, withGeneric := range []bool{false, true} {
			for _, traces := range []string{"no", "equivalent", "different"} {
				for _, metricsEndpoint := range []string{"no", "equivalent", "different", "unreachable"} {
					if !withGeneric && traces == "no" {
						continue
					}
					values := maps.Clone(protocol)
					if withGeneric {
						values["OTEL_EXPORTER_OTLP_ENDPOINT"] = generic
					}
					switch traces {
					case "equivalent":
						values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = equivalent(protocol, "TRACES", "/v1/traces")
					case "different":
						values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "http://traces.otel.svc.cluster.local:4318/v1/traces"
					}
					switch metricsEndpoint {
					case "equivalent":
						values["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] = equivalent(protocol, "METRICS", "/v1/metrics")
					case "different":
						values["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] = "http://metrics.otel.svc.cluster.local:4318/v1/metrics"
					case "unreachable":
						values["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] = "http://localhost:4318/v1/metrics"
					}
					configured := withGeneric || metricsEndpoint != "no"
					// One shared endpoint leaves room for a protocol entry; an
					// endpoint per signal leaves room only over gRPC.
					shared := traces != "different" && metricsEndpoint != "different"
					name := fmt.Sprintf("%s, generic endpoint %t, %s trace endpoint, %s metrics endpoint", protocolName, withGeneric, traces, metricsEndpoint)
					combinations[name] = runtimePoolTelemetryCombination{
						values:  values,
						metrics: configured && metricsEndpoint != "unreachable" && (shared || !anyHTTP),
					}
				}
			}
		}
	}
	return combinations
}

// resolveOTLPSignal resolves a signal's exporter URL and protocol the way the
// supervisor and the OTel SDK read the environment. ok is false when the
// signal has no endpoint.
func resolveOTLPSignal(t *testing.T, getenv func(string) string, signal, signalPath string) (string, string, bool) {
	t.Helper()
	protocol := getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL")
	if protocol == "" {
		protocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol == "" {
		protocol = "grpc"
	}
	endpoint := getenv("OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT")
	generic := endpoint == ""
	if generic {
		endpoint = getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" {
		return "", protocol, false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("%s endpoint %q: %v", signal, endpoint, err)
	}
	if protocol == "http/protobuf" {
		if generic {
			parsed.Path = path.Join(parsed.Path, signalPath)
		} else if parsed.Path == "" {
			parsed.Path = "/"
		}
	}
	return parsed.String(), protocol, true
}

// Every endpoint and protocol combination fits the native budget, keeps each
// exported signal's SDK URL and protocol, drops metrics rather than letting
// them fall back to another collector, and keeps metrics whenever they fit.
func TestRuntimePoolTelemetryFitsNativeBudget(t *testing.T) {
	for name, combination := range runtimePoolTelemetryCombinations() {
		t.Run(name, func(t *testing.T) {
			input := func(key string) string { return combination.values[key] }
			projected := runtimePoolTelemetryEnv(true, input)
			if len(projected) > runtimePoolTelemetryEnvBudget {
				t.Fatalf("projected %d telemetry entries (budget %d): %v", len(projected), runtimePoolTelemetryEnvBudget, projected)
			}
			env := runtimePoolLiteralEnvironment(projected)
			output := func(key string) string { return env[key] }
			if env["ORKA_ENABLE_TELEMETRY"] != "true" {
				t.Fatalf("traces disabled: %v", projected)
			}
			wantURL, wantProtocol, _ := resolveOTLPSignal(t, input, "TRACES", "/v1/traces")
			if gotURL, gotProtocol, ok := resolveOTLPSignal(t, output, "TRACES", "/v1/traces"); !ok || gotURL != wantURL || gotProtocol != wantProtocol {
				t.Fatalf("traces resolve to %s %s, want %s %s: %v", gotProtocol, gotURL, wantProtocol, wantURL, projected)
			}
			gotURL, gotProtocol, ok := resolveOTLPSignal(t, output, "METRICS", "/v1/metrics")
			if ok != combination.metrics {
				t.Fatalf("metrics exported = %t, want %t: %v", ok, combination.metrics, projected)
			}
			if wantURL, wantProtocol, _ := resolveOTLPSignal(t, input, "METRICS", "/v1/metrics"); ok && (gotURL != wantURL || gotProtocol != wantProtocol) {
				t.Fatalf("metrics resolve to %s %s, want %s %s: %v", gotProtocol, gotURL, wantProtocol, wantURL, projected)
			}
		})
	}
}
