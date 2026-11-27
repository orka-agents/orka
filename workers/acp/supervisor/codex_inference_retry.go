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

type codexInferenceResponse struct {
	*http.Response
	ctx    context.Context
	cancel context.CancelFunc
}

type codexRetryDispatch struct {
	cancel context.CancelFunc
}

func (s *providerProxySession) beginCodexRetryDispatch(gate, ctx context.Context, deadline time.Time, dispatch *codexRetryDispatch) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.admissionDone == nil || s.gateContext != gate || ctx.Err() != nil || !now.Before(deadline) || !now.Before(s.leaseExpiresAt) {
		return false
	}
	if s.pendingCodexRetries == nil {
		s.pendingCodexRetries = make(map[*codexRetryDispatch]struct{})
	}
	s.pendingCodexRetries[dispatch] = struct{}{}
	return true
}

func (s *providerProxySession) sendCodexInference(req *http.Request, gate context.Context, deadline time.Time) (*codexInferenceResponse, error) {
	ctx, cancel := context.WithCancel(req.Context())
	var timer *time.Timer
	var dispatch *codexRetryDispatch
	if gate != nil {
		dispatch = &codexRetryDispatch{cancel: cancel}
		timer = time.AfterFunc(time.Until(deadline), cancel)
		if !s.beginCodexRetryDispatch(gate, ctx, deadline, dispatch) {
			timer.Stop()
			cancel()
			return nil, nil
		}
	}
	response, err := s.proxy.client.Do(req.WithContext(ctx))
	if dispatch != nil {
		inBudget := timer.Stop() && time.Now().Before(deadline)
		s.mu.Lock()
		_, admitted := s.pendingCodexRetries[dispatch]
		delete(s.pendingCodexRetries, dispatch)
		s.mu.Unlock()
		if !admitted {
			cancel()
			if err == nil {
				_ = response.Body.Close()
			}
			return nil, nil
		}
		if !inBudget {
			cancel()
			if err != nil {
				return nil, context.DeadlineExceeded
			}
			if response.StatusCode < http.StatusBadRequest {
				_ = response.Body.Close()
				return nil, context.DeadlineExceeded
			}
		}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return &codexInferenceResponse{Response: response, ctx: ctx, cancel: cancel}, nil
}

func (s *providerProxySession) confirmedCodexRejection(req *http.Request, response *http.Response) bool {
	proof := providerproxy.Codex429Challenge(response.Header, s.proxy.codex429RouteID)
	return proof != "" && proof == req.Header.Get(providerproxy.Codex429RouteHeader)
}

func (response *codexInferenceResponse) snapshotRejection(deadline time.Time, maxResponseBytes int64) bool {
	timer := time.AfterFunc(time.Until(deadline), response.cancel)
	detail, err := io.ReadAll(io.LimitReader(response.Body, providerproxy.Codex429RejectionDetailBytes+1))
	timer.Stop()
	_ = response.Body.Close()
	response.cancel()
	complete := err == nil && len(detail) <= providerproxy.Codex429RejectionDetailBytes
	detail = detail[:min(int64(len(detail)), maxResponseBytes, providerproxy.Codex429RejectionDetailBytes)]
	response.Header.Del("Content-Length")
	response.ContentLength = int64(len(detail))
	response.Body = io.NopCloser(bytes.NewReader(detail))
	return complete
}

func (s *providerProxySession) retryRejectedCodexInference(req *http.Request, body []byte, response *codexInferenceResponse, gate context.Context, deadline time.Time) (*codexInferenceResponse, error) {
	ctx := req.Context()
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for attempt := 1; attempt < codexInferenceMaxAttempts && response.StatusCode == http.StatusTooManyRequests; attempt++ {
		if !s.confirmedCodexRejection(req, response.Response) {
			break
		}
		if providerproxy.HasDisallowedContentEncoding(response.Header) || response.ContentLength > providerproxy.Codex429RejectionDetailBytes {
			break
		}
		now := time.Now()
		due := now.Add(codexRetryDelay(response.Header, attempt, now))
		admissionDone := s.codexRetryAdmission(gate, due)
		if admissionDone == nil || !due.Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return response, nil
		case <-gate.Done():
			return response, nil
		case <-admissionDone:
			return response, nil
		case <-time.After(time.Until(due)):
		}
		if !response.snapshotRejection(deadline, s.proxy.maxResponseBytes) {
			break
		}
		next := req.Clone(ctx)
		next.Body = io.NopCloser(bytes.NewReader(body))
		next.Header.Set(providerproxy.Codex429RouteHeader, providerproxy.NewCodex429Challenge(s.proxy.codex429RouteID))
		next.Header.Set(providerproxy.Codex429RetryHeader, "1")
		next.Header.Set(providerAuthorizationHeader, providerproxy.Codex429RetryAuthScheme+" "+string(s.proxy.upstreamToken))
		retried, err := s.sendCodexInference(next, gate, deadline)
		if retried == nil && err == nil {
			break
		}
		if err != nil {
			return nil, err
		}
		req, response = next, retried
	}
	return response, nil
}

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
	if s.lastCodexRejectionSeq > 0 && s.lastFailureSeq >= max(s.lastCodexRejectionSeq, s.lastSuccessSeq) {
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
