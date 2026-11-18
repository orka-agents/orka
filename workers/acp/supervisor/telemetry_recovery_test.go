package supervisor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Exercise the SDK's asynchronous error path and reuse the same provider after
// the collector recovers. Failed exports must neither expose response content
// nor disable later exports or override a remote parent's sampling decision.
func TestSupervisorTelemetrySamplingAndRecovery(t *testing.T) {
	// The SDK's first SetErrorHandler call permanently changes previously
	// returned delegators. A subprocess keeps that state out of other tests.
	if os.Getenv("ORKA_TELEMETRY_RECOVERY_FIXTURE") != "true" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorTelemetrySamplingAndRecovery$", "-test.timeout=20s")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "OTEL_") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "ORKA_TELEMETRY_RECOVERY_FIXTURE=true")
		output, err := command.CombinedOutput()
		if bytes.Contains(output, []byte("PRIVATE_")) {
			t.Fatal("export failure logged private collector response content")
		}
		if err != nil {
			t.Fatalf("recovery subprocess failed: %s", output)
		}
		return
	}
	var healthy atomic.Bool
	received := make(chan []*tracepb.Span, 4)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request collectortrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		var spans []*tracepb.Span
		for _, rs := range request.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		received <- spans
		if !healthy.Load() {
			http.Error(w, "PRIVATE_COLLECTOR_RESPONSE_CANARY", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()

	errors := make(chan error, 4)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { errors <- err }))
	t.Setenv("ORKA_ENABLE_TELEMETRY", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
	tracer, shutdown, err := NewTelemetryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	state, err := trace.ParseTraceState("vendor=current")
	if err != nil {
		t.Fatal(err)
	}
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceState: state,
		TraceFlags: trace.FlagsSampled, Remote: true,
	})
	emit := func(name string, sampled bool) {
		ctx := trace.ContextWithRemoteSpanContext(context.Background(), parent.WithTraceFlags(0))
		if sampled {
			ctx = trace.ContextWithRemoteSpanContext(context.Background(), parent)
		}
		_, span := tracer.Start(ctx, name)
		span.End()
	}
	assertBatch := func(name string) {
		t.Helper()
		select {
		case spans := <-received:
			if len(spans) != 1 || spans[0].Name != name {
				t.Fatal("exported an unsampled span or lost the sampled operation")
			}
			tid, sid := parent.TraceID(), parent.SpanID()
			if !bytes.Equal(spans[0].TraceId, tid[:]) || !bytes.Equal(spans[0].ParentSpanId, sid[:]) || spans[0].TraceState != state.String() {
				t.Fatal("export lost the operation's W3C parent or state")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("collector received no export")
		}
	}
	emit("unsampled.before", false)
	emit("sampled.before", true)
	select {
	case err := <-errors:
		if err.Error() != "supervisor telemetry export failed" {
			t.Fatal("asynchronous export error did not use the fixed safe diagnostic")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SDK did not report the failed export")
	}
	assertBatch("sampled.before")
	healthy.Store(true)
	emit("unsampled.after", false)
	emit("sampled.after", true)
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBatch("sampled.after")
	select {
	case <-errors:
		t.Fatal("healthy collector export failed")
	default:
	}
}
