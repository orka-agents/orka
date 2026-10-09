package common

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	eventPOSTMaxAttempts = 6
	eventPOSTRetryBudget = 30 * time.Second
)

func (r *HTTPEventRecorder) postEvent(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, eventPOSTRetryBudget)
	defer cancel()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("record execution event: %w", err)
		}
		retryable, err := r.postEventAttempt(ctx, body)
		if err == nil || !retryable || attempt+1 == eventPOSTMaxAttempts {
			return err
		}
		delay := time.Second << min(attempt, 3)
		//nolint:gosec // Retry jitter is not security-sensitive.
		delay = time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("record execution event: %w", errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
	}
}

func (r *HTTPEventRecorder) postEventAttempt(ctx context.Context, body []byte) (bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, min(r.timeout, defaultEventRecorderTimeout))
	defer cancel()

	// GotConn precedes HTTP/1 and HTTP/2 round trips. Once a connection is
	// acquired, conservatively treat delivery as possible, even if writing
	// fails without a completed write callback. Keep this state across redirects.
	var deliveryPossible atomic.Bool
	var handshakeMu sync.Mutex
	var handshakeErr error
	trace := &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { deliveryPossible.Store(true) },
		WroteHeaderField:     func(string, []string) { deliveryPossible.Store(true) },
		WroteHeaders:         func() { deliveryPossible.Store(true) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { deliveryPossible.Store(true) },
		GotFirstResponseByte: func() { deliveryPossible.Store(true) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				handshakeMu.Lock()
				handshakeErr = err
				handshakeMu.Unlock()
			}
		},
	}
	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(requestCtx, trace), http.MethodPost, r.endpoint, bytes.NewReader(body),
	)
	if err != nil {
		return false, fmt.Errorf("create execution event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := readServiceAccountToken(r.bearerPath); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		handshakeMu.Lock()
		tlsErr := handshakeErr
		handshakeMu.Unlock()
		retryable := ctx.Err() == nil && !deliveryPossible.Load()
		if retryable {
			if tlsErr != nil {
				// A TLS validator can return dial/DNS-shaped errors of its own.
				// Never let those bypass the handshake validation safeguards.
				retryable = r.transientTLSHandshakeError(err, tlsErr)
			} else {
				retryable = preDeliveryDialError(err)
				if req.URL.Scheme == "https" {
					_, standardTLS := r.standardTLSTransport()
					retryable = retryable && standardTLS
				}
			}
		}
		return retryable, fmt.Errorf("record execution event: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	bodyPreview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false, fmt.Errorf("controller rejected execution event: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(bodyPreview)))
	}
	return false, nil
}

func preDeliveryDialError(err error) bool {
	var addressError *net.AddrError
	var networkError net.UnknownNetworkError
	var certificateError *tls.CertificateVerificationError
	var invalidCertificate x509.CertificateInvalidError
	var hostnameError x509.HostnameError
	var authorityError x509.UnknownAuthorityError
	if errors.As(err, &addressError) || errors.As(err, &networkError) ||
		errors.As(err, &certificateError) || errors.As(err, &invalidCertificate) ||
		errors.As(err, &hostnameError) || errors.As(err, &authorityError) {
		return false
	}
	// Proxy dial failures may wrap another net.OpError. CONNECT responses and
	// reads are not dial failures and must never make a POST replayable.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if op, ok := cause.(*net.OpError); ok && op.Op == "dial" {
			return true
		}
		if _, ok := cause.(*net.DNSError); ok {
			return true
		}
	}
	return false
}

func (r *HTTPEventRecorder) transientTLSHandshakeError(err, handshakeErr error) bool {
	var certificateError *tls.CertificateVerificationError
	if handshakeErr == nil || !errors.Is(err, handshakeErr) || errors.As(handshakeErr, &certificateError) {
		return false
	}
	httpTransport, ok := r.standardTLSTransport()
	if !ok {
		return false
	}
	// Custom certificate/authentication callbacks can return arbitrary errors,
	// including EOF. Without a typed validation result, fail closed on those.
	if config := httpTransport.TLSClientConfig; config != nil &&
		(config.VerifyPeerCertificate != nil || config.VerifyConnection != nil ||
			config.GetClientCertificate != nil || config.EncryptedClientHelloRejectionVerify != nil) {
		return false
	}
	if errors.Is(handshakeErr, io.EOF) || errors.Is(handshakeErr, io.ErrUnexpectedEOF) ||
		errors.Is(handshakeErr, syscall.ECONNRESET) || errors.Is(handshakeErr, syscall.EPIPE) {
		return true
	}
	var networkError net.Error
	return errors.As(handshakeErr, &networkError) && networkError.Timeout()
}

func (r *HTTPEventRecorder) standardTLSTransport() (*http.Transport, bool) {
	transport := r.client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpTransport, ok := transport.(*http.Transport)
	// Custom TLS dialers can perform validation before any handshake trace hook.
	//nolint:staticcheck // Legacy TLS dialers also have opaque validation semantics.
	if !ok || httpTransport.DialTLSContext != nil || httpTransport.DialTLS != nil {
		return nil, false
	}
	return httpTransport, true
}
