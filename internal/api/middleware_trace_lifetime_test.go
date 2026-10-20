/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Ended spans and retained handler contexts outlive Fiber's borrowed strings.
// Reuse one real fasthttp request buffer before exporting to make this ordering
// deterministic without relying on the server's pool or concurrent timing.
func TestNewTracingMiddlewareRequestBufferLifetime(t *testing.T) {
	for _, sampled := range []bool{true, false} {
		t.Run(fmt.Sprintf("sampled=%t", sampled), func(t *testing.T) {
			batches := make(chan []*tracepb.Span, 1)
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var request collectortrace.ExportTraceServiceRequest
				if err := proto.Unmarshal(body, &request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var spans []*tracepb.Span
				for _, rs := range request.ResourceSpans {
					for _, ss := range rs.ScopeSpans {
						spans = append(spans, ss.Spans...)
					}
				}
				batches <- spans
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			t.Cleanup(collector.Close)
			exporter, err := otlptracehttp.New(t.Context(),
				otlptracehttp.WithEndpointURL(collector.URL),
				otlptracehttp.WithHeaders(nil),
				otlptracehttp.WithCompression(otlptracehttp.NoCompression),
				otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
				otlptracehttp.WithTimeout(5*time.Second),
			)
			require.NoError(t, err)
			provider := sdktrace.NewTracerProvider(
				sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
				sdktrace.WithResource(resource.Empty()),
				sdktrace.WithBatcher(exporter,
					sdktrace.WithBatchTimeout(time.Hour),
					sdktrace.WithMaxExportBatchSize(16),
					sdktrace.WithMaxQueueSize(16),
				),
			)
			previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
			otel.SetTracerProvider(provider)
			otel.SetTextMapPropagator(propagation.TraceContext{})
			t.Cleanup(func() {
				otel.SetTracerProvider(previousProvider)
				otel.SetTextMapPropagator(previousPropagator)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				assert.NoError(t, provider.Shutdown(ctx))
			})

			var contexts []context.Context
			var headerKeys [][]string
			app := fiber.New()
			require.False(t, app.Config().Immutable, "exercise default borrowed request strings")
			app.Use(requestid.New())
			app.Use(NewTracingMiddleware())
			app.All("/probe", func(c fiber.Ctx) error {
				contexts = append(contexts, c.Context())
				headerKeys = append(headerKeys, (fiberHeaderCarrier{c: c}).Keys())
				return c.SendStatus(fiber.StatusNoContent)
			})
			handler := app.Handler()
			var requestCtx fasthttp.RequestCtx
			requestCtx.Init(&fasthttp.Request{}, nil, nil)
			var flags trace.TraceFlags
			if sampled {
				flags = trace.FlagsSampled
			}
			requests := []struct {
				method, state, requestID, header string
				traceID                          trace.TraceID
				parentID                         trace.SpanID
			}{
				{http.MethodGet, "orka=first", "request-one", "X-First", trace.TraceID{1}, trace.SpanID{2}},
				{http.MethodPut, "orka=other", "request-two", "X-Other", trace.TraceID{3}, trace.SpanID{4}},
			}
			var borrowedState, borrowedRequestID []byte
			for i, request := range requests {
				requestCtx.ResetUserValues()
				requestCtx.Request.Reset()
				requestCtx.Response.Reset()
				raw := fmt.Sprintf("%s /probe HTTP/1.1\r\nHost: example.test\r\nTraceparent: 00-%s-%s-%02x\r\nTracestate: %s\r\nX-Request-Id: %s\r\n%s: present\r\n\r\n",
					request.method, request.traceID, request.parentID, byte(flags), request.state, request.requestID, request.header)
				require.NoError(t, requestCtx.Request.Read(bufio.NewReader(strings.NewReader(raw))))
				handler(&requestCtx)
				require.Equal(t, fiber.StatusNoContent, requestCtx.Response.StatusCode())
				require.Len(t, contexts, i+1)
				require.Equal(t, request.state, trace.SpanContextFromContext(contexts[i]).TraceState().String())
				if i == 0 {
					borrowedState = requestCtx.Request.Header.Peek("Tracestate")
					borrowedRequestID = requestCtx.Request.Header.Peek("X-Request-Id")
				}
			}
			// Equal-length peer values must overwrite the same underlying storage.
			require.Equal(t, requests[1].state, string(borrowedState), "fixture did not reuse tracestate storage")
			require.Equal(t, requests[1].requestID, string(borrowedRequestID), "fixture did not reuse request ID storage")

			for i, request := range requests {
				spanContext := trace.SpanContextFromContext(contexts[i])
				assert.Equal(t, request.traceID, spanContext.TraceID(), "retained request %d trace ID", i)
				assert.Equal(t, flags, spanContext.TraceFlags(), "retained request %d sampling", i)
				assert.Equal(t, request.state, spanContext.TraceState().String(), "retained request %d tracestate", i)
				assert.Contains(t, headerKeys[i], request.header, "retained request %d carrier keys", i)
			}
			// A broker or other detached handler can create children after the HTTP
			// request ends. Its parent must retain the original request's state.
			_, child := provider.Tracer("test").Start(contexts[0], "delayed.child")
			assert.Equal(t, requests[0].traceID, child.SpanContext().TraceID())
			assert.Equal(t, flags, child.SpanContext().TraceFlags())
			assert.Equal(t, requests[0].state, child.SpanContext().TraceState().String())
			child.End()
			select {
			case <-batches:
				t.Fatal("spans exported before the controlled flush")
			default:
			}
			require.NoError(t, provider.ForceFlush(t.Context()))
			if !sampled {
				select {
				case spans := <-batches:
					t.Fatalf("unsampled requests exported %d spans", len(spans))
				default:
				}
				return
			}

			var spans []*tracepb.Span
			select {
			case spans = <-batches:
			default:
				t.Fatal("flush did not deliver sampled spans to the OTLP collector")
			}
			require.Len(t, spans, 3)
			byName := make(map[string]*tracepb.Span, len(spans))
			for _, span := range spans {
				byName[span.Name] = span
			}
			for i, request := range requests {
				span := byName[request.method+" /probe"]
				require.NotNil(t, span)
				assert.Equal(t, request.traceID[:], span.TraceId)
				assert.Equal(t, request.parentID[:], span.ParentSpanId)
				assert.Equal(t, request.state, span.TraceState, "exported request %d tracestate", i)
				attributes := make(map[string]string)
				for _, attr := range span.Attributes {
					attributes[attr.Key] = attr.Value.GetStringValue()
				}
				assert.Equal(t, request.method, attributes["http.method"])
				assert.Equal(t, request.requestID, attributes["http.request_id"], "exported request %d request ID", i)
			}
			childSpan := byName["delayed.child"]
			require.NotNil(t, childSpan)
			assert.Equal(t, requests[0].traceID[:], childSpan.TraceId)
			assert.Equal(t, byName["GET /probe"].SpanId, childSpan.ParentSpanId)
			assert.Equal(t, requests[0].state, childSpan.TraceState)
		})
	}
}
