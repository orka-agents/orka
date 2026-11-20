package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/providerproxy"
)

const (
	codexInferenceMaxAttempts = 3
	codexInferenceRetryWindow = 30 * time.Second
	codexErrorWriteGrace      = time.Second
)

func (p *providerProxy) setCodex429Challenge(upstream, child *http.Request, original []byte) {
	if p.codex429RouteID != "" && child.Method == http.MethodPost && upstream.URL.Path == providerOpenAIResponsesV1Path &&
		child.URL.RawQuery == "" && !child.URL.ForceQuery && child.URL.RawPath == "" &&
		providerproxy.DirectCodexHeaders(child.Header) && providerproxy.DirectCodexBody(original, p.model) {
		upstream.Header.Set(providerproxy.Codex429RouteHeader, providerproxy.NewCodex429Challenge(p.codex429RouteID))
	}
}

// On local revocation or expiry, finish the terminal HTTP error with
// the bytes received so far. The normal relay sanitizes their bounded prefix.
type codexRetryErrorBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b codexRetryErrorBody) Read(buf []byte) (int, error) {
	n, err := b.ReadCloser.Read(buf)
	if err != nil && b.ctx.Err() != nil {
		return n, io.EOF
	}
	return n, err
}

// The caller has admitted, normalized and charged one Codex Responses POST.
// Keep its last 429 untouched until another send is authorized: on exhaustion
// the normal relay retains the status, headers and bounded, sanitized detail.
func (s *providerProxySession) retryRejectedCodexInference(req *http.Request, body []byte, response *http.Response, gate context.Context, deadline time.Time) (*http.Response, error) {
	ctx := req.Context()
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for attempt := 1; attempt < codexInferenceMaxAttempts && response.StatusCode == http.StatusTooManyRequests; attempt++ {
		proof := providerproxy.Codex429Challenge(response.Header, s.proxy.codex429RouteID)
		if proof == "" || proof != req.Header.Get(providerproxy.Codex429RouteHeader) {
			break // the first hop did not confirm the qualified route for this send
		}
		if providerproxy.HasDisallowedContentEncoding(response.Header) || response.ContentLength > s.proxy.maxResponseBytes {
			break // let the normal relay reject the invalid response
		}
		now := time.Now()
		due := now.Add(codexRetryDelay(response.Header, attempt, now))
		admissionDone := s.codexRetryAdmission(gate, due)
		if admissionDone == nil || !due.Before(deadline) {
			break
		}
		// A cancellation stops backoff; it did not cause the HTTP 429 we
		// already received. Return that independent result, not a local
		// transport error that would enter the courtesy-cancellation hold.
		select {
		case <-ctx.Done():
			return response, nil
		case <-gate.Done():
			return response, nil
		case <-admissionDone:
			return response, nil
		case <-time.After(time.Until(due)):
		}
		now = time.Now()
		if ctx.Err() != nil || !now.Before(deadline) || s.codexRetryAdmission(gate, now) == nil {
			break
		}
		_ = response.Body.Close()
		req = req.Clone(ctx)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.Header.Set(providerproxy.Codex429RouteHeader, providerproxy.NewCodex429Challenge(s.proxy.codex429RouteID))
		req.Header.Set(providerproxy.Codex429RetryHeader, "1")
		req.Header.Set(providerAuthorizationHeader, providerproxy.Codex429RetryAuthScheme+" "+string(s.proxy.upstreamToken))
		var err error
		response, err = s.proxy.client.Do(req)
		if err != nil {
			return nil, err // no status: delivery is ambiguous
		}
	}
	return response, nil
}

// An abandoned retry (including a disconnect during relay) may have no
// accountable outcome. It cannot certify recovery from the observed 429.
// A normally accounted result at this sequence or a later one wins.
func (s *providerProxySession) retainUnrecoveredCodexRejection(auth providerProxyAuthorization) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnPromptID != auth.promptID {
		return
	}
	s.lastCodexRejectionSeq = max(s.lastCodexRejectionSeq, auth.inferenceSeq)
	if max(s.lastSuccessSeq, s.lastFailureSeq) < auth.inferenceSeq {
		s.recordInferenceOutcomeLocked(auth.inferenceSeq, http.StatusTooManyRequests, "no confirmed retry result after the rate limit rejection")
	}
}

// The cancellation handler can publish proof before the original prompt
// writer. A child-reported end_turn is not proof of recovery: join admitted
// inference after revocation, then use the same issuance order as the relay.
func (s *providerProxySession) codexCancelFailure(ctx context.Context, promptID string, settlement harnessv2.PromptSettlement) error {
	if s == nil || s.proxy == nil || s.proxy.providerKind != providerKindCodex || settlement.TerminalEvent != harnessv2.EventCompleted {
		return nil
	}
	drainErr := s.waitPromptInference(ctx, promptID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnPromptID != promptID {
		return nil
	}
	if drainErr != nil && s.inflightInference != 0 {
		return &providerDrainTimeoutError{}
	}
	if s.lastSuccessSeq < s.lastCodexRejectionSeq && s.lastFailureSeq >= s.lastCodexRejectionSeq {
		return &providerUpstreamFailureError{Status: s.lastUpstreamStatus, Detail: s.lastUpstreamDetail}
	}
	return nil
}

func (s *providerProxySession) codexRetryAdmission(gate context.Context, at time.Time) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gateContext != gate || !at.Before(s.leaseExpiresAt) {
		return nil
	}
	return s.admissionDone
}

func codexRetryDelay(header http.Header, attempt int, now time.Time) time.Duration {
	var longest time.Duration = -1
	for name, unit := range map[string]time.Duration{
		"Retry-After": time.Second, "Retry-After-Ms": time.Millisecond, "X-Ms-Retry-After-Ms": time.Millisecond,
	} {
		for _, raw := range header.Values(name) {
			value := strings.TrimSpace(raw)
			if unit == time.Millisecond {
				ms, err := strconv.ParseFloat(value, 64)
				// Positive overflow parses as +Inf: don't replace a long
				// provider hint with the much shorter fallback.
				if ms >= float64(codexInferenceRetryWindow/time.Millisecond) {
					return codexInferenceRetryWindow
				}
				if err == nil && ms >= 0 {
					longest = max(longest, time.Duration(math.Ceil(ms))*time.Millisecond)
				}
				continue
			}
			if at, err := http.ParseTime(value); err == nil {
				longest = max(longest, at.Sub(now), 0)
				continue
			}
			n, err := strconv.ParseUint(value, 10, 64)
			if errors.Is(err, strconv.ErrRange) || (err == nil && n >= uint64(codexInferenceRetryWindow/unit)) {
				// A valid but unrepresentably long delay is not a missing hint.
				return codexInferenceRetryWindow
			}
			if err == nil {
				longest = max(longest, time.Duration(n)*unit)
			}
		}
	}
	if longest >= 0 {
		return longest
	}
	return time.Duration(attempt) * 250 * time.Millisecond
}
