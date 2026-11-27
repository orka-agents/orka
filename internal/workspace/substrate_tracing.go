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

const substrateInstrumentationName = "orka.substrate"

var substrateTracePropagator = propagation.TraceContext{}

func substrateGRPCTelemetryOptions(enabled bool) []grpc.DialOption {
	if !enabled {
		return nil
	}
	return []grpc.DialOption{grpc.WithStatsHandler(otelgrpc.NewClientHandler(
		otelgrpc.WithPropagators(substrateTracePropagator),
		otelgrpc.WithTracerProvider(substrateParentOnlyTracerProvider{}),
	))}
}

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
		traced = request.Clone(ctx)
		traced.Header.Del("Baggage")
		substrateTracePropagator.Inject(ctx, propagation.HeaderCarrier(traced.Header))
	}
	response, err := t.base.RoundTrip(traced)
	if err != nil {
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
	span.End()
	return response, nil
}

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

func (t *substrateTracingTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
