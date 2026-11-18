package supervisor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type providerCancellationBarrier struct {
	cause   *promptGateCancellation
	entered chan context.Context
	release func()
}

func newProviderCancellationBarrier(t *testing.T) providerCancellationBarrier {
	t.Helper()
	entered := make(chan context.Context, 16)
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	return providerCancellationBarrier{
		cause: &promptGateCancellation{wait: func(ctx context.Context) {
			entered <- ctx
			select {
			case <-ctx.Done():
			case <-released:
			}
		}},
		entered: entered,
		release: release,
	}
}

func awaitProviderCancellationHold(t *testing.T, barrier providerCancellationBarrier) {
	t.Helper()
	select {
	case ctx := <-barrier.entered:
		if ctx.Err() != nil {
			t.Fatal("response hold received the cancelled upstream context instead of the live downstream context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local cancellation response did not enter the hold")
	}
}

type providerCancellationResponse struct {
	status int
	err    error
}

func startProviderCancellationRequest(t *testing.T, binding ProviderProxyBinding) <-chan providerCancellationResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, binding.BaseURL+"/responses", strings.NewReader(`{"model":"test-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(providerAuthorizationHeader, "Bearer "+binding.Credential)
	done := make(chan providerCancellationResponse, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		result := providerCancellationResponse{err: requestErr}
		if requestErr == nil {
			result.status = response.StatusCode
			_, result.err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		done <- result
	}()
	return done
}

func awaitProviderCancellationResponse(t *testing.T, done <-chan providerCancellationResponse) providerCancellationResponse {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("provider response did not finish")
		return providerCancellationResponse{}
	}
}

func awaitProviderCancellationSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

func TestProviderProxyCancellationHoldsPreHeaderErrorAfterImmediateRevocation(t *testing.T) {
	started, revoked := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(revoked)
	}))
	t.Cleanup(upstream.Close)
	_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken,
	})
	t.Cleanup(session.close)
	barrier := newProviderCancellationBarrier(t)
	done := startProviderCancellationRequest(t, binding)
	awaitProviderCancellationSignal(t, started, "provider request never reached upstream")
	session.deactivateWithCause(testPromptOneID, barrier.cause)
	awaitProviderCancellationSignal(t, revoked, "response hold delayed upstream revocation")
	awaitProviderCancellationHold(t, barrier)
	select {
	case <-done:
		t.Fatal("gate-induced error reached the child before cancellation settlement")
	default:
	}
	barrier.release()
	if result := awaitProviderCancellationResponse(t, done); result.err != nil || result.status != http.StatusBadGateway {
		t.Fatalf("released transport response = %+v, want HTTP 502", result)
	}
	waitProviderProxyIdle(t, session)
	if failed, _, _ := session.upstreamFailureUnrecovered(testPromptOneID); failed {
		t.Fatal("pre-header local cancellation became upstream failure evidence")
	}
}

func TestProviderProxyCancellationStreamTermination(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		chunk       string
		wantHold    bool
		wantFailure bool
	}{
		{"unfinished SSE", http.StatusOK, "data: {}\n\n", true, true},
		{"completed SSE", http.StatusOK, "event: response.completed\ndata: {}\n\n", false, false},
		{"explicit SSE failure", http.StatusOK, "event: response.failed\ndata: {}\n\n", false, true},
		{"upstream HTTP failure", http.StatusServiceUnavailable, "upstream unavailable\n", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			revoked := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.chunk)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(revoked)
			}))
			t.Cleanup(upstream.Close)
			_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
				UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken,
			})
			t.Cleanup(session.close)
			barrier := newProviderCancellationBarrier(t)
			response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
			t.Cleanup(func() { _ = response.Body.Close() })
			if _, err := io.ReadFull(response.Body, make([]byte, len(test.chunk))); err != nil {
				t.Fatalf("read delivered prefix: %v", err)
			}
			done := make(chan providerCancellationResponse, 1)
			go func() {
				_, err := io.Copy(io.Discard, response.Body)
				done <- providerCancellationResponse{status: response.StatusCode, err: err}
			}()
			session.deactivateWithCause(testPromptOneID, barrier.cause)
			awaitProviderCancellationSignal(t, revoked, "stream hold delayed upstream revocation")
			if test.wantHold {
				awaitProviderCancellationHold(t, barrier)
				select {
				case <-done:
					t.Fatal("stream termination reached the child before cancellation settlement")
				default:
				}
				barrier.release()
			}
			if result := awaitProviderCancellationResponse(t, done); result.err == nil {
				t.Fatal("revoked, unterminated HTTP stream unexpectedly ended cleanly")
			}
			select {
			case <-barrier.entered:
				t.Fatal("conclusive upstream result incorrectly entered cancellation hold")
			default:
			}
			waitProviderProxyIdle(t, session)
			if failed, _, _ := session.upstreamFailureUnrecovered(testPromptOneID); failed != test.wantFailure {
				t.Fatalf("upstream failure = %v, want %v", failed, test.wantFailure)
			}
		})
	}
}

func TestProviderProxyCancellationPreservesIndependentResults(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		contentType   string
		encoding      string
		contentLength int64
		body          string
		transportErr  error
		wantStatus    int
		wantReadError bool
	}{
		{name: "transport error", transportErr: errors.New("independent transport failure"), wantStatus: http.StatusBadGateway},
		{name: "independent closed transport", transportErr: net.ErrClosed, wantStatus: http.StatusBadGateway},
		{name: "unproven transport EOF", transportErr: io.ErrUnexpectedEOF, wantStatus: http.StatusBadGateway},
		{name: "HTTP failure", status: http.StatusBadRequest, body: `{"error":"invalid request"}`, wantStatus: http.StatusBadRequest},
		{name: "complete JSON", status: http.StatusOK, body: `{"result":"complete"}`, wantStatus: http.StatusOK},
		{name: "complete SSE", status: http.StatusOK, contentType: "text/event-stream", body: "event: response.completed\ndata: {}\n\n", wantStatus: http.StatusOK},
		{name: "SSE error", status: http.StatusOK, contentType: "text/event-stream", body: "event: response.failed\ndata: {}\n\n", wantStatus: http.StatusOK},
		{name: "unproven EOF", status: http.StatusOK, contentType: "text/event-stream", body: "data: {}\n\n", wantStatus: http.StatusOK},
		{name: "redirect", status: http.StatusTemporaryRedirect, wantStatus: http.StatusBadGateway},
		{name: "compressed response", status: http.StatusOK, encoding: "gzip", wantStatus: http.StatusBadGateway},
		{name: "declared response limit", status: http.StatusOK, contentLength: 1 << 30, wantStatus: http.StatusBadGateway},
		{name: "stream response limit", status: http.StatusOK, body: strings.Repeat("x", 1025), wantReadError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
				UpstreamBaseURL: testUnreachableUpstreamURL, UpstreamBearerToken: testUpstreamToken, MaxResponseBytes: 1024,
			})
			t.Cleanup(session.close)
			barrier := newProviderCancellationBarrier(t)
			proxy.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				// The independent outcome is already known when cancellation
				// races its delivery. A cancelled gate alone cannot relabel it.
				session.deactivateWithCause(testPromptOneID, barrier.cause)
				if test.transportErr != nil {
					return nil, test.transportErr
				}
				return &http.Response{
					StatusCode: test.status, ContentLength: test.contentLength,
					Header: http.Header{"Content-Type": []string{test.contentType}, "Content-Encoding": []string{test.encoding}},
					Body:   io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})
			result := awaitProviderCancellationResponse(t, startProviderCancellationRequest(t, binding))
			if test.wantReadError {
				if result.err == nil {
					t.Fatal("oversized response unexpectedly completed")
				}
			} else if result.err != nil || result.status != test.wantStatus {
				t.Fatalf("response = %+v, want HTTP %d", result, test.wantStatus)
			}
			select {
			case <-barrier.entered:
				t.Fatal("independent result entered cancellation hold")
			default:
			}
		})
	}
}

func TestProviderProxyCancellationInactiveRequestsStayAuthenticatedAndBounded(t *testing.T) {
	proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: testUnreachableUpstreamURL, UpstreamBearerToken: testUpstreamToken,
	})
	t.Cleanup(session.close)
	proxy.requestSlots = make(chan struct{}, cap(session.requestSlots))
	barrier := newProviderCancellationBarrier(t)
	session.deactivateWithCause(testPromptOneID, barrier.cause)
	held := make([]<-chan providerCancellationResponse, 0, cap(session.requestSlots))
	for range cap(session.requestSlots) {
		held = append(held, startProviderCancellationRequest(t, binding))
		awaitProviderCancellationHold(t, barrier)
	}
	if result := awaitProviderCancellationResponse(t, startProviderCancellationRequest(t, binding)); result.err != nil || result.status != http.StatusTooManyRequests {
		t.Fatalf("session overflow = %+v, want HTTP 429", result)
	}
	wrongCredential := binding
	wrongCredential.Credential = "incorrect-test-credential"
	if result := awaitProviderCancellationResponse(t, startProviderCancellationRequest(t, wrongCredential)); result.err != nil || result.status != http.StatusForbidden {
		t.Fatalf("unauthenticated request = %+v, want immediate HTTP 403", result)
	}
	second, secondBinding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.close)
	now := time.Now()
	if err := second.activateWithMaxTurns(testPromptOneID, 50, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	second.deactivateWithCause(testPromptOneID, barrier.cause)
	if result := awaitProviderCancellationResponse(t, startProviderCancellationRequest(t, secondBinding)); result.err != nil || result.status != http.StatusTooManyRequests {
		t.Fatalf("global overflow = %+v, want HTTP 429", result)
	}
	barrier.release()
	for _, done := range held {
		if result := awaitProviderCancellationResponse(t, done); result.err != nil || result.status != http.StatusForbidden {
			t.Fatalf("released inactive request = %+v, want HTTP 403", result)
		}
	}
	if len(proxy.requestSlots) != 0 || len(session.requestSlots) != 0 || len(second.requestSlots) != 0 {
		t.Fatal("inactive cancellation requests leaked capacity")
	}
}

func TestProviderProxyCancellationKeepsOldGateSeparateFromSuccessor(t *testing.T) {
	proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: testUnreachableUpstreamURL, UpstreamBearerToken: testUpstreamToken,
	})
	t.Cleanup(session.close)
	proxy.client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	oldBarrier, nextBarrier := newProviderCancellationBarrier(t), newProviderCancellationBarrier(t)
	session.deactivateWithCause(testPromptOneID, oldBarrier.cause)
	oldRequest := startProviderCancellationRequest(t, binding)
	awaitProviderCancellationHold(t, oldBarrier)
	now := time.Now()
	if err := session.activateWithMaxTurns("successor", 50, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	// A stale cancellation may neither revoke nor attach a hold to the successor.
	session.deactivateWithCause(testPromptOneID, oldBarrier.cause)
	if result := awaitProviderCancellationResponse(t, startProviderCancellationRequest(t, binding)); result.err != nil || result.status != http.StatusOK {
		t.Fatalf("successor request = %+v, want HTTP 200", result)
	}
	session.deactivateWithCause("successor", nextBarrier.cause)
	nextRequest := startProviderCancellationRequest(t, binding)
	awaitProviderCancellationHold(t, nextBarrier)
	oldBarrier.release()
	if result := awaitProviderCancellationResponse(t, oldRequest); result.err != nil || result.status != http.StatusForbidden {
		t.Fatalf("old rejection did not finish on its original gate: %+v", result)
	}
	select {
	case <-nextRequest:
		t.Fatal("old gate release also released the successor's rejection")
	default:
	}
	nextBarrier.release()
	if result := awaitProviderCancellationResponse(t, nextRequest); result.err != nil || result.status != http.StatusForbidden {
		t.Fatalf("successor rejection = %+v, want HTTP 403", result)
	}
}

type providerCancellationBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *providerCancellationBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, context.Canceled
}

func (b *providerCancellationBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestProviderProxyCancellationHoldsCancelledRequestBody(t *testing.T) {
	proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: testUnreachableUpstreamURL, UpstreamBearerToken: testUpstreamToken,
	})
	t.Cleanup(session.close)
	barrier := newProviderCancellationBarrier(t)
	body := &providerCancellationBody{started: make(chan struct{}), closed: make(chan struct{})}
	request := httptest.NewRequest(http.MethodPost, binding.BaseURL+"/responses", body)
	request.Header.Set(providerAuthorizationHeader, "Bearer "+binding.Credential)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		proxy.serveHTTP(response, request)
		close(done)
	}()
	awaitProviderCancellationSignal(t, body.started, "request body read never started")
	session.deactivateWithCause(testPromptOneID, barrier.cause)
	awaitProviderCancellationSignal(t, body.closed, "response hold delayed request body closure")
	awaitProviderCancellationHold(t, barrier)
	select {
	case <-done:
		t.Fatal("cancelled request body rejection escaped the hold")
	default:
	}
	barrier.release()
	awaitProviderCancellationSignal(t, done, "request body rejection did not finish after settlement")
	if response.Code != http.StatusForbidden {
		t.Fatalf("cancelled body status = %d, want HTTP 403", response.Code)
	}
}

func TestProviderProxyCancellationDownstreamDisconnectReleasesHold(t *testing.T) {
	proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: testUnreachableUpstreamURL, UpstreamBearerToken: testUpstreamToken,
	})
	t.Cleanup(session.close)
	barrier := newProviderCancellationBarrier(t)
	session.deactivateWithCause(testPromptOneID, barrier.cause)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request := httptest.NewRequest(http.MethodPost, binding.BaseURL+"/responses", strings.NewReader(`{"model":"test-model"}`)).WithContext(ctx)
	request.Header.Set(providerAuthorizationHeader, "Bearer "+binding.Credential)
	done := make(chan struct{})
	go func() {
		proxy.serveHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	awaitProviderCancellationHold(t, barrier)
	cancel()
	awaitProviderCancellationSignal(t, done, "downstream disconnect did not release the cancellation hold")
	if len(proxy.requestSlots) != 0 || len(session.requestSlots) != 0 {
		t.Fatal("downstream disconnect leaked cancellation request capacity")
	}
}
