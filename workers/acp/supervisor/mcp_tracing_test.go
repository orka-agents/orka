package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestMCPProxyBrokerUsesAdmittedPromptTrace(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	privateBaggage, err := baggage.Parse("private=PRIVATE_BAGGAGE_CANARY")
	if err != nil {
		t.Fatal(err)
	}
	forged := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{9}, SpanID: trace.SpanID{9}, TraceFlags: trace.FlagsSampled,
	})
	client := newTestControllerMCPBrokerClient(t)
	session, endpoint := newTestMCPProxySession(t, client, false)
	const parent = "00-01000000000000000000000000000000-0200000000000000-01"
	const unsampled = "00-03000000000000000000000000000000-0400000000000000-00"
	for index, tc := range []struct {
		name   string
		parent string
		tracer trace.Tracer
	}{
		{"sampled", parent, provider.Tracer("test")},
		{"unsampled", unsampled, provider.Tracer("test")},
		{"missing", "", provider.Tracer("test")},
		{"malformed", "PRIVATE_INVALID_PARENT", provider.Tracer("test")},
		{"disabled sampled", parent, nil},
		{"disabled unsampled", unsampled, nil},
		{"disabled missing", "", nil},
		{"disabled malformed", "PRIVATE_INVALID_PARENT", nil},
		{"noop sampled", parent, noop.NewTracerProvider().Tracer("test")},
		{"noop unsampled", unsampled, noop.NewTracerProvider().Tracer("test")},
		{"noop missing", "", noop.NewTracerProvider().Tracer("test")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reuse one RuntimeSession across prompts, including an untraced
			// continuation after a traced prompt. No previous gate may survive.
			now := time.Now().UTC()
			authorization, lease := testMCPAuthorization(t, session.fence, now, false)
			authorization.PromptID = harnessv2.PromptID(fmt.Sprintf("prompt-%d", index))
			type privateKey struct{}
			promptCtx := trace.ContextWithSpanContext(t.Context(), forged)
			promptCtx = baggage.ContextWithBaggage(promptCtx, privateBaggage)
			promptCtx = context.WithValue(promptCtx, privateKey{}, "PRIVATE_REQUEST_CANARY")
			promptCtx, promptCancel := context.WithTimeout(promptCtx, time.Minute)
			defer promptCancel()
			promptRequest := httptest.NewRequestWithContext(promptCtx, http.MethodPost, "/prompt", nil)
			promptRequest.Header.Set("traceparent", tc.parent)
			promptRequest.Header.Set("tracestate", "vendor=prompt")
			server := &Server{cfg: Config{Tracer: tc.tracer}}
			promptRequest, span := server.traceOperation(promptRequest, harnessv2.MutationMetadata{}, "prompt")
			defer span.End()
			trusted := trace.SpanContextFromContext(promptRequest.Context())
			incoming := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(promptRequest.Header)))
			if incoming.IsValid() {
				if trusted.TraceID() != incoming.TraceID() || trusted.TraceFlags() != incoming.TraceFlags() ||
					trusted.TraceState().String() != incoming.TraceState().String() {
					t.Fatal("prompt lost its incoming trace, sampling decision, or tracestate")
				}
			} else if trusted.TraceID() == forged.TraceID() {
				t.Fatal("missing or malformed prompt header inherited an ambient parent")
			}
			if err := session.activate(promptRequest.Context(), authorization, lease, now); err != nil {
				t.Fatal(err)
			}
			defer session.deactivate(authorization.PromptID, harnessv2.RuntimeSessionStateIdle)
			if err := session.markRunning(authorization.PromptID, now); err != nil {
				t.Fatal(err)
			}
			gate, _, _, err := session.authorizeCall("lookup", now)
			if err != nil {
				t.Fatal(err)
			}
			if _, hasDeadline := gate.Deadline(); hasDeadline || gate.Value(privateKey{}) != nil ||
				baggage.FromContext(gate).Len() != 0 || trace.SpanFromContext(gate).IsRecording() {
				t.Fatal("prompt gate retained more than the span context")
			}
			// Renewal must retain the initial prompt parent and cancellable gate.
			lease.Generation++
			authorization.LeaseGeneration = lease.Generation
			if err := session.renew(authorization, lease, now); err != nil {
				t.Fatal(err)
			}
			promptCancel()
			if gate.Err() != nil {
				t.Fatal("gate lifetime was coupled to the copied prompt context")
			}
			requestCtx, cancelRequest := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancelRequest()
			requestCtx = trace.ContextWithSpanContext(requestCtx, forged)
			requestCtx = baggage.ContextWithBaggage(requestCtx, privateBaggage)
			deadline, _ := requestCtx.Deadline()
			called := false
			client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				called = true
				if !trace.SpanContextFromContext(request.Context()).Equal(trusted) {
					t.Error("provider request replaced the admitted prompt parent")
				}
				gotDeadline, _ := request.Context().Deadline()
				if !gotDeadline.Equal(deadline) {
					t.Error("trace propagation changed the tool request deadline")
				}
				transported := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(request.Header)))
				if transported.IsValid() != trusted.IsValid() || transported.TraceID() != trusted.TraceID() ||
					transported.SpanID() != trusted.SpanID() || transported.TraceFlags() != trusted.TraceFlags() ||
					transported.TraceState().String() != trusted.TraceState().String() {
					t.Error("broker transport lost the prompt trace context")
				}
				if request.Header.Get("baggage") != "" {
					t.Error("broker transport forwarded baggage")
				}
				var call harnessv2.MCPBrokerCallRequest
				if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
					return nil, err
				}
				if call.Metadata.PromptID != authorization.PromptID || call.Lease.Generation != lease.Generation {
					t.Error("trace propagation changed prompt authority")
				}
				return testMCPBrokerHTTPResponse(t, request, call.Call.CallID), nil
			})
			payload := `{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{"name":"lookup","arguments":{},"_meta":{"traceparent":"PRIVATE_META_CANARY"}}}`
			request := httptest.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(payload))
			request.RemoteAddr = "127.0.0.1:12345"
			request.Header.Set("Authorization", "Bearer credential")
			request.Header.Set("traceparent", "00-09000000000000000000000000000000-0900000000000000-01")
			request.Header.Set("tracestate", "vendor=forged")
			request.Header.Set("baggage", "private=PRIVATE_PROVIDER_CANARY")
			response := httptest.NewRecorder()
			session.proxy.serveHTTP(response, request)
			decoded := decodeMCPResponse(t, response.Result())
			if decoded.Error != nil || !called {
				t.Fatal("authorized MCP tool call did not reach the controller broker")
			}
		})
	}
}
