package v2

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestClientTraceContextIsTransportOnly(t *testing.T) {
	now := time.Now().UTC()
	request := clientTestCreateSessionRequest(t, now, "trace-operation")
	var firstBody []byte
	var firstCapability string
	var wantParent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if firstBody == nil {
			firstBody = body
			firstCapability = r.Header.Get(OperationCapabilityHeader)
		} else if !bytes.Equal(firstBody, body) || firstCapability != r.Header.Get(OperationCapabilityHeader) {
			t.Error("trace context changed canonical body or operation capability")
		}
		if r.Header.Get("traceparent") != wantParent {
			t.Error("HTTP request did not carry the current operation's W3C parent")
		}
		if r.Header.Get("baggage") != "" {
			t.Error("v2 transport propagated baggage")
		}
		writeClientTestJSON(w, http.StatusOK, clientTestCreateSessionResponse(request, now, RequestClassificationFresh))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithControllerBearerToken(clientTestBearer), WithOperationCapabilitySecret(clientTestCapabilitySecret))
	if err != nil {
		t.Fatal(err)
	}
	member, err := baggage.NewMember("private", "must-not-propagate")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []byte{1, 2, 0} {
		ctx := baggage.ContextWithBaggage(context.Background(), bag)
		if n != 0 {
			ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{n}, SpanID: trace.SpanID{n}, TraceFlags: trace.FlagsSampled,
			}))
		}
		carrier := propagation.MapCarrier{}
		propagation.TraceContext{}.Inject(ctx, carrier)
		wantParent = carrier.Get("traceparent")
		if _, err := client.CreateRuntimeSession(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
}
