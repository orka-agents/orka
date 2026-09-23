package controller

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// The transport checks for EOF after forwarding Content-Length bytes. Hold that
// check until the downstream response has flushed, matching the ordering that
// previously disconnected the recovery fixture's original prompt stream.
type runtimeProxyEOFBody struct {
	io.ReadCloser
	ctx       context.Context
	remaining int64
	reached   chan struct{}
	resume    <-chan struct{}
	result    chan error
	checked   bool
}

func (b *runtimeProxyEOFBody) Read(p []byte) (int, error) {
	if b.remaining > 0 {
		n, err := b.ReadCloser.Read(p)
		b.remaining -= int64(n)
		return n, err
	}
	if !b.checked {
		b.checked = true
		close(b.reached)
		select {
		case <-b.resume:
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
		n, err := b.ReadCloser.Read(p)
		b.result <- err
		return n, err
	}
	return b.ReadCloser.Read(p)
}

func TestExternalRuntimeStatusProxyPreservesOriginalPromptStream(t *testing.T) {
	assertExternalRuntimeProxyPreservesOriginalPromptStream(t, func(t *testing.T, upstream string) *httptest.Server {
		return newExternalRuntimeStatusProxy(t, upstream, func(*harnessv2.StatusResponse) {})
	})
}

func TestExternalRuntimeCapabilitiesProxyPreservesOriginalPromptStream(t *testing.T) {
	assertExternalRuntimeProxyPreservesOriginalPromptStream(t, func(t *testing.T, upstream string) *httptest.Server {
		return newExternalRuntimeCapabilitiesProxy(t, upstream, func(*harnessv2.CapabilitiesResponse) {})
	})
}

func assertExternalRuntimeProxyPreservesOriginalPromptStream(t *testing.T, newProxy func(*testing.T, string) *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resumeEOF, finish := make(chan struct{}), make(chan struct{})
	var resumeOnce, finishOnce sync.Once
	defer func() {
		resumeOnce.Do(func() { close(resumeEOF) })
		finishOnce.Do(func() { close(finish) })
	}()
	const accepted = "{\"type\":\"accepted\"}\n"
	const terminal = "{\"type\":\"cancelled\"}\n"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("consume proxied request: %v", err)
			return
		}
		w.Header().Set("Content-Type", harnessv2.NDJSONMediaType)
		if _, err := io.WriteString(w, accepted); err != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush acceptance: %v", err)
			return
		}
		select {
		case <-finish:
			_, _ = io.WriteString(w, terminal)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	proxy := newProxy(t, upstream.URL)
	reached, eofResult := make(chan struct{}), make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &runtimeProxyEOFBody{ReadCloser: r.Body, ctx: r.Context(), remaining: r.ContentLength,
			reached: reached, resume: resumeEOF, result: eofResult}
		proxy.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		server.URL+"/v2/runtime-sessions/original/prompts/original", strings.NewReader(`{"input":"fixture"}`))
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: harnessv2.NewProxylessTransport()}
	t.Cleanup(httpClient.CloseIdleConnections)
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != accepted {
		t.Fatalf("read original acceptance: line=%q err=%v", line, err)
	}
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("proxy transport did not reach its final EOF check")
	}
	resumeOnce.Do(func() { close(resumeEOF) })
	select {
	case err := <-eofResult:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("flushing acceptance closed the request body before transport EOF: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("proxy transport did not complete its final EOF check")
	}
	finishOnce.Do(func() { close(finish) })
	if line, err := reader.ReadString('\n'); err != nil || line != terminal {
		t.Fatalf("read original terminal event: line=%q err=%v", line, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("original prompt was sent %d times", calls.Load())
	}
}
