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
	"go.opentelemetry.io/otel/trace"
)

func TestClientTraceContextIsTransportOnly(t *testing.T) {
	now := time.Now().UTC()
	request := clientTestCreateSessionRequest(t, now, "trace-operation")
	var firstBody []byte
	var firstCapability string
	var wantParent string
	var wantState string
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
		if r.Header.Get("tracestate") != wantState {
			t.Error("HTTP request did not carry the current operation's W3C state")
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
	for _, tc := range []struct {
		id      byte
		parent  string
		state   string
		sampled bool
	}{
		{1, "00-01000000000000000000000000000000-0100000000000000-01", "vendor=first", true},
		{2, "00-02000000000000000000000000000000-0200000000000000-01", "vendor=second", true},
		{3, "00-03000000000000000000000000000000-0300000000000000-00", "vendor=unsampled", false},
		{},
	} {
		ctx := baggage.ContextWithBaggage(context.Background(), bag)
		if tc.id != 0 {
			state, err := trace.ParseTraceState(tc.state)
			if err != nil {
				t.Fatal(err)
			}
			var flags trace.TraceFlags
			if tc.sampled {
				flags = trace.FlagsSampled
			}
			ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{tc.id}, SpanID: trace.SpanID{tc.id}, TraceFlags: flags, TraceState: state,
			}))
		}
		wantParent, wantState = tc.parent, tc.state
		if _, err := client.CreateRuntimeSession(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
}
