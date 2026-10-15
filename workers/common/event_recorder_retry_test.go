package common

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orka-agents/orka/internal/events"
)

func refusedEventDial() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func retryTestRecorder(t *testing.T, transport http.RoundTripper) EventRecorder {
	t.Helper()
	return NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: "http://controller.test",
		Namespace:     "default",
		TaskName:      "task-1",
		BearerPath:    writeTestSAToken(t, "fixture"),
		Client:        &http.Client{Transport: transport},
	})
}

func TestEventPOSTBestEffortDoesNotRetryRefusedConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		recorder := retryTestRecorder(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, refusedEventDial()
		}))
		start := time.Now()
		recorder.Record(t.Context(), events.ExecutionEventTypeWorkerStarted)
		if attempts != 1 || time.Since(start) != 0 {
			t.Fatalf("attempts=%d elapsed=%v, want one attempt without backoff", attempts, time.Since(start))
		}
	})
}

func TestEventPOSTRetryExhaustsSixAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var starts []time.Time
		var bodies []string
		recorder := retryTestRecorder(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
			starts = append(starts, time.Now())
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, string(body))
			return nil, refusedEventDial()
		}))
		err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated,
			WithEventContent([]byte(`{"usage":{"id":"call/start","counterID":"call"}}`)))
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("error = %v, want connection refused after exhaustion", err)
		}
		if len(starts) != 6 {
			t.Fatalf("attempts = %d, want 6", len(starts))
		}
		bases := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
		for i, base := range bases {
			delay := starts[i+1].Sub(starts[i])
			if delay < base*4/5 || delay > base*6/5 {
				t.Fatalf("backoff %d = %v, outside ±20%% of %v", i, delay, base)
			}
			if bodies[i+1] != bodies[0] {
				t.Fatal("retry changed the event body")
			}
		}
	})
}

func TestEventPOSTRetryStopsAtOverallCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		recorder := retryTestRecorder(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			<-req.Context().Done()
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: req.Context().Err()}
		}))
		start := time.Now()
		err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want deadline exceeded", err)
		}
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Fatalf("elapsed = %v, want 30s overall cap", elapsed)
		}
		if attempts < 2 || attempts > 6 {
			t.Fatalf("attempts = %d, want 2..6 before cap", attempts)
		}
	})
}

func TestEventPOSTRetryRecoversDNSAndProxyDialFailures(t *testing.T) {
	for name, failure := range map[string]error{
		"dns":        &net.DNSError{Err: "temporary lookup failure", Name: "controller.test", IsTemporary: true},
		"proxy dial": &net.OpError{Op: "proxyconnect", Net: "tcp", Err: refusedEventDial()},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				recorder := retryTestRecorder(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						return nil, failure
					}
					return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}))
				err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
				if err != nil || attempts != 2 {
					t.Fatalf("error=%v attempts=%d, want recovered pre-delivery failure", err, attempts)
				}
			})
		})
	}
}

func TestEventPOSTRetryCapsPerRequestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
			ControllerURL: "http://controller.test", Namespace: "default", TaskName: "task-1",
			BearerPath: writeTestSAToken(t, "fixture"), Timeout: 10 * time.Second,
			Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempts++
				<-req.Context().Done()
				return nil, req.Context().Err() // No evidence this was pre-delivery.
			})},
		})
		start := time.Now()
		err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 2*time.Second || attempts != 1 {
			t.Fatalf("error=%v elapsed=%v attempts=%d, want one 2s request", err, time.Since(start), attempts)
		}
	})
}

func TestEventPOSTRetryHonorsCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		attempts := 0
		recorder := retryTestRecorder(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, refusedEventDial()
		}))
		start := time.Now()
		err := RecordEventStrict(ctx, recorder, events.ExecutionEventTypeModelUsageUpdated)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 3*time.Second {
			t.Fatalf("error=%v elapsed=%v, want caller deadline at 3s", err, time.Since(start))
		}
		if attempts > 3 {
			t.Fatalf("attempts = %d, want no attempt after caller deadline", attempts)
		}
	})
}

func TestEventPOSTRetryRejectsDialValidationErrors(t *testing.T) {
	for name, cause := range map[string]error{
		"address":     &net.AddrError{Err: "invalid address", Addr: "invalid"},
		"network":     net.UnknownNetworkError("invalid"),
		"certificate": x509.UnknownAuthorityError{},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				recorder := retryTestRecorder(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
					attempts++
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: cause}
				}))
				err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
				if !errors.Is(err, cause) || attempts != 1 {
					t.Fatalf("error=%v attempts=%d, want one terminal validation failure", err, attempts)
				}
			})
		})
	}
}

func TestEventPOSTRetryAcquiredConnectionIsNotReplayable(t *testing.T) {
	attempts := 0
	recorder := retryTestRecorder(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.GotConn != nil {
			trace.GotConn(httptrace.GotConnInfo{})
		}
		// A connection was already handed to HTTP, even without a completed
		// write callback. A subsequent dial failure cannot authorize replay.
		return nil, refusedEventDial()
	}))
	err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
	if err == nil || attempts != 1 {
		t.Fatalf("error=%v attempts=%d, want no replay after connection acquisition", err, attempts)
	}
}

func TestEventPOSTRetryDoesNotRetryHTTPResponses(t *testing.T) {
	statuses := []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
	}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
				ControllerURL: server.URL, Namespace: "default", TaskName: "task-1",
				BearerPath: writeTestSAToken(t, "fixture"),
			})
			err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
			if err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(status)) {
				t.Fatalf("error = %v, want rejected HTTP response", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("HTTP requests = %d, want exactly one", calls.Load())
			}
		})
	}
}

func TestEventPOSTRetryCancellationInterruptsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attempts := 0
		recorder := retryTestRecorder(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, refusedEventDial()
		}))
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		defer func() { <-stopped }()
		start := time.Now()
		err := RecordEventStrict(ctx, recorder, events.ExecutionEventTypeModelUsageUpdated)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want caller cancellation", err)
		}
		if elapsed := time.Since(start); elapsed != 100*time.Millisecond || attempts != 1 {
			t.Fatalf("elapsed=%v attempts=%d, want immediate cancellation before second attempt", elapsed, attempts)
		}
	})
}

type dropFirstEventTLSConnection struct {
	net.Listener
	accepts atomic.Int32
}

func (l *dropFirstEventTLSConnection) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil && l.accepts.Add(1) == 1 {
		_ = conn.Close()
	}
	return conn, err
}

func TestEventPOSTRetryRecoversTLSHandshakeFailure(t *testing.T) {
	var delivered atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	listener := &dropFirstEventTLSConnection{Listener: server.Listener}
	server.Listener = listener
	server.StartTLS()
	t.Cleanup(server.Close)
	recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: server.URL, Namespace: "default", TaskName: "task-1",
		BearerPath: writeTestSAToken(t, "fixture"), Client: server.Client(),
	})
	if err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated); err != nil {
		t.Fatalf("handshake recovery: %v", err)
	}
	if delivered.Load() != 1 || listener.accepts.Load() != 2 {
		t.Fatalf("POSTs=%d connections=%d, want one POST after handshake recovery",
			delivered.Load(), listener.accepts.Load())
	}
}

func TestEventPOSTRetryDoesNotRetryCustomTLSValidation(t *testing.T) {
	for name, test := range map[string]struct {
		failure      error
		customDialer bool
	}{
		"EOF":        {failure: io.EOF},
		"dial":       {failure: refusedEventDial()},
		"DNS":        {failure: &net.DNSError{Err: "validation lookup failed", Name: "validation.test"}},
		"TLS dialer": {failure: refusedEventDial(), customDialer: true},
		"TLS DNS": {
			failure: &net.DNSError{Err: "validation lookup failed", Name: "validation.test"}, customDialer: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("custom validation must prevent delivery")
			}))
			t.Cleanup(server.Close)
			var validations atomic.Int32
			client := server.Client()
			transport := client.Transport.(*http.Transport).Clone()
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
			transport.TLSClientConfig.VerifyConnection = func(tls.ConnectionState) error {
				validations.Add(1)
				return test.failure // Validation errors must not look like network outages.
			}
			if test.customDialer {
				transport.DialTLSContext = (&tls.Dialer{Config: transport.TLSClientConfig}).DialContext
			}
			client = &http.Client{Transport: transport}
			t.Cleanup(transport.CloseIdleConnections)
			recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
				ControllerURL: server.URL, Namespace: "default", TaskName: "task-1",
				BearerPath: writeTestSAToken(t, "fixture"), Client: client,
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := RecordEventStrict(ctx, recorder, events.ExecutionEventTypeModelUsageUpdated)
			if !errors.Is(err, test.failure) || validations.Load() != 1 {
				t.Fatalf("error=%v validations=%d, want one terminal validation failure", err, validations.Load())
			}
		})
	}
}

func TestEventPOSTRetryRejectsInvalidCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid certificate must prevent delivery")
	}))
	t.Cleanup(server.Close)
	var attempts atomic.Int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		attempts.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	t.Cleanup(transport.CloseIdleConnections)
	recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: server.URL, Namespace: "default", TaskName: "task-1",
		BearerPath: writeTestSAToken(t, "fixture"), Client: &http.Client{Transport: transport},
	})
	err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
		t.Fatalf("error = %v, want certificate verification error", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("connections = %d, want no validation-error retries", attempts.Load())
	}
}

func TestEventPOSTRetryDoesNotReplayDeliveredRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, req.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close() // Lose the response after receiving the POST.
	}))
	t.Cleanup(server.Close)
	recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: server.URL, Namespace: "default", TaskName: "task-1",
		BearerPath: writeTestSAToken(t, "fixture"),
	})
	if err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated); err == nil {
		t.Fatal("want an error for the lost response")
	}
	if calls.Load() != 1 {
		t.Fatalf("delivered POSTs = %d, want exactly one", calls.Load())
	}
}

func TestEventPOSTRetryDoesNotReplayHTTP2DeliveredRequest(t *testing.T) {
	var delivered atomic.Int32
	server := httptest.NewUnstartedServer(nil)
	server.EnableHTTP2 = true
	server.Config.Handler = http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		if req.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", req.Proto)
		}
		delivered.Add(1)
		_, _ = io.Copy(io.Discard, req.Body)
		server.CloseClientConnections()
	})
	server.StartTLS()
	t.Cleanup(server.Close)
	recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: server.URL, Namespace: "default", TaskName: "task-1",
		BearerPath: writeTestSAToken(t, "fixture"), Client: server.Client(),
	})
	err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated)
	if err == nil || delivered.Load() != 1 {
		t.Fatalf("error=%v delivered=%d, want one terminal HTTP/2 delivery", err, delivered.Load())
	}
}

func TestEventPOSTRetryDoesNotReplayRedirectedPOST(t *testing.T) {
	var delivered atomic.Int32
	attempts := 0
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if req.Method != http.MethodPost {
			return nil, refusedEventDial()
		}
		delivered.Add(1)
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"http://redirect.test/failed"}},
			Body:       io.NopCloser(strings.NewReader("")), Request: req,
		}, nil
	})
	recorder := retryTestRecorder(t, transport)
	if err := RecordEventStrict(t.Context(), recorder, events.ExecutionEventTypeModelUsageUpdated); err == nil {
		t.Fatal("want redirected dial failure")
	}
	if delivered.Load() != 1 || attempts != 2 {
		t.Fatalf("delivered POSTs=%d attempts=%d, want one POST and one failed redirect", delivered.Load(), attempts)
	}
}
