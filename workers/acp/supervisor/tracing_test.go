package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestSupervisorTraceHeadersPreserveAuthorizationAndReplay(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	server.cfg.Tracer = provider.Tracer("test")
	ambient := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{9}, SpanID: trace.SpanID{9}, TraceFlags: trace.FlagsSampled})
	requestDone := make(chan struct{}, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.Handler().ServeHTTP(w, r.WithContext(trace.ContextWithSpanContext(r.Context(), ambient)))
		requestDone <- struct{}{}
	}))
	defer httpServer.Close()
	create := testCreateSessionRequest(t, cfg, profile)
	payload, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := harnessv2.SignOperationCapability(cfg.CapabilitySecret, harnessv2.ClaimsForMutation(create.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	parentA := "00-01000000000000000000000000000000-0100000000000000-01"
	parentB := "00-02000000000000000000000000000000-0200000000000000-01"
	send := func(body []byte, header, state, auth string, want int) {
		t.Helper()
		r, err := http.NewRequest(http.MethodPut, httpServer.URL+"/v2/runtime-sessions/session-1", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+cfg.ControllerBearerToken)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(OperationCapabilityHeader, auth)
		r.Header.Set("traceparent", header)
		r.Header.Set("tracestate", state)
		r.Header.Set("baggage", "private=PRIVATE_BAGGAGE_CANARY")
		response, err := httpServer.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		<-requestDone
		if response.StatusCode != want {
			t.Fatalf("operation HTTP status = %d, want %d", response.StatusCode, want)
		}
	}
	for i, tc := range []struct{ parent, state string }{
		{parentA, "vendor=first"},
		{parentB, "vendor=second"},
		{parentB, "invalid-PRIVATE_STATE_CANARY"},
		{"", "vendor=orphan"},
		{"invalid-PRIVATE_HEADER_CANARY", "vendor=orphan"},
	} {
		want := http.StatusOK
		if i == 0 {
			want = http.StatusCreated
		}
		send(payload, tc.parent, tc.state, capability, want)
		ended := recorder.Ended()
		s := ended[len(ended)-1]
		carrier := propagation.MapCarrier{"traceparent": tc.parent, "tracestate": tc.state}
		parent := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
		if !s.Parent().Equal(parent) {
			t.Fatal("missing/invalid/current trace context inherited a stale parent")
		}
		for _, attr := range s.Attributes() {
			if strings.Contains(attr.Value.AsString(), "PRIVATE_") {
				t.Fatal("exported private header")
			}
		}
		count := len(ended)
		send(payload, tc.parent, tc.state, "invalid-capability", http.StatusForbidden)
		if len(recorder.Ended()) != count {
			t.Fatal("unauthorized request generated an operation span")
		}
	}
	changed := create
	changed.Metadata.TaskUID = "different-task"
	changedPayload, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	send(changedPayload, parentA, "vendor=first", capability, http.StatusBadRequest)
	changed = create
	changed.Metadata.Fence.SupervisorBootID = "stale-boot"
	changed.Metadata.RequestDigest = ""
	sealRequest(t, &changed.Metadata.RequestDigest, changed)
	staleCapability, err := harnessv2.SignOperationCapability(cfg.CapabilitySecret, harnessv2.ClaimsForMutation(changed.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	changedPayload, err = json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	send(changedPayload, parentB, "vendor=second", staleCapability, http.StatusGone)
	count := len(recorder.Ended())
	server.cfg.Tracer = nil
	send(payload, parentB, "vendor=second", capability, http.StatusOK)
	if len(recorder.Ended()) != count {
		t.Fatal("disabled supervisor exported a span")
	}
}

func TestSupervisorTelemetryInvalidAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		name, enable, endpoint, protocol string
		wantErr                          bool
	}{
		{name: "disabled", endpoint: "http://127.0.0.1:1", protocol: "http/protobuf"},
		{name: "invalid flag", enable: "yes", endpoint: "http://127.0.0.1:1"},
		{name: "missing endpoint", enable: "true"},
		{name: "invalid endpoint", enable: "true", endpoint: "%invalid", wantErr: true},
		{name: "credential endpoint", enable: "true", endpoint: "https://user:PRIVATE_CANARY@example.test", wantErr: true},
		{name: "unsupported protocol", enable: "true", endpoint: "http://127.0.0.1:1", protocol: "invalid-PRIVATE_CANARY", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORKA_ENABLE_TELEMETRY", tc.enable)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", tc.endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", tc.protocol)
			tracer, shutdown, err := NewTelemetryFromEnv()
			if (err != nil) != tc.wantErr || tracer != nil {
				t.Fatal("telemetry did not fail closed to disabled")
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("configuration error exposed value")
			}
			if err := shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSupervisorTelemetryShutdownIsBounded(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-release; w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	defer close(release)
	t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
	tracer, shutdown, err := NewTelemetryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	_, span := tracer.Start(context.Background(), "acp.supervisor.test")
	span.End()
	start := time.Now()
	_ = shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("telemetry shutdown took %v", elapsed)
	}
}

func TestSupervisorExecutesWithUnavailableTelemetry(t *testing.T) {
	for _, tc := range []struct{ name, enabled, endpoint, header, sdkHeader string }{
		{name: "disabled", header: "invalid"},
		{name: "missing endpoint", enabled: "true"},
		{name: "invalid configuration", enabled: "true", endpoint: "%invalid", header: "invalid"},
		{name: "unsafe SDK configuration", enabled: "true", endpoint: "http://127.0.0.1:1", sdkHeader: "PRIVATE_CANARY%invalid"},
		{name: "unreachable collector", enabled: "true", endpoint: "http://127.0.0.1:1", header: "00-03000000000000000000000000000000-0300000000000000-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORKA_ENABLE_TELEMETRY", tc.enabled)
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", tc.sdkHeader)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", tc.endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
			tracer, shutdown, _ := NewTelemetryFromEnv()
			defer func() { _ = shutdown(context.Background()) }()
			server, cfg, profile := newTestServer(t, "immediate")
			server.cfg.Tracer = tracer
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Header.Set("traceparent", tc.header)
				server.Handler().ServeHTTP(w, r)
			}))
			defer httpServer.Close()
			client, err := harnessv2.NewClient(httpServer.URL, harnessv2.WithControllerBearerToken(cfg.ControllerBearerToken), harnessv2.WithOperationCapabilitySecret(cfg.CapabilitySecret))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			create := testCreateSessionRequest(t, cfg, profile)
			if _, err := client.CreateRuntimeSession(ctx, create); err != nil {
				t.Fatal(err)
			}
			prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			stream, err := client.StartPrompt(ctx, create.RuntimeSessionID, prompt)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close() //nolint:errcheck
			completed := false
			for {
				event, err := stream.Decode()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if event.Type == harnessv2.EventCompleted {
					completed = true
				}
			}
			if !completed {
				t.Fatal("telemetry failure prevented prompt completion")
			}
		})
	}
}
