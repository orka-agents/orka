package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type gatewayTestServer struct {
	server   *httptest.Server
	token    string
	finished <-chan struct{}
	logs     *bytes.Buffer
}

func newGatewayTestServer(t *testing.T, handler http.HandlerFunc) *gatewayTestServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	g := &gateway{
		key: &key.PublicKey, credential: []byte("fixture-resource"), target: target,
		client: upstream.Client(), output: &logs,
	}
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		g.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return &gatewayTestServer{
		server: server, token: signedToken(t, key, time.Now().Add(time.Minute).Unix()),
		finished: finished, logs: &logs,
	}
}

func (s *gatewayTestServer) request(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.server.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Txn-Token", s.token)
	req.Header.Set("Authorization", "Bearer fixture-resource")
	return req
}

func awaitGatewaySignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway or upstream did not finish")
	}
}

func TestGatewayEventStreamForwardsBeforeEOF(t *testing.T) {
	const event = "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[]}}\n\n"
	for _, keepOpen := range []bool{true, false} {
		name := "closed stream"
		if keepOpen {
			name = "open stream"
		}
		t.Run(name, func(t *testing.T) {
			upstreamFinished := make(chan struct{})
			var calls atomic.Int32
			s := newGatewayTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamFinished)
				calls.Add(1)
				if r.URL.RequestURI() != "/mcp" || r.Header.Get("Txn-Token") != "" ||
					r.Header.Get("Authorization") != "Bearer fixture-resource" {
					t.Error("fixed route or credential forwarding changed")
				}
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Mcp-Session-Id", "fixture-session")
				w.Header().Set("Mcp-Protocol-Version", "2025-03-26")
				_, _ = io.WriteString(w, event)
				w.(http.Flusher).Flush()
				if keepOpen {
					<-r.Context().Done()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := s.server.Client().Do(s.request(t, ctx))
			if err != nil {
				t.Fatalf("flushed response unavailable before upstream EOF: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" ||
				resp.Header.Get("Mcp-Session-Id") != "fixture-session" || resp.Header.Get("Mcp-Protocol-Version") != "2025-03-26" {
				t.Fatal("upstream status or protocol headers changed")
			}
			got := make([]byte, len(event))
			if _, err := io.ReadFull(resp.Body, got); err != nil {
				t.Fatalf("terminal event unavailable before upstream EOF: %v", err)
			}
			if string(got) != event {
				t.Fatal("gateway changed event bytes")
			}
			if keepOpen {
				select {
				case <-upstreamFinished:
					t.Fatal("upstream must still be open when the terminal event arrives")
				default:
				}
				cancel()
			} else if tail, err := io.ReadAll(resp.Body); err != nil || len(tail) != 0 {
				t.Fatalf("closed stream did not finish cleanly: %v", err)
			}
			awaitGatewaySignal(t, upstreamFinished)
			awaitGatewaySignal(t, s.finished)
			if calls.Load() != 1 {
				t.Fatal("gateway replayed the request")
			}
			for _, secret := range []string{event, "fixture-resource", s.token, "fixture-session"} {
				if strings.Contains(s.logs.String(), secret) {
					t.Fatal("application or credential data reached the log")
				}
			}
		})
	}
}

func TestGatewayEventStreamResponseByteBound(t *testing.T) {
	const limit = 4 << 20
	for _, tc := range []struct {
		name          string
		size          int
		contentLength bool
		wantStatus    int
		wantError     bool
	}{
		{name: "exact bound", size: limit, wantStatus: http.StatusOK},
		{name: "stream overflow", size: limit + 1, wantStatus: http.StatusOK, wantError: true},
		{name: "known overflow", size: limit + 1, contentLength: true, wantStatus: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), tc.size)
			upstreamFinished := make(chan struct{})
			var calls atomic.Int32
			s := newGatewayTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamFinished)
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.contentLength {
					w.Header().Set("Content-Length", strconv.Itoa(tc.size))
				}
				w.(http.Flusher).Flush()
				_, _ = w.Write(payload)
				w.(http.Flusher).Flush()
				if tc.wantError {
					<-r.Context().Done()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := s.server.Client().Do(s.request(t, ctx))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if ctx.Err() != nil {
				t.Fatal("response limit must finish without waiting for the request deadline")
			}
			if resp.StatusCode != tc.wantStatus || (err != nil) != tc.wantError {
				t.Fatalf("status=%d read error=%v, want status=%d read error=%t", resp.StatusCode, err, tc.wantStatus, tc.wantError)
			}
			if tc.wantStatus == http.StatusOK {
				if len(body) != limit || !bytes.Equal(body, payload[:limit]) {
					t.Fatalf("stream body changed or exceeded the byte bound: received %d bytes", len(body))
				}
			} else if string(body) != "upstream response too large\n" {
				t.Fatal("pre-stream overflow error changed")
			}
			awaitGatewaySignal(t, upstreamFinished)
			awaitGatewaySignal(t, s.finished)
			if calls.Load() != 1 {
				t.Fatal("gateway replayed the request after the response limit")
			}
		})
	}
}

func TestGatewayEventStreamCancellationWhileWaiting(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	var calls atomic.Int32
	s := newGatewayTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.server.Client().Do(s.request(t, ctx))
	if err != nil {
		t.Fatalf("stream headers unavailable before body or EOF: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	cancel()
	awaitGatewaySignal(t, upstreamCanceled)
	awaitGatewaySignal(t, s.finished)
	if calls.Load() != 1 {
		t.Fatal("gateway replayed the canceled request")
	}
}

func TestGatewayPreservesBufferedJSONErrorResponse(t *testing.T) {
	const body = `{"error":"fixture upstream unavailable"}`
	s := newGatewayTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, body)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.server.Client().Do(s.request(t, ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	result, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || string(result) != body ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatal("buffered JSON response behavior changed")
	}
	awaitGatewaySignal(t, s.finished)
}
