/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package aimockcapture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newFixture(t *testing.T, upstream string) *Handler {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal("cannot parse fixture URL")
	}
	h, err := New(target)
	if err != nil {
		t.Fatal("cannot construct fixture")
	}
	return h
}

func serveFixture(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(h)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func fakeResponse(r *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{Request: r, StatusCode: status, Header: make(http.Header), Body: body, ContentLength: -1}
}

func localFixture(t *testing.T, bounds limits) (*Handler, *atomic.Int64) {
	t.Helper()
	h := newFixture(t, "http://aimock.invalid")
	h.limits = bounds
	calls := &atomic.Int64{}
	h.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return fakeResponse(r, http.StatusOK, http.NoBody), nil
	})
	return h, calls
}

func post(t *testing.T, h http.Handler, body string, unknown bool) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if unknown {
		r.ContentLength = -1
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func readCapture(t *testing.T, client *http.Client, endpoint string) Capture {
	t.Helper()
	response, err := client.Get(endpoint + capturePath)
	if err != nil {
		t.Fatal("capture endpoint failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatal("capture endpoint returned a non-OK status")
	}
	var capture Capture
	if err := json.NewDecoder(response.Body).Decode(&capture); err != nil {
		t.Fatal("capture endpoint returned invalid JSON")
	}
	return capture
}

func awaitEntry(t *testing.T, h *Handler, terminal func(Entry) bool) Entry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		capture := h.snapshot()
		if len(capture.Entries) == 1 && terminal(capture.Entries[0]) {
			return capture.Entries[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("capture did not reach the expected lifecycle state")
	return Entry{}
}

func TestExactLargeBodyAndCanary(t *testing.T) {
	const canary = "SYNTHETIC_CONTAINMENT_CANARY_not_a_secret"
	body := "{\n  \"model\":\"fixture\", \"messages\":[], \"extra\":{\"raw\":\"" +
		strings.Repeat("z", 96<<10) + "\",\"nested\":[\"" + canary + "\"]," +
		"\"unicode\":\"<>&\u2028\U0001f416\"}, \"unknown\":true\n}\n"
	var observedBody, observedID string
	upstream := serveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("upstream body read failed")
		}
		observedBody, observedID = string(data), r.Header.Get("X-Request-Id")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "fixture response")
	}))
	h := newFixture(t, upstream.URL)
	proxy := serveFixture(t, h)
	r, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages?unchanged=1", strings.NewReader(body))
	if err != nil {
		t.Fatal("cannot create request")
	}
	r.Header.Set("X-Request-Id", "caller-chosen")
	r.Header.Set("Connection", "X-Request-Id")
	r.Header.Set("Authorization", "SYNTHETIC_HEADER_not_a_secret")
	response, err := proxy.Client().Do(r)
	if err != nil {
		t.Fatal("model request failed")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	entry := awaitEntry(t, h, func(e Entry) bool { return e.Complete })
	if entry.Body != body || observedBody != body || len(entry.Body) <= 64<<10 {
		t.Fatal("large raw request body was changed or truncated")
	}
	if !strings.Contains(entry.Body, canary) {
		t.Fatal("synthetic canary was not detected in the raw body")
	}
	if entry.RequestID == "" || entry.RequestID == "caller-chosen" || observedID != entry.RequestID {
		t.Fatal("minted request ID did not reach upstream")
	}
	if entry.Path != "/v1/messages" || entry.Method != http.MethodPost ||
		entry.Status != http.StatusCreated || entry.Error != "" {
		t.Fatal("capture metadata did not match the relayed response")
	}
	capture := readCapture(t, proxy.Client(), proxy.URL)
	encoded, err := json.Marshal(capture)
	if err != nil {
		t.Fatal("cannot encode capture")
	}
	if strings.Contains(string(encoded), "SYNTHETIC_HEADER") || strings.Contains(string(encoded), "fixture response") {
		t.Fatal("capture retained a header or response body")
	}
	if len(capture.Entries) != 1 || capture.Entries[0].Body != body {
		t.Fatal("capture endpoint did not preserve the raw body")
	}
}

func TestLimitsRejectBeforeForwarding(t *testing.T) {
	cases := []struct {
		name    string
		bounds  limits
		prior   []string
		body    string
		unknown bool
		status  int
	}{
		{"body known", limits{8, 32, 8}, nil, "123456789", false, http.StatusRequestEntityTooLarge},
		{"body unknown", limits{8, 32, 8}, nil, "123456789", true, http.StatusRequestEntityTooLarge},
		{"entries", limits{8, 32, 2}, []string{"a", "b"}, "c", false, http.StatusServiceUnavailable},
		{"total known", limits{8, 12, 8}, []string{"12345678", "1234"}, "x", false, http.StatusServiceUnavailable},
		{"total unknown", limits{8, 12, 8}, []string{"12345678", "123"}, "xy", true, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := localFixture(t, tc.bounds)
			for _, body := range tc.prior {
				if post(t, h, body, false) != http.StatusOK {
					t.Fatal("request at the capacity boundary was rejected")
				}
			}
			if status := post(t, h, tc.body, tc.unknown); status != tc.status {
				t.Fatalf("rejection status = %d, want %d", status, tc.status)
			}
			capture := h.snapshot()
			if calls.Load() != int64(len(tc.prior)) || len(capture.Entries) != len(tc.prior) || capture.Rejected != 1 {
				t.Fatal("rejected request was forwarded, retained, or not counted")
			}
			for i, entry := range capture.Entries {
				if entry.Body != tc.prior[i] || !entry.Complete {
					t.Fatal("existing capture was evicted or changed on rejection")
				}
			}
		})
	}
}

func TestDefaultBounds(t *testing.T) {
	h, calls := localFixture(t, limits{MaxBodyBytes, MaxTotalBodyBytes, MaxEntries})
	if h.limits.body != 4<<20 || h.limits.total != 64<<20 || h.limits.entries != 4096 {
		t.Fatal("default capture limits changed")
	}
	if post(t, h, strings.Repeat("x", int(MaxBodyBytes)), true) != http.StatusOK {
		t.Fatal("exact 4 MiB body was rejected")
	}
	if post(t, h, strings.Repeat("x", int(MaxBodyBytes)+1), true) != http.StatusRequestEntityTooLarge {
		t.Fatal("oversized body was not rejected")
	}
	if calls.Load() != 1 {
		t.Fatal("oversized request reached upstream")
	}
}

func TestDefaultTotalBodyLimit(t *testing.T) {
	h, calls := localFixture(t, limits{MaxBodyBytes, MaxTotalBodyBytes, MaxEntries})
	body := strings.Repeat("x", int(MaxBodyBytes))
	for range MaxTotalBodyBytes / MaxBodyBytes {
		if post(t, h, body, false) != http.StatusOK {
			t.Fatal("body was rejected before the default total limit")
		}
	}
	if h.bodyBytes != MaxTotalBodyBytes || post(t, h, "x", false) != http.StatusServiceUnavailable ||
		calls.Load() != MaxTotalBodyBytes/MaxBodyBytes {
		t.Fatal("default total body limit was not enforced")
	}
	if post(t, h, "", true) != http.StatusOK {
		t.Fatal("empty body was rejected despite an available entry slot")
	}
}

func TestDefaultEntryLimit(t *testing.T) {
	h, calls := localFixture(t, limits{MaxBodyBytes, MaxTotalBodyBytes, MaxEntries})
	for range MaxEntries {
		if post(t, h, "", false) != http.StatusOK {
			t.Fatal("entry was rejected before the default limit")
		}
	}
	if post(t, h, "", false) != http.StatusServiceUnavailable || calls.Load() != MaxEntries {
		t.Fatal("default entry limit was not enforced")
	}
}

func TestConcurrentIsolationAndAdmission(t *testing.T) {
	var mu sync.Mutex
	upstreamBodies := make(map[string]string)
	upstream := serveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("upstream body read failed")
		}
		mu.Lock()
		upstreamBodies[r.Header.Get("X-Request-Id")] = string(data)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	h := newFixture(t, upstream.URL)
	h.limits = limits{8, 12, 8}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			body := strings.Repeat(string(rune('A'+i)), 3)
			_ = post(t, h, body, false)
		})
	}
	wg.Wait()
	capture := h.snapshot()
	if len(capture.Entries) != 4 || capture.Rejected != 28 || h.bodyBytes != 12 {
		t.Fatal("concurrent admission exceeded or lost the total byte budget")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(upstreamBodies) != 4 {
		t.Fatal("concurrent requests did not have distinct upstream IDs")
	}
	for _, entry := range capture.Entries {
		if upstreamBodies[entry.RequestID] != entry.Body || !entry.Complete {
			t.Fatal("request IDs did not isolate concurrent captures")
		}
	}
	other, _ := localFixture(t, limits{8, 12, 8})
	if len(other.snapshot().Entries) != 0 || other.snapshot().Rejected != 0 {
		t.Fatal("another fixture inherited capture data")
	}
	_ = post(t, other, "new", false)
	if other.snapshot().Entries[0].RequestID == capture.Entries[0].RequestID {
		t.Fatal("separate fixtures minted the same ID")
	}
}

type blockedBody struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockedBody) Read(p []byte) (int, error) {
	close(b.started)
	<-b.release
	return copy(p, "12345678"), io.EOF
}

func (*blockedBody) Close() error { return nil }

func TestPendingBodyReservation(t *testing.T) {
	h, calls := localFixture(t, limits{8, 8, 2})
	body := &blockedBody{started: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-body.started
	if post(t, h, "x", false) != http.StatusServiceUnavailable {
		t.Error("in-flight body reservation was not enforced")
	}
	if len(h.snapshot().Entries) != 0 || calls.Load() != 0 {
		t.Error("partially read request was forwarded or retained")
	}
	close(body.release)
	<-done
	if len(h.snapshot().Entries) != 1 || calls.Load() != 1 {
		t.Fatal("reserved request was not captured after its read completed")
	}
}

func TestStreamingAndCaptureDuringActiveResponse(t *testing.T) {
	const first = "event: fixture\ndata: first\n\n"
	const last = "data: [DONE]\n\n"
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	upstream := serveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "X-Fixture-Trailer")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, last)
		w.Header().Set("X-Fixture-Trailer", "done")
	}))
	h := newFixture(t, upstream.URL)
	proxy := serveFixture(t, h)
	response, err := proxy.Client().Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal("stream request failed")
	}
	defer func() { _ = response.Body.Close() }()
	data := make([]byte, len(first))
	if _, err := io.ReadFull(response.Body, data); err != nil || string(data) != first {
		t.Fatal("initial stream chunk was buffered or changed")
	}
	client := &http.Client{Timeout: time.Second}
	capture := readCapture(t, client, proxy.URL)
	if len(capture.Entries) != 1 || capture.Entries[0].Complete || capture.Entries[0].Error != "" ||
		capture.Entries[0].Status != http.StatusOK {
		t.Fatal("active stream capture was missing, blocked, or prematurely complete")
	}
	once.Do(func() { close(release) })
	data, err = io.ReadAll(response.Body)
	if err != nil || string(data) != last || response.Trailer.Get("X-Fixture-Trailer") != "done" {
		t.Fatal("stream suffix or trailer was changed")
	}
	_ = awaitEntry(t, h, func(e Entry) bool { return e.Complete })
}

func TestCanceledStreamIncomplete(t *testing.T) {
	upstreamDone := make(chan struct{})
	upstream := serveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	h := newFixture(t, upstream.URL)
	proxy := serveFixture(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal("cannot create stream request")
	}
	response, err := proxy.Client().Do(r)
	if err != nil {
		t.Fatal("stream request failed")
	}
	cancel()
	_ = response.Body.Close()
	entry := awaitEntry(t, h, func(e Entry) bool { return e.Error != "" })
	if entry.Complete || entry.Error != "request canceled" || entry.Status != http.StatusOK {
		t.Fatal("canceled stream was marked complete or had unsafe error metadata")
	}
	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reach upstream")
	}
}

type brokenBody struct{}

func (brokenBody) Read(p []byte) (int, error) {
	return copy(p, "partial"), errors.New("SYNTHETIC_PRIVATE_ERROR_not_a_secret")
}

func (brokenBody) Close() error { return nil }

func TestUpstreamStreamErrorIncomplete(t *testing.T) {
	h := newFixture(t, "http://aimock.invalid")
	h.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := fakeResponse(r, http.StatusOK, brokenBody{})
		response.Header.Set("Content-Type", "text/event-stream")
		return response, nil
	})
	proxy := serveFixture(t, h)
	response, err := proxy.Client().Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err == nil {
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("failed upstream stream was presented as successful")
	}
	entry := awaitEntry(t, h, func(e Entry) bool { return e.Error != "" })
	if entry.Complete || entry.Error != "upstream response failed" {
		t.Fatal("failed stream was marked complete or exposed an unsafe error")
	}
}

func TestTransportErrorSafe(t *testing.T) {
	h := newFixture(t, "http://aimock.invalid")
	h.proxy.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("SYNTHETIC_PRIVATE_ERROR_not_a_secret")
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	capture := h.snapshot()
	if w.Code != http.StatusBadGateway || w.Body.String() != "upstream request failed\n" ||
		capture.Entries[0].Complete || capture.Entries[0].Error != "upstream request failed" ||
		capture.Entries[0].Status != http.StatusBadGateway {
		t.Fatal("transport failure was not safely reported as incomplete")
	}
}

type failedWriter struct{ *httptest.ResponseRecorder }

func (*failedWriter) Write([]byte) (int, error) {
	return 0, errors.New("SYNTHETIC_PRIVATE_WRITE_ERROR")
}

func (*failedWriter) FlushError() error { return errors.New("SYNTHETIC_PRIVATE_FLUSH_ERROR") }

func TestDownstreamFailureIncomplete(t *testing.T) {
	for _, body := range []string{"", "response"} {
		t.Run(body, func(t *testing.T) {
			h := newFixture(t, "http://aimock.invalid")
			h.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return fakeResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(body))), nil
			})
			w := &failedWriter{httptest.NewRecorder()}
			// ServerContextKey makes ReverseProxy abort rather than suppressing copy errors in direct tests.
			ctx := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")).WithContext(ctx)
			func() {
				defer func() {
					if recovered := recover(); recovered != nil && recovered != http.ErrAbortHandler {
						t.Fatal("unexpected proxy panic")
					}
				}()
				h.ServeHTTP(w, r)
			}()
			entry := h.snapshot().Entries[0]
			if entry.Complete || entry.Error != "downstream response failed" {
				t.Fatal("downstream write or flush failure was marked complete")
			}
		})
	}
}

func TestBodyReadFailureReleasesReservation(t *testing.T) {
	h, calls := localFixture(t, limits{8, 8, 1})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", brokenBody{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || len(h.snapshot().Entries) != 0 ||
		h.snapshot().Rejected != 1 || calls.Load() != 0 {
		t.Fatal("failed body read was forwarded or not counted")
	}
	if post(t, h, "12345678", false) != http.StatusOK {
		t.Fatal("failed body read leaked its reservation")
	}
}

func TestControlAndOtherEndpointsProxyUncaptured(t *testing.T) {
	h := newFixture(t, "http://aimock.invalid")
	h.limits = limits{1, 1, 1}
	h.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Request-Id") != "caller-id" || r.Header.Get("X-Fixture") != "unchanged" ||
			r.URL.RawQuery != "raw=a%2Fb" {
			t.Error("uncaptured request headers or query changed")
		}
		return fakeResponse(r, http.StatusAccepted, r.Body), nil
	})
	for _, path := range []string{"/__aimock", "/__aimock/journal", "/__aimock/fixtures", "/__orka_eval/other"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			r := httptest.NewRequest(method, path+"?raw=a%2Fb", strings.NewReader("larger than capture limits"))
			r.Header.Set("X-Request-Id", "caller-id")
			r.Header.Set("X-Fixture", "unchanged")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusAccepted || w.Body.String() != "larger than capture limits" {
				t.Fatal("uncaptured endpoint was not transparently proxied")
			}
		}
	}
	if len(h.snapshot().Entries) != 0 || h.snapshot().Rejected != 0 {
		t.Fatal("control endpoints consumed capture capacity")
	}
}

func TestControlNamespaceBoundaries(t *testing.T) {
	h, calls := localFixture(t, limits{8, 16, 2})
	for _, path := range []string{"/__aimockish", "/__orka_evalish"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
	}
	if len(h.snapshot().Entries) != 2 || calls.Load() != 2 {
		t.Fatal("namespace lookalikes escaped capture")
	}
}

func TestCaptureEmptyAndMethodRestriction(t *testing.T) {
	h := newFixture(t, "http://aimock.invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, capturePath, nil))
	var capture Capture
	if err := json.Unmarshal(w.Body.Bytes(), &capture); err != nil || capture.InstanceID == "" ||
		capture.Entries == nil || len(capture.Entries) != 0 || capture.Rejected != 0 ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("empty capture response has an unexpected wire shape or caching policy")
	}
	for _, path := range []string{capturePath, healthPath} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
			t.Fatal("control endpoint accepted a non-GET method")
		}
	}
}

func TestReadinessChecksUpstream(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			upstream := serveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/__aimock/health" || r.URL.RawQuery != "" || r.Method != http.MethodGet {
					t.Error("readiness did not probe the upstream health endpoint")
				}
				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "SYNTHETIC_PRIVATE_HEALTH_BODY")
			}))
			h := newFixture(t, upstream.URL+"/base?ignored=1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, healthPath, nil))
			want := http.StatusServiceUnavailable
			if status == http.StatusOK {
				want = http.StatusOK
			}
			if w.Code != want || calls.Load() != 1 || strings.Contains(w.Body.String(), "SYNTHETIC_PRIVATE") {
				t.Fatal("readiness accepted an unhealthy upstream, followed a redirect, or exposed a response body")
			}
		})
	}
}

func TestReadinessTimeoutAndTransportFailure(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "timeout"}[slow], func(t *testing.T) {
			h := newFixture(t, "http://aimock.invalid")
			if h.healthClient.Timeout != 2*time.Second {
				t.Fatal("readiness probe lacks the fixed bounded timeout")
			}
			h.healthClient.Timeout = 20 * time.Millisecond
			h.healthClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if slow {
					<-r.Context().Done()
				}
				return nil, errors.New("SYNTHETIC_PRIVATE_HEALTH_ERROR")
			})
			w := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, healthPath, nil))
			if w.Code != http.StatusServiceUnavailable || w.Body.String() != "upstream not ready\n" ||
				time.Since(start) > time.Second {
				t.Fatal("readiness failure was not safely bounded")
			}
		})
	}
}

func TestInvalidUpstream(t *testing.T) {
	for _, target := range []*url.URL{nil, {}, {Scheme: "file", Host: "fixture"},
		{Scheme: "http", Host: "fixture", User: url.User("fixture")}} {
		if _, err := New(target); err == nil || err.Error() != "invalid upstream URL" {
			t.Fatal("invalid upstream was accepted or produced an unsafe error")
		}
	}
}

func TestInstanceIDStableAndRestartDistinct(t *testing.T) {
	h, _ := localFixture(t, limits{8, 8, 1})
	proxy := serveFixture(t, h)
	initial := readCapture(t, proxy.Client(), proxy.URL)
	if initial.InstanceID == "" || len(initial.Entries) != 0 {
		t.Fatal("empty baseline snapshot lacks an instance ID")
	}
	if post(t, h, "request", false) != http.StatusOK || post(t, h, "reject", false) != http.StatusServiceUnavailable {
		t.Fatal("cannot populate and reject against fixture snapshot")
	}
	final := readCapture(t, proxy.Client(), proxy.URL)
	if final.InstanceID != initial.InstanceID || len(final.Entries) != 1 || final.Rejected != 1 {
		t.Fatal("handler instance ID changed across captures or rejections")
	}
	priorID := final.Entries[0].RequestID
	retained := readCapture(t, proxy.Client(), proxy.URL)
	if retained.InstanceID != initial.InstanceID || retained.Entries[0].RequestID != priorID {
		t.Fatal("later snapshot lost instance identity or an existing request ID")
	}
	restarted := newFixture(t, "http://aimock.invalid")
	fresh := serveFixture(t, restarted)
	baseline := readCapture(t, fresh.Client(), fresh.URL)
	if baseline.InstanceID == "" || baseline.InstanceID == initial.InstanceID || len(baseline.Entries) != 0 {
		t.Fatal("new handler did not expose a distinct nonempty instance ID")
	}
}

func TestNonUTF8BodyRejectedWithoutLossyCapture(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "unknown"}[unknown], func(t *testing.T) {
			h, calls := localFixture(t, limits{8, 8, 1})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("\xff"))
			if unknown {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || w.Body.String() != "capture body encoding invalid\n" ||
				calls.Load() != 0 || len(h.snapshot().Entries) != 0 || h.snapshot().Rejected != 1 {
				t.Fatal("unrepresentable raw body was forwarded or silently changed in capture")
			}
			if post(t, h, "valid", false) != http.StatusOK {
				t.Fatal("rejected body leaked its capacity reservation")
			}
		})
	}
}
