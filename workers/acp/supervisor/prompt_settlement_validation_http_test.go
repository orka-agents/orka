package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Blocking the ordinary HTTP writer keeps terminal processing behind real ACP
// completion. Tombstone() is the existing native completion observation seam;
// no test-only production hooks or synthetic native cancellation results are used.
type settlementValidationWriteGate struct {
	eventType harnessv2.EventType
	writeErr  error
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	open      sync.Once
}

func (g *settlementValidationWriteGate) unblock() { g.open.Do(func() { close(g.release) }) }

type settlementValidationWriter struct {
	http.ResponseWriter
	gate *settlementValidationWriteGate
}

func (w *settlementValidationWriter) Write(data []byte) (int, error) {
	var event harnessv2.Event
	if json.Unmarshal(bytes.TrimSpace(data), &event) == nil && event.Type == w.gate.eventType {
		w.gate.once.Do(func() {
			close(w.gate.entered)
			<-w.gate.release
		})
		if w.gate.writeErr != nil {
			return 0, w.gate.writeErr
		}
	}
	return w.ResponseWriter.Write(data)
}

func (w *settlementValidationWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func newPromptSettlementValidationHTTPFixture(t *testing.T, blockAt harnessv2.EventType, cancelGrace time.Duration, configure ...func(*Config)) (*assistantResultHTTPFixture, *Server, *settlementValidationWriteGate) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires the provisioned root Linux HTTP lane with the real ACP exec helper")
	}
	if _, err := os.Stat(acp.DefaultExecHelperCommand); err != nil {
		t.Fatal(err)
	}
	nonce, err := harnessv2.NewCapabilityNonce()
	if err != nil {
		t.Fatal(err)
	}
	cfg, profile := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1", nonce)
	profile.ProviderKind = providerKindOpencode
	profile.AgentConfigurationDigest, err = harnessv2.CanonicalAgentConfigurationDigest(*testServerAgentConfiguration(profile))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fence.RuntimeProfileDigest = digest
	cfg.Capabilities.RuntimeProfileDigest = digest
	cfg.Capabilities.Provider.ProviderKinds = []string{providerKindOpencode}
	cfg.Capabilities.Limits.MaxTerminalResultBytes = assistantResultTestLimit
	cfg.Capabilities.Limits.MaxBufferedEvents = 1024
	cfg.CancelGrace = cancelGrace
	cfg.Provider.Kind = providerKindOpencode
	cfg.Provider.Command = assistantResultHelperExecutable(t)
	cfg.Provider.Args = []string{"-test.run=^TestSupervisorAssistantResultACPHelper$"}
	cfg.Provider.Environment = map[string]string{assistantResultHelperEnv: "1"}
	cfg.ProviderProxy.ProviderKind = providerKindOpencode
	cfg.ProviderProxy.ModelOutputLimit = assistantResultTestLimit
	for _, customize := range configure {
		customize(&cfg)
	}
	supervisor, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := supervisor.Close(ctx); err != nil {
			t.Errorf("close supervisor: %v", err)
		}
	})
	gate := &settlementValidationWriteGate{eventType: blockAt, entered: make(chan struct{}), release: make(chan struct{})}
	handler := supervisor.Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(&settlementValidationWriter{ResponseWriter: w, gate: gate}, r)
	}))
	httpServer.Client().Timeout = 15 * time.Second
	httpServer.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(httpServer.Close)
	f := &assistantResultHTTPFixture{cfg: cfg, create: testCreateSessionRequest(t, cfg, profile), server: httpServer}
	raw := f.mutate(t, "/v2/runtime-sessions/session-1", f.create, http.StatusCreated)
	if err := json.Unmarshal(raw, &f.created); err != nil {
		t.Fatal(err)
	}
	if err := f.created.ValidateFor(f.create); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.deleteSession(t) })
	// Release a blocked writer before public cleanup even if an assertion fails.
	t.Cleanup(gate.unblock)
	return f, supervisor, gate
}

type settlementValidationHTTPResult struct {
	status int
	body   []byte
	err    error
}

func settlementValidationExchangeAsync(t *testing.T, f *assistantResultHTTPFixture, method, route string, payload any) <-chan settlementValidationHTTPResult {
	t.Helper()
	signed := mutationHTTPRequest(t, method, route, payload, f.cfg)
	request, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+route, signed.Body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = signed.Header.Clone()
	done := make(chan settlementValidationHTTPResult, 1)
	go func() {
		response, err := f.server.Client().Do(request)
		if err != nil {
			done <- settlementValidationHTTPResult{err: err}
			return
		}
		defer response.Body.Close() //nolint:errcheck
		body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		done <- settlementValidationHTTPResult{status: response.StatusCode, body: body, err: err}
	}()
	return done
}

func awaitSettlementValidationHTTP(t *testing.T, done <-chan settlementValidationHTTPResult) settlementValidationHTTPResult {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP settlement response exceeded its test bound")
		return settlementValidationHTTPResult{}
	}
}

func startBlockedSettlementValidationPrompt(t *testing.T, f *assistantResultHTTPFixture, s *Server, gate *settlementValidationWriteGate, chunks []assistantResultTestChunk, expected ...acp.PromptOutcome) (harnessv2.StartPromptRequest, <-chan settlementValidationHTTPResult) {
	t.Helper()
	encoded, err := json.Marshal(chunks)
	if err != nil {
		t.Fatal(err)
	}
	request := testStartPromptRequest(t, f.cfg, f.create.Metadata.Fence)
	request.Metadata.OperationID = "start-validation-race"
	request.Input.Content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: string(encoded)}}
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	f.lastPrompt = &request
	done := settlementValidationExchangeAsync(t, f, http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", request)
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the real HTTP stream did not reach its write barrier")
	}
	s.mu.Lock()
	native := s.sessions[f.create.RuntimeSessionID].runtime
	s.mu.Unlock()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	want := acp.PromptOutcomeCompleted
	if len(expected) != 0 {
		want = expected[0]
	}
	for {
		if tombstone, ok := native.Tombstone(string(request.Metadata.PromptID)); ok {
			if tombstone.Result.Outcome != want {
				t.Fatalf("native result = %s, want %s before supervisor validation", tombstone.Result.Outcome, want)
			}
			if want == acp.PromptOutcomeFailed && (tombstone.Result.StopReason != acp.StopReasonEndTurn || !errors.Is(tombstone.Result.Err, acp.ErrPromptEventBufferOverflow)) {
				t.Fatal("fixture did not produce real native event loss with retained end_turn")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("native completion did not precede the blocked terminal processing")
		case <-tick.C:
		}
	}
	return request, done
}

func settlementValidationCancelRequest(t *testing.T, prompt harnessv2.StartPromptRequest, operation harnessv2.OperationID, deadline time.Time) harnessv2.CancelPromptRequest {
	t.Helper()
	request := harnessv2.CancelPromptRequest{Protocol: harnessv2.ProtocolVersion, Metadata: prompt.Metadata, Reason: harnessv2.CancelReasonUserRequested, SettlementDeadline: deadline}
	request.Metadata.OperationID = operation
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	return request
}

func TestSupervisorPromptSettlementValidationNativeCompletionRaceHTTP(t *testing.T) {
	for _, test := range []struct {
		name    string
		last    assistantResultTestChunk
		outcome harnessv2.EventType
	}{
		{"identity_lost", assistantResultTestChunk{Text: "unidentified"}, harnessv2.EventFailed},
		{"invalid_tool_id", assistantResultTestChunk{RawUpdate: []byte(`{"sessionUpdate":"tool_call","toolCallId":"","title":"bad","status":"pending"}`)}, harnessv2.EventFailed},
		{"invalid_tool_status", assistantResultTestChunk{RawUpdate: []byte(`{"sessionUpdate":"tool_call","toolCallId":"tool-bad","title":"bad","status":"not-a-status"}`)}, harnessv2.EventFailed},
		{"identity_too_long", assistantResultTestChunk{MessageID: strings.Repeat("i", 513), Text: "invalid identity"}, harnessv2.EventFailed},
		{"text_overflow", assistantResultTestChunk{MessageID: "answer", Text: strings.Repeat("x", assistantResultTestLimit+1)}, harnessv2.EventFailed},
		{"serialized_overflow", assistantResultTestChunk{MessageID: "answer", Text: strings.Repeat("\"", assistantResultTestLimit/2+1)}, harnessv2.EventFailed},
		{"valid_answer", assistantResultTestChunk{MessageID: "answer", Text: "valid final answer"}, harnessv2.EventCompleted},
		{"whitespace_placeholder", assistantResultTestChunk{MessageID: "answer", Text: " \n\t "}, harnessv2.EventCompleted},
		{"reasoning_placeholder", assistantResultTestChunk{MessageID: "answer", Text: assistantResultThoughtFixture, Thought: true}, harnessv2.EventCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, supervisor, gate := newPromptSettlementValidationHTTPFixture(t, harnessv2.EventUpdate, 100*time.Millisecond)
			prompt, stream := startBlockedSettlementValidationPrompt(t, f, supervisor, gate, []assistantResultTestChunk{{MessageID: "note", Text: "earlier execution note"}, test.last})
			cancel := settlementValidationCancelRequest(t, prompt, "cancel-before-validation", time.Now().UTC().Add(5*time.Second))
			cancelRoute := "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel"
			// Native Completed is proven and the HTTP owner cannot yet validate.
			// The request must finish within its bound without freezing success.
			pending := f.mutate(t, cancelRoute, cancel, http.StatusConflict)
			var pendingError harnessv2.ErrorResponse
			if err := json.Unmarshal(pending, &pendingError); err != nil {
				t.Fatal(err)
			}
			if !pendingError.Retryable || pendingError.Code != harnessv2.ErrorCodeAlreadyAccepted {
				t.Fatalf("pending validation error = %#v", pendingError)
			}
			admissionBytes := f.mutate(t, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, http.StatusOK)
			var admission harnessv2.PromptAdmissionResponse
			if err := json.Unmarshal(admissionBytes, &admission); err != nil || admission.Settlement != nil {
				t.Fatalf("unchecked native success was frozen: settlement=%#v error=%v", admission.Settlement, err)
			}
			// Deletion must not consume that same unchecked native tombstone.
			deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: prompt.Metadata, Reason: "validation race"}
			deletion.Metadata.OperationID = "delete-before-validation"
			deletion.Metadata.RequestDigest = ""
			sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
			status, _ := f.exchange(t, t.Context(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion)
			if status != http.StatusConflict {
				t.Fatalf("deletion before validation = %d, want retryable conflict", status)
			}
			gate.unblock()
			streamed := awaitSettlementValidationHTTP(t, stream)
			if test.outcome == harnessv2.EventCompleted {
				decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(streamed.body), eventLimits(f.cfg.Capabilities.Limits), harnessv2.EventExpectationFromMetadata(prompt.Metadata))
				if err != nil {
					t.Fatal(err)
				}
				events, err := decoder.DecodeAll()
				if err != nil {
					t.Fatal(err)
				}
				wantText := "Prompt completed without textual output."
				if test.name == "valid_answer" {
					wantText = "valid final answer"
				}
				assertAssistantResultCompleted(t, assistantResultTerminal(t, events), wantText)
			}
			// The original bounded-pending response remains immutable on replay.
			replayed := f.mutate(t, cancelRoute, cancel, http.StatusConflict)
			var replayError harnessv2.ErrorResponse
			if err := json.Unmarshal(replayed, &replayError); err != nil {
				t.Fatal(err)
			}
			if replayError.Code != pendingError.Code || replayError.Message != pendingError.Message || replayError.Retryable != pendingError.Retryable {
				t.Fatal("cancellation error replay changed after terminal validation")
			}
			settledCancel := settlementValidationCancelRequest(t, prompt, "cancel-after-validation", time.Now().UTC().Add(3*time.Second))
			raw := f.mutate(t, cancelRoute, settledCancel, http.StatusOK)
			var first harnessv2.CancelPromptResponse
			if err := json.Unmarshal(raw, &first); err != nil {
				t.Fatal(err)
			}
			if first.Settlement.TerminalEvent != test.outcome {
				t.Fatalf("settlement = %s, want %s", first.Settlement.TerminalEvent, test.outcome)
			}
			var duplicate harnessv2.CancelPromptResponse
			if err := json.Unmarshal(f.mutate(t, cancelRoute, settledCancel, http.StatusOK), &duplicate); err != nil {
				t.Fatal(err)
			}
			if duplicate.Settlement != first.Settlement {
				t.Fatal("validated cancellation replay changed settlement")
			}
		})
	}
}

// This existing mutation interface is wrapped only to observe the real native
// tombstone returned by cancellation; its result and side effects are unchanged.
type settlementValidationMutationObserver struct {
	promptMutationExecutor
	completed chan struct{}
	once      sync.Once
	calls     atomic.Uint32
}

func (m *settlementValidationMutationObserver) CancelPrompt(ctx context.Context, promptID string) (acp.PromptResult, error) {
	m.calls.Add(1)
	result, err := m.promptMutationExecutor.CancelPrompt(ctx, promptID)
	if err == nil && result.Outcome == acp.PromptOutcomeCompleted {
		m.once.Do(func() { close(m.completed) })
	}
	return result, err
}

func TestSupervisorPromptSettlementValidationCancellationJoinsHTTP(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "completed"
		answer := "validated answer"
		want := harnessv2.EventCompleted
		if failed {
			name, answer, want = "oversized", strings.Repeat("x", assistantResultTestLimit+1), harnessv2.EventFailed
		}
		t.Run(name, func(t *testing.T) {
			f, supervisor, gate := newPromptSettlementValidationHTTPFixture(t, harnessv2.EventUpdate, time.Second)
			prompt, stream := startBlockedSettlementValidationPrompt(t, f, supervisor, gate, []assistantResultTestChunk{{MessageID: "note", Text: "earlier note"}, {MessageID: "answer", Text: answer}})
			supervisor.mu.Lock()
			state := supervisor.sessions[f.create.RuntimeSessionID]
			observer := &settlementValidationMutationObserver{promptMutationExecutor: state.promptMutations, completed: make(chan struct{})}
			state.promptMutations = observer
			supervisor.mu.Unlock()
			request := settlementValidationCancelRequest(t, prompt, "cancel-join-validation", time.Now().UTC().Add(3*time.Second))
			route := "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel"
			first := settlementValidationExchangeAsync(t, f, http.MethodPut, route, request)
			select {
			case <-observer.completed:
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not return the real native Completed tombstone")
			}
			second := settlementValidationExchangeAsync(t, f, http.MethodPut, route, request)
			gate.unblock()
			_ = awaitSettlementValidationHTTP(t, stream)
			var settlement *harnessv2.PromptSettlement
			for _, response := range []settlementValidationHTTPResult{awaitSettlementValidationHTTP(t, first), awaitSettlementValidationHTTP(t, second)} {
				if response.status != http.StatusOK {
					t.Fatalf("joined cancellation status = %d", response.status)
				}
				var cancellation harnessv2.CancelPromptResponse
				if err := json.Unmarshal(response.body, &cancellation); err != nil {
					t.Fatal(err)
				}
				if cancellation.Settlement.TerminalEvent != want {
					t.Fatalf("joined settlement = %s, want %s", cancellation.Settlement.TerminalEvent, want)
				}
				if settlement != nil && *settlement != cancellation.Settlement {
					t.Fatal("duplicate cancellation disagreed with the validated owner")
				}
				settlement = &cancellation.Settlement
			}
			if observer.calls.Load() != 1 {
				t.Fatalf("duplicate cancellation repeated native side effects: %d calls", observer.calls.Load())
			}
		})
	}
}

func TestSupervisorPromptSettlementValidationPublishedBeforeTerminalWriteHTTP(t *testing.T) {
	f, supervisor, gate := newPromptSettlementValidationHTTPFixture(t, harnessv2.EventCompleted, time.Second)
	prompt, stream := startBlockedSettlementValidationPrompt(t, f, supervisor, gate, []assistantResultTestChunk{{MessageID: "answer", Text: "validated answer"}})
	var admission harnessv2.PromptAdmissionResponse
	if err := json.Unmarshal(f.mutate(t, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, http.StatusOK), &admission); err != nil {
		t.Fatal(err)
	}
	if admission.Settlement == nil || admission.Settlement.TerminalEvent != harnessv2.EventCompleted {
		t.Fatal("validated settlement was not published before the blocked terminal write")
	}
	cancel := settlementValidationCancelRequest(t, prompt, "cancel-during-terminal-write", time.Now().UTC().Add(3*time.Second))
	var response harnessv2.CancelPromptResponse
	if err := json.Unmarshal(f.mutate(t, "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel", cancel, http.StatusOK), &response); err != nil {
		t.Fatal(err)
	}
	if response.Settlement != *admission.Settlement {
		t.Fatal("late cancellation changed an already-validated settlement")
	}
	gate.unblock()
	_ = awaitSettlementValidationHTTP(t, stream)
}

// Native event loss changes the outcome to Failed but retains the provider's
// end_turn. Cancellation must retain that failure, while deletion must not
// consume unchecked success before the HTTP owner finishes validation.
func TestSupervisorPromptSettlementValidationNativeBufferFailureHTTP(t *testing.T) {
	for _, viaDelete := range []bool{false, true} {
		name := "cancellation"
		if viaDelete {
			name = "deletion_guard"
		}
		t.Run(name, func(t *testing.T) {
			f, supervisor, gate := newPromptSettlementValidationHTTPFixture(t, harnessv2.EventAccepted, time.Second, func(cfg *Config) {
				cfg.Capabilities.Limits.MaxBufferedEvents = 4
			})
			chunks := make([]assistantResultTestChunk, 32)
			for i := range chunks {
				chunks[i] = assistantResultTestChunk{MessageID: "answer", Text: "native fragment "}
			}
			prompt, stream := startBlockedSettlementValidationPrompt(t, f, supervisor, gate, chunks, acp.PromptOutcomeFailed)
			if viaDelete {
				request := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: prompt.Metadata, Reason: "native event loss"}
				request.Metadata.OperationID = "delete-native-failed-before-drain"
				request.Metadata.RequestDigest = ""
				sealRequest(t, &request.Metadata.RequestDigest, request)
				status, _ := f.exchange(t, t.Context(), http.MethodDelete, "/v2/runtime-sessions/session-1", request)
				if status != http.StatusConflict {
					t.Fatalf("deletion status = %d, want pending conflict", status)
				}
			} else {
				request := settlementValidationCancelRequest(t, prompt, "cancel-native-failed", time.Now().UTC().Add(3*time.Second))
				route := "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel"
				var result, replay harnessv2.CancelPromptResponse
				if err := json.Unmarshal(f.mutate(t, route, request, http.StatusOK), &result); err != nil {
					t.Fatal(err)
				}
				if result.Settlement.TerminalEvent != harnessv2.EventFailed || result.Settlement.Outcome != harnessv2.PromptOutcomeFailed || result.Settlement.StopReason != harnessv2.ACPStopReasonRefusal {
					t.Fatalf("native Failed/end_turn became %s/%s/%s", result.Settlement.TerminalEvent, result.Settlement.Outcome, result.Settlement.StopReason)
				}
				if err := json.Unmarshal(f.mutate(t, route, request, http.StatusOK), &replay); err != nil {
					t.Fatal(err)
				}
				if replay.Settlement != result.Settlement {
					t.Fatal("native failure cancellation replay changed")
				}
			}
			status, raw := f.exchange(t, t.Context(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt)
			if status == http.StatusOK {
				var admission harnessv2.PromptAdmissionResponse
				if err := json.Unmarshal(raw, &admission); err != nil {
					t.Fatal(err)
				}
				if err := admission.Validate(); err != nil {
					t.Fatal(err)
				}
				// Deletion requests cancellation but deliberately leave settlement
				// to the stream owner, so pending is valid in that branch. An
				// explicit cancellation response above must already be Failed.
				if admission.Settlement == nil && !viaDelete {
					t.Fatal("native failure cancellation did not retain its settlement")
				}
				if admission.Settlement != nil && (admission.Settlement.Outcome != harnessv2.PromptOutcomeFailed || admission.Settlement.TerminalEvent != harnessv2.EventFailed) {
					t.Fatal("native failure produced a non-failed prompt settlement before stream drain")
				}
			} else if status != http.StatusNotFound {
				t.Fatalf("failure admission status = %d", status)
			}
			gate.unblock()
			streamed := awaitSettlementValidationHTTP(t, stream)
			limits := eventLimits(f.cfg.Capabilities.Limits)
			// This is client-side collection capacity, not the native event queue
			// deliberately constrained above to cause real event loss.
			limits.MaxBufferedEvents = 1024
			decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(streamed.body), limits, harnessv2.EventExpectationFromMetadata(prompt.Metadata))
			if err != nil {
				t.Fatal(err)
			}
			events, err := decoder.DecodeAll()
			if err != nil {
				t.Fatal(err)
			}
			terminal := assistantResultTerminal(t, events)
			if terminal.Type != harnessv2.EventFailed || terminal.Failed == nil || terminal.Failed.StopReason != harnessv2.ACPStopReasonRefusal {
				t.Fatal("stream terminal did not preserve the native event-loss failure")
			}
		})
	}
}

func TestSupervisorPromptSettlementValidationTransportFailureKeepsValidatedSuccessHTTP(t *testing.T) {
	f, supervisor, gate := newPromptSettlementValidationHTTPFixture(t, harnessv2.EventUpdate, 100*time.Millisecond)
	gate.writeErr = io.ErrClosedPipe
	prompt, stream := startBlockedSettlementValidationPrompt(t, f, supervisor, gate, []assistantResultTestChunk{
		{MessageID: "note", Text: "Execution note."}, {MessageID: "answer", Text: "Valid final answer."},
	})
	gate.unblock()
	_ = awaitSettlementValidationHTTP(t, stream)
	cancel := settlementValidationCancelRequest(t, prompt, "cancel-after-transport-error", time.Now().UTC().Add(3*time.Second))
	raw := f.mutate(t, "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel", cancel, http.StatusOK)
	var response harnessv2.CancelPromptResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.Settlement.TerminalEvent != harnessv2.EventCompleted || response.Settlement.Outcome != harnessv2.PromptOutcomeSucceeded {
		t.Fatal("a transport write failure must not replace a valid execution outcome")
	}
}
