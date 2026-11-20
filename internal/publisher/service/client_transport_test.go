package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func newTransportTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		BaseURL:          baseURL,
		BearerToken:      bytes.Repeat([]byte("t"), 32),
		CapabilitySecret: bytes.Repeat([]byte("s"), MinSecretBytes),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestTransportCauseExposesConnectionFailureWithoutUnwrapping(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = newTransportTestClient(t, "http://"+address).Capabilities(context.Background())
	if err == nil || err.Error() != "workspace publisher transport failed" {
		t.Fatalf("Capabilities error = %v, want the unchanged transport failure", err)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatal("the transport failure must stay opaque to errors.Is")
	}
	if cause := TransportCause(fmt.Errorf("read capabilities: %w", err)); !errors.Is(cause, syscall.ECONNREFUSED) {
		t.Fatalf("TransportCause = %v, want the refused connection", cause)
	}
}

func TestTransportCauseKeepsTimeoutsOpaque(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newTransportTestClient(t, server.URL).Capabilities(ctx)
	if err == nil {
		t.Fatal("Capabilities succeeded against a server that never answers")
	}
	// Callers that branch on context errors keep seeing an opaque failure.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the transport failure must not match context.DeadlineExceeded")
	}
	if cause := TransportCause(err); !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("TransportCause = %v, want the request deadline", cause)
	}
}

func TestTransportCauseIgnoresPublisherResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"code":"unauthorized","message":"bad token"}`))
	}))
	t.Cleanup(server.Close)

	_, err := newTransportTestClient(t, server.URL).Capabilities(context.Background())
	if _, ok := errors.AsType[*ClientError](err); !ok {
		t.Fatalf("Capabilities error = %v, want a ClientError", err)
	}
	for _, err := range []error{err, nil, errors.New("workspace publisher transport failed")} {
		if cause := TransportCause(err); cause != nil {
			t.Fatalf("TransportCause(%v) = %v, want nil", err, cause)
		}
	}
}
