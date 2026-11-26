package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Delay only the courtesy cancel so the regression deterministically exercises
// the interval after authority revocation and before the real child sees it.
type delayedProviderPromptCancellation struct {
	promptMutationExecutor
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (m *delayedProviderPromptCancellation) CancelPrompt(ctx context.Context, promptID string) (acp.PromptResult, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
	}
	select {
	case <-m.release:
		return m.promptMutationExecutor.CancelPrompt(ctx, promptID)
	case <-ctx.Done():
		return acp.PromptResult{}, ctx.Err()
	}
}

func TestSupervisorCancellationRevokesProviderBeforeCourtesyCancel(t *testing.T) {
	started, revoked := make(chan struct{}), make(chan struct{})
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		first := upstreamCalls.Add(1) == 1
		if first {
			close(started)
		}
		select {
		case <-r.Context().Done():
		case <-t.Context().Done():
		}
		if first {
			close(revoked)
		}
	}))
	t.Cleanup(upstream.Close)
	server, cfg, profile := newTestServerWithUpstream(t, "wait", upstream.URL, testUpstreamToken)
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d", created.Code)
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	mutations := &delayedProviderPromptCancellation{
		promptMutationExecutor: state.runtime, entered: make(chan struct{}), release: make(chan struct{}),
	}
	state.promptMutations = mutations
	binding := ProviderProxyBinding{BaseURL: state.providerProxy.baseURL, Credential: string(state.providerProxy.credential)}
	server.mu.Unlock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(mutations.release) }) }
	defer release()
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	promptCtx, cancelPrompt := context.WithCancel(t.Context())
	defer cancelPrompt()
	promptDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut,
		"/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg).WithContext(promptCtx))
	awaitProviderCancellationPromptRunning(t, server, state)
	providerDone := startProviderCancellationRequest(t, binding)
	awaitSignal(t, started, "active provider request never reached upstream")
	cancellation := testLateCancellation(t, create.Metadata.Fence)
	path := "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel"
	cancelDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, path, cancellation, cfg))
	awaitSignal(t, mutations.entered, "cancellation did not reach the real runtime boundary")
	awaitSignal(t, revoked, "courtesy cancellation delayed upstream revocation")
	lateDone := startProviderCancellationRequest(t, binding)
	replayDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, path, cancellation, cfg))
	assertProviderCancellationPending(t, providerDone, "revocation error reached the child before cancellation")
	assertProviderCancellationPending(t, lateDone, "post-revocation rejection reached the child before cancellation")
	assertStillWaiting(t, replayDone, "duplicate cancellation bypassed the first request")
	if upstreamCalls.Load() != 1 || mutations.calls.Load() != 1 {
		t.Fatal("revoked provider access or cancellation replay caused another side effect")
	}
	release()
	var settled, repeated harnessv2.CancelPromptResponse
	decodeResponse(t, awaitRecorder(t, cancelDone, "cancellation did not settle"), &settled)
	decodeResponse(t, awaitRecorder(t, replayDone, "duplicate cancellation did not settle"), &repeated)
	if err := settled.Validate(); err != nil {
		t.Fatal(err)
	}
	if !settled.SettlementProven || settled.Settlement.TerminalEvent != harnessv2.EventCancelled ||
		!reflect.DeepEqual(settled.Settlement, repeated.Settlement) || mutations.calls.Load() != 1 {
		t.Fatalf("cancellation lost the child's exact settlement: %#v, replay %#v", settled, repeated)
	}
	if result := awaitProviderCancellationResponse(t, providerDone); result.err != nil || result.status != http.StatusBadGateway {
		t.Fatalf("revoked provider response = %+v", result)
	}
	if result := awaitProviderCancellationResponse(t, lateDone); result.err != nil || result.status != http.StatusForbidden {
		t.Fatalf("post-revocation provider response = %+v", result)
	}
	response := awaitRecorder(t, promptDone, "original prompt did not settle")
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(response.Body.Bytes()), eventLimits(cfg.Capabilities.Limits),
		harnessv2.EventExpectationFromMetadata(prompt.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	events, err := decoder.DecodeAll()
	if err != nil || len(events) == 0 || events[len(events)-1].Type != harnessv2.EventCancelled {
		t.Fatalf("original stream lost cancellation: events=%#v error=%v", events, err)
	}
	waitProviderProxyIdle(t, state.providerProxy)
	if upstreamCalls.Load() != 1 {
		t.Fatal("post-revocation request reached upstream")
	}
}

func TestCodexCancelReceivesEndTurnFirst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
	}{
		{"unrecovered rejection", []int{http.StatusTooManyRequests}},
		{"failure after recovery", []int{http.StatusTooManyRequests, http.StatusOK, http.StatusServiceUnavailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "/v2/runtime-sessions/session-1/prompts/prompt-1"
			const detail = "independent provider failure"
			var sends atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				index := int(sends.Add(1)) - 1
				if index >= len(tc.statuses) {
					t.Error("unexpected provider send")
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statuses[index])
				if tc.statuses[index] == http.StatusOK {
					_, _ = io.WriteString(w, `{"id":"recovered"}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, detail)
			}))
			t.Cleanup(upstream.Close)
			server, cfg, profile := newTestServerWithUpstream(t, "wait", upstream.URL, testUpstreamToken)
			create := testCreateSessionRequest(t, cfg, profile)
			if resp := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); resp.Code != http.StatusCreated {
				t.Fatalf("create HTTP=%d", resp.Code)
			}
			prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			mutator := newBlockingPromptMutator()
			mutator.cancelResult = acp.PromptResult{Outcome: acp.PromptOutcomeCompleted, StopReason: acp.StopReasonEndTurn, Accepted: true, SettledAt: time.Now().UTC()}
			close(mutator.cancelRelease)
			state.promptMutations = mutator
			binding := ProviderProxyBinding{BaseURL: state.providerProxy.baseURL, Credential: string(state.providerProxy.credential)}
			server.mu.Unlock()
			promptCtx, stopPrompt := context.WithCancel(t.Context())
			defer stopPrompt()
			original := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, path, prompt, cfg).WithContext(promptCtx))
			awaitProviderCancellationPromptRunning(t, server, state)
			for _, want := range tc.statuses {
				provider := awaitProviderCancellationResponse(t, startProviderCancellationRequest(t, binding))
				if provider.err != nil || provider.status != want {
					t.Fatalf("expected HTTP %d, got HTTP %d: %v", want, provider.status, provider.err)
				}
			}
			cancellation := testLateCancellation(t, create.Metadata.Fence)
			cancelling := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, path+"/cancel", cancellation, cfg))
			var proof harnessv2.CancelPromptResponse
			decodeResponse(t, awaitRecorder(t, cancelling, "cancel handler did not settle before the child"), &proof)
			if err := proof.Validate(); err != nil || !proof.SettlementProven || proof.Settlement.TerminalEvent != harnessv2.EventFailed {
				t.Fatalf("ACP end_turn overrode a provider failure: receipt=%s err=%v", proof.Settlement.TerminalEvent, err)
			}
			ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			_, _ = state.runtime.CancelPrompt(ctx, string(prompt.Metadata.PromptID))
			response := awaitRecorder(t, original, "original prompt did not settle")
			decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(response.Body.Bytes()), eventLimits(cfg.Capabilities.Limits),
				harnessv2.EventExpectationFromMetadata(prompt.Metadata))
			if err != nil {
				t.Fatal(err)
			}
			events, err := decoder.DecodeAll()
			if err != nil || len(events) == 0 {
				t.Fatalf("invalid event stream: %d events, err=%v", len(events), err)
			}
			event := events[len(events)-1]
			status := fmt.Sprintf("HTTP %d", tc.statuses[len(tc.statuses)-1])
			if f := event.Failed; event.Type != proof.Settlement.TerminalEvent || f == nil || f.Code != providerUpstreamErrorCode ||
				f.StopReason != harnessv2.ACPStopReasonRefusal || f.Retryable || !strings.Contains(f.Message, status) || !strings.Contains(f.Message, detail) {
				t.Fatalf("prompt did not preserve %s: receipt=%s stream=%+v", status, proof.Settlement.TerminalEvent, event.Failed)
			}
		})
	}
}

func awaitProviderCancellationPromptRunning(t *testing.T, server *Server, state *sessionState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		running := state.descriptor.State == harnessv2.RuntimeSessionStatePromptRunning
		server.mu.Unlock()
		if running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("prompt did not become active")
}

func assertProviderCancellationPending(t *testing.T, done <-chan providerCancellationResponse, message string) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("%s: %+v", message, result)
	case <-time.After(50 * time.Millisecond):
	}
}
