package workspace

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/orka-agents/orka/internal/tracing/testutil"
)

const substrateTestSecret = "substrate-secret-credential"

func TestSubstrateRouterTransportPropagatesTraceWithoutRecordingRequestDetails(t *testing.T) {
	harness := testutil.NewSpanHarness(t)
	var received http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := &http.Client{Transport: newSubstrateTracingTransport(http.DefaultTransport, true)}
	ctx, parent := otel.Tracer("test").Start(context.Background(), "task.reconcile")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v2/sessions/s-1/prompts/p-1?probe="+substrateTestSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+substrateTestSecret)
	request.Header.Set("ate-target-actor", "atespace/actor")
	request.Header.Set("Baggage", "user="+substrateTestSecret)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	parent.End()

	if request.Header.Get("traceparent") != "" {
		t.Fatal("transport mutated the caller's request headers")
	}
	extracted := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(received)))
	if !extracted.IsValid() || extracted.TraceID() != parent.SpanContext().TraceID() {
		t.Fatalf("router received traceparent %q, want the Task trace %s", received.Get("traceparent"), parent.SpanContext().TraceID())
	}
	span := testutil.SpanNamed(harness.Recorder.Ended(), "substrate.router POST")
	if span == nil {
		t.Fatal("missing substrate.router client span")
	}
	if span.SpanKind() != trace.SpanKindClient || span.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("router span kind/parent = %v/%v", span.SpanKind(), span.Parent().SpanID())
	}
	if extracted.SpanID() != span.SpanContext().SpanID() {
		t.Fatal("router traceparent does not name the client span")
	}
	attrs := testutil.AttributeMap(span)
	if attrs["http.response.status_code"].AsInt64() != http.StatusAccepted || attrs["server.address"].AsString() == "" {
		t.Fatalf("router span attributes = %v", attrs)
	}
	for key, value := range attrs {
		if strings.HasPrefix(key, "url.") || strings.HasPrefix(key, "http.request.header") ||
			strings.Contains(value.String(), substrateTestSecret) || strings.Contains(value.String(), "/v2/sessions") || strings.Contains(value.String(), "atespace/actor") {
			t.Fatalf("router span records request detail %s=%s", key, value.String())
		}
	}
	if !(&substrateTracingTransport{base: http.DefaultTransport}).DelegatesHTTPTrace() {
		t.Fatal("tracing transport must keep delegating httptrace hooks to *http.Transport")
	}
}

func TestSubstrateRouterTransportDisabledLeavesClientUnchanged(t *testing.T) {
	harness := testutil.NewSpanHarness(t)
	var received http.Header
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { received = r.Header.Clone() }))
	defer server.Close()
	// Requests outside a trace pass through: background reconciles start no
	// root trace per router call.
	client := &http.Client{Transport: newSubstrateTracingTransport(http.DefaultTransport, true)}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if received.Get("traceparent") != "" || len(harness.Recorder.Ended()) != 0 {
		t.Fatal("untraced router request started a trace")
	}
	base := http.DefaultTransport
	if got := newSubstrateTracingTransport(base, false); got != base {
		t.Fatalf("disabled telemetry wrapped the router transport: %T", got)
	}
	if options := substrateGRPCTelemetryOptions(false); len(options) != 0 {
		t.Fatalf("disabled telemetry added %d gRPC dial options", len(options))
	}
}

type substrateTestCredentials struct{}

func (substrateTestCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + substrateTestSecret}, nil
}

func (substrateTestCredentials) RequireTransportSecurity() bool { return false }

func TestSubstrateGRPCClientPropagatesTraceWithoutRecordingMetadata(t *testing.T) {
	harness := testutil.NewSpanHarness(t)
	listener := bufconn.Listen(1 << 20)
	received := make(chan metadata.MD, 1)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		received <- md
		return handler(ctx, req)
	}))
	healthpb.RegisterHealthServer(server, health.NewServer())
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	options := append([]grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(substrateTestCredentials{}),
	}, substrateGRPCTelemetryOptions(true)...)
	conn, err := grpc.NewClient("passthrough:///substrate", options...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck

	ctx, parent := otel.Tracer("test").Start(context.Background(), "task.reconcile")
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	parent.End()

	md := <-received
	traceparent := md.Get("traceparent")
	if len(traceparent) != 1 || !strings.Contains(traceparent[0], parent.SpanContext().TraceID().String()) {
		t.Fatalf("Substrate API received traceparent %v, want Task trace %s", traceparent, parent.SpanContext().TraceID())
	}
	if len(md.Get("baggage")) != 0 {
		t.Fatal("Substrate API received baggage")
	}
	var found bool
	for _, span := range harness.Recorder.Ended() {
		if span.SpanKind() != trace.SpanKindClient {
			continue
		}
		found = true
		if span.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Fatalf("gRPC client span parent = %v", span.Parent().SpanID())
		}
		for _, kv := range span.Attributes() {
			if strings.Contains(kv.Value.String(), substrateTestSecret) || strings.Contains(string(kv.Key), "metadata") {
				t.Fatalf("gRPC client span records metadata %s=%s", kv.Key, kv.Value.String())
			}
		}
		if len(span.Events()) != 0 {
			t.Fatalf("gRPC client span records %d message events", len(span.Events()))
		}
	}
	if !found {
		t.Fatal("missing gRPC client span")
	}

	// A call outside a trace records no span and sends no trace context.
	before := len(harness.Recorder.Ended())
	if _, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if md := <-received; len(md.Get("traceparent")) != 0 {
		t.Fatal("untraced Substrate call sent trace context")
	}
	if len(harness.Recorder.Ended()) != before {
		t.Fatal("untraced Substrate call started a root span")
	}
}
