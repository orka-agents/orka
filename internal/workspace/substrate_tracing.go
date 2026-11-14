package workspace

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"google.golang.org/grpc"
)

// substrateInstrumentationName scopes Orka's client spans for Substrate
// control and router calls.
const substrateInstrumentationName = "orka.substrate"

// substrateTracePropagator sends only W3C trace context to Substrate. Baggage
// can carry caller-supplied values and never crosses this trust boundary.
var substrateTracePropagator = propagation.TraceContext{}

// substrateGRPCTelemetryOptions returns the dial option that propagates the
// caller's trace context to the Substrate control API, so ateapi lifecycle
// spans join the caller's trace. Disabled telemetry adds nothing. Spans are
// recorded only inside an existing trace: background reconciles never start a
// root trace per Substrate call. The handler records no message events and
// never reads request metadata, so bearer credentials stay out of spans.
func substrateGRPCTelemetryOptions(enabled bool) []grpc.DialOption {
	if !enabled {
		return nil
	}
	return []grpc.DialOption{grpc.WithStatsHandler(otelgrpc.NewClientHandler(
		otelgrpc.WithPropagators(substrateTracePropagator),
		otelgrpc.WithTracerProvider(substrateParentOnlyTracerProvider{}),
	))}
}

// substrateParentOnlyTracerProvider resolves the global provider at span
// start and starts spans only under a valid parent span context.
type substrateParentOnlyTracerProvider struct{ embedded.TracerProvider }

func (substrateParentOnlyTracerProvider) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	return substrateParentOnlyTracer{name: name, options: options}
}

type substrateParentOnlyTracer struct {
	embedded.Tracer
	name    string
	options []trace.TracerOption
}

func (t substrateParentOnlyTracer) Start(ctx context.Context, name string, options ...trace.SpanStartOption) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, trace.SpanFromContext(ctx)
	}
	return otel.GetTracerProvider().Tracer(t.name, t.options...).Start(ctx, name, options...)
}

// substrateTracingTransport records one client span per router request made
// inside an existing trace and propagates W3C trace context to atenet-router
// and the Actor behind it. Requests without a trace pass through unchanged.
//
// It deliberately records only the method, the router authority and the
// response status: request paths, queries and headers (including the target
// Actor, handoff and bootstrap credentials) never enter span attributes. The
// request context, and so any httptrace hooks, reaches the base transport
// unchanged; harness v2 relies on that for written/unwritten request evidence.
type substrateTracingTransport struct {
	base http.RoundTripper
}

func newSubstrateTracingTransport(base http.RoundTripper, enabled bool) http.RoundTripper {
	if !enabled {
		return base
	}
	return &substrateTracingTransport{base: base}
}

func (t *substrateTracingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !trace.SpanContextFromContext(request.Context()).IsValid() {
		return t.base.RoundTrip(request)
	}
	tracer := otel.GetTracerProvider().Tracer(substrateInstrumentationName)
	attributes := []attribute.KeyValue{attribute.String("http.request.method", request.Method)}
	if request.URL != nil {
		if host, port, err := net.SplitHostPort(request.URL.Host); err == nil {
			attributes = append(attributes, attribute.String("server.address", host))
			if number, err := strconv.Atoi(port); err == nil {
				attributes = append(attributes, attribute.Int("server.port", number))
			}
		} else if request.URL.Host != "" {
			attributes = append(attributes, attribute.String("server.address", request.URL.Hostname()))
		}
	}
	ctx, span := tracer.Start(request.Context(), "substrate.router "+request.Method,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attributes...))
	traced := request
	if span.SpanContext().IsValid() {
		// A RoundTripper must not modify the caller's request.
		traced = request.Clone(ctx)
		// Clone copies the caller's headers; only trace context crosses.
		traced.Header.Del("Baggage")
		substrateTracePropagator.Inject(ctx, propagation.HeaderCarrier(traced.Header))
	}
	response, err := t.base.RoundTrip(traced)
	if err != nil {
		// Transport errors can embed URLs; record only a coarse class.
		span.SetAttributes(attribute.String("error.type", "transport"))
		span.SetStatus(codes.Error, "transport")
		span.End()
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
	if response.StatusCode >= http.StatusBadRequest {
		span.SetAttributes(attribute.String("error.type", strconv.Itoa(response.StatusCode)))
		span.SetStatus(codes.Error, "")
	}
	// The span measures time to response headers; long-lived router streams
	// would otherwise keep it open for the life of an ACP prompt.
	span.End()
	return response, nil
}

// DelegatesHTTPTrace reports that request contexts, and so httptrace hooks,
// reach the wrapped transport. It mirrors harness v2's delegating contract.
func (t *substrateTracingTransport) DelegatesHTTPTrace() bool {
	if t.base == nil {
		return true
	}
	if _, ok := t.base.(*http.Transport); ok {
		return true
	}
	delegating, ok := t.base.(interface{ DelegatesHTTPTrace() bool })
	return ok && delegating.DelegatesHTTPTrace()
}

// CloseIdleConnections releases pooled router connections of the base
// transport, as http.Client.CloseIdleConnections expects.
func (t *substrateTracingTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
